package ha

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// 备用侧的失联判定(设计 4.4–4.6、5.2–5.3),每 10 秒一次。
//
// 什么情况下算"主机失联":本机已经 60 秒以上没收到主机的请求,而且
//   - 其它副机也都联系不上它(谁还联系得上,就是本机和主机之间的网络问题,不接任);
//   - 境外测点(Globalping,不用大陆测点)也连不上主机的公网 IP(连得上就是网络分区,不接任);
//   - 主机没有在暂停接任(升级、计划内重启前自动暂停),Telegram 也没显示主机的机器人还在跑;
//   - 时间到了:确认后接任默认 3 分钟、自动接任默认 10 分钟;备用 2 再多等 3 分钟,而且要确认备用 1 也不在。
//
// 确认后接任:发一条 Telegram,回复 /takeover <码>(或「接任 <码>」)或在本机面板点「接任为主机」才动手;
// 自动接任:三台以上的集群要过半副机投票同意(而且境外测点没说主机还活着),只有一主一副时必须境外测点也说主机不通。

var (
	evidenceEvery  = 30 * time.Second
	witnessEvery   = 5 * time.Minute
	peerCheckEvery = 5 * time.Minute // 主机正常时多久问一圈同伴(就绪检查的"副机互通")
)

type engine struct {
	s  *Service
	mu sync.Mutex

	suspectAt  time.Time
	evidenceAt time.Time
	peers      map[uint]peerObs
	peerSum    *hub.HAPeers
	peerSumAt  time.Time
	witness    *WitnessResult
	witnessAt  time.Time
	tgConflict time.Time // getUpdates 最近一次 409:另有实例在长轮询(多半是主机的机器人还活着)
	prompt     *prompt
	alerted    map[string]bool
	reason     string // 最近一次不接任的原因(面板显示)
	running    bool
}

type prompt struct {
	code   string
	epoch  uint64
	sentAt time.Time
	offset int64
	stop   chan struct{}
}

func newEngine(s *Service) *engine { return &engine{s: s, alerted: map[string]bool{}} }

// contact 收到了主机的请求:疑似失联解除。
func (e *engine) contact() {
	e.mu.Lock()
	recovering := !e.suspectAt.IsZero()
	p := e.prompt
	e.suspectAt, e.prompt, e.reason = time.Time{}, nil, ""
	e.alerted = map[string]bool{}
	e.mu.Unlock()
	if p != nil {
		close(p.stop)
	}
	if recovering && p != nil {
		go e.s.standbyNotify("🟢 <b>主机已恢复联系</b>,取消接任。")
	}
}

// adopted 本机切到了新纪元:之前的判定作废。
func (e *engine) adopted(uint64, uint) { e.contact() }

func (e *engine) peerSummary() *hub.HAPeers {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.peerSum
}

// PromptView 确认后接任:正在等确认(面板显示,不含确认码)。
type PromptView struct {
	SentAt int64  `json:"sentAt"`
	Epoch  uint64 `json:"epoch"`
}

// EvidenceView 失联判定的证据(面板显示)。
type EvidenceView struct {
	SuspectAt   int64          `json:"suspectAt,omitempty"`
	PeersOK     int            `json:"peersOk"`    // 能连上的同伴
	PeersLost   int            `json:"peersLost"`  // 也联系不上主机的同伴
	PeersTotal  int            `json:"peersTotal"` // 同伴总数
	Witness     *WitnessResult `json:"witness,omitempty"`
	TGConflict  bool           `json:"tgConflict,omitempty"`
	Reason      string         `json:"reason,omitempty"` // 不接任的原因代码
	ThresholdAt int64          `json:"thresholdAt,omitempty"`
}

func (e *engine) promptView() *PromptView {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.prompt == nil {
		return nil
	}
	return &PromptView{SentAt: e.prompt.sentAt.Unix(), Epoch: e.prompt.epoch}
}

func (e *engine) evidenceView() *EvidenceView {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.suspectAt.IsZero() {
		return nil
	}
	v := &EvidenceView{SuspectAt: e.suspectAt.Unix(), Witness: e.witness, Reason: e.reason,
		TGConflict: !e.tgConflict.IsZero() && e.s.now().Sub(e.tgConflict) < 5*time.Minute}
	for _, o := range e.peers {
		v.PeersTotal++
		if o.ok {
			v.PeersOK++
			if o.st.LeaseAgo*int64(time.Second) >= int64(SuspectAfter) {
				v.PeersLost++
			}
		}
	}
	return v
}

func (e *engine) reset() {
	e.mu.Lock()
	p := e.prompt
	e.suspectAt, e.prompt, e.reason, e.witness, e.peers = time.Time{}, nil, "", nil, nil
	e.mu.Unlock()
	if p != nil {
		close(p.stop)
	}
}

func (e *engine) setReason(r string) {
	e.mu.Lock()
	e.reason = r
	e.mu.Unlock()
}

// alertOnce 这一轮疑似失联里同一件事只说一次。
func (e *engine) alertOnce(key, text string) {
	e.mu.Lock()
	done := e.alerted[key]
	e.alerted[key] = true
	e.mu.Unlock()
	if !done {
		logger.Warning("容灾:", strings.ReplaceAll(strings.ReplaceAll(text, "<b>", ""), "</b>", ""))
		go e.s.standbyNotify(text)
	}
}

// tick 每 10 秒判定一次。
func (e *engine) tick() {
	s := e.s
	if !s.IsNode() || s.Epoch() == 0 || s.setting(KeyRole) == RolePromoting {
		e.reset()
		return
	}
	st, ok := s.NodeState()
	if !ok || st.Mode == ModeOff {
		e.reset()
		return
	}
	now := s.now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	lease := s.LeaseAgo()
	if lease < SuspectAfter {
		e.mu.Lock()
		was := !e.suspectAt.IsZero()
		due := now.Sub(e.peerSumAt) >= peerCheckEvery
		e.mu.Unlock()
		if was {
			e.contact()
		}
		if due { // 主机正常时也隔一阵问一圈同伴:就绪检查里显示副机之间通不通
			e.notePeers(s.collectPeers(ctx))
		}
		return
	}
	e.mu.Lock()
	if e.suspectAt.IsZero() {
		e.suspectAt = now
		logger.Warning("容灾:主机已 ", int(lease.Seconds()), " 秒没联系本机,疑似失联")
	}
	needEvidence := now.Sub(e.evidenceAt) >= evidenceEvery
	e.mu.Unlock()
	rank := st.rank()
	if rank == 0 {
		return // 不是备用:只回答同伴的问询和投票
	}
	if st.PauseUntil > now.Unix() {
		e.setReason("paused")
		return
	}
	if needEvidence {
		e.notePeers(s.collectPeers(ctx))
		e.mu.Lock()
		e.evidenceAt = now
		e.mu.Unlock()
	}
	e.mu.Lock()
	peers := e.peers
	e.mu.Unlock()
	L := s.Epoch()
	// 有人纪元比本机高:别的备用已经接任了,等它来推
	for _, o := range peers {
		if o.ok && o.st.Epoch > L {
			e.setReason("higherEpoch")
			return
		}
	}
	// 别的副机还联系得上主机:是本机和主机之间的网络问题
	for id, o := range peers {
		if o.ok && o.st.Epoch == L && o.st.LeaseAgo*int64(time.Second) < int64(SuspectAfter) {
			e.setReason("peerSeesMaster")
			e.alertOnce("peer", fmt.Sprintf("🟠 <b>本机连不上主机「%s」已 %d 分钟</b>,但 %s 还能连上:多半是本机和主机之间的网络问题,不接任。",
				notify.Esc(st.MasterName), int(lease.Minutes()), notify.Esc(s.nodeName(id))))
			return
		}
	}
	// 境外测点见证:主机对外正常就是网络分区
	if w := e.witnessNow(ctx, st, lease); w != nil && w.Verdict == "up" {
		e.setReason("witnessUp")
		e.alertOnce("witness", fmt.Sprintf("🟠 <b>本机和其它副机都连不上主机「%s」已 %d 分钟</b>,但境外测点 %d/%d 能连上它:可能是网络分区,不接任。",
			notify.Esc(st.MasterName), int(lease.Minutes()), w.OK, w.Total))
		return
	}
	// 名次接力:排在前面的备用还在、手里有副本,让它先来
	for _, o := range peers {
		if o.ok && o.st.Rank > 0 && o.st.Rank < rank && o.st.ReplicaAt > 0 && o.st.Epoch == L {
			e.setReason("betterStandby")
			return
		}
	}
	wait := time.Duration(st.ConfirmMin) * time.Minute
	if st.Mode == ModeAuto {
		wait = time.Duration(st.AutoMin) * time.Minute
	}
	wait += time.Duration(rank-1) * time.Duration(st.RankStepMin) * time.Minute
	if lease < wait {
		e.setReason("waiting")
		return
	}
	if _, ok := s.replicaInfo(); !ok {
		e.setReason("noReplica")
		return
	}
	switch st.Mode {
	case ModeConfirm:
		e.askConfirm(st, lease, peers)
	case ModeAuto:
		e.autoTakeover(ctx, st, lease)
	}
}

func (e *engine) notePeers(obs map[uint]peerObs) {
	sum := &hub.HAPeers{Total: len(obs), At: e.s.now().Unix()}
	for _, o := range obs {
		if o.ok {
			sum.OK++
		}
	}
	e.mu.Lock()
	e.peers, e.peerSum, e.peerSumAt = obs, sum, e.s.now()
	e.mu.Unlock()
}

// witnessNow 需要时做一次境外见证(主机失联 2 分钟后,每 5 分钟最多一次)。
func (e *engine) witnessNow(ctx context.Context, st State, lease time.Duration) *WitnessResult {
	s := e.s
	if !st.Witness || s.d.Witness == nil || st.Probe == nil || lease < 2*time.Minute {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.witness
	}
	e.mu.Lock()
	if e.witness != nil && s.now().Sub(e.witnessAt) < witnessEvery {
		w := e.witness
		e.mu.Unlock()
		return w
	}
	e.mu.Unlock()
	host := st.Probe.IP
	if host == "" {
		host = st.Probe.IP6
	}
	c, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	w, err := s.d.Witness(c, host, st.Probe.Ports)
	if err != nil {
		w = WitnessResult{Verdict: "unknown", Error: err.Error()}
	}
	w.At = s.now().Unix()
	e.mu.Lock()
	e.witness, e.witnessAt = &w, s.now()
	e.mu.Unlock()
	logger.Info("容灾:境外见证 ", w.Verdict, " ", w.OK, "/", w.Total)
	return &w
}

// askConfirm 确认后接任:发一次 Telegram(带确认码),等回复或面板上点。
func (e *engine) askConfirm(st State, lease time.Duration, peers map[uint]peerObs) {
	s := e.s
	e.mu.Lock()
	if e.prompt != nil {
		e.mu.Unlock()
		e.setReason("awaitConfirm")
		return
	}
	p := &prompt{code: randCode(), epoch: s.Epoch() + 1, sentAt: s.now(), stop: make(chan struct{})}
	e.prompt = p
	e.reason = "awaitConfirm"
	w := e.witness
	e.mu.Unlock()
	ri, _ := s.replicaInfo()
	lost, total := 0, 0
	for _, o := range peers {
		total++
		if o.ok && o.st.LeaseAgo*int64(time.Second) >= int64(SuspectAfter) {
			lost++
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "⚠️ <b>主机「%s」已失联 %d 分钟</b>\n", notify.Esc(st.MasterName), int(lease.Minutes()))
	if total > 0 {
		fmt.Fprintf(&sb, "其它副机 %d/%d 也联系不上它。", lost, total)
	}
	if w != nil && w.Total > 0 {
		fmt.Fprintf(&sb, "境外测点 %d/%d 能连上它。", w.OK, w.Total)
	}
	fmt.Fprintf(&sb, "\n回复 <code>/takeover %s</code>(或「接任 %s」)由「%s」接任为主机(数据截至 %s),也可以在 %s 的面板上点「接任为主机」。主机恢复后自动取消。",
		p.code, p.code, notify.Esc(s.selfName()), time.Unix(ri.AsOf, 0).In(s.loc()).Format("01-02 15:04:05"), notify.Esc(s.selfName()))
	logger.Warning("容灾:主机失联 ", int(lease.Minutes()), " 分钟,等确认接任")
	go s.standbyNotify(sb.String())
	go e.pollConfirm(p)
}

// pollConfirm 等 Telegram 上的确认(只在等确认期间长轮询,只认设置里的 Chat ID)。收到 409 记为"主机的机器人可能还活着"。
func (e *engine) pollConfirm(p *prompt) {
	s := e.s
	n := s.standbyNotifier()
	if n == nil {
		return
	}
	chat := s.standbyNotifySetting("tgChatId")
	ctx0, cancel0 := context.WithTimeout(context.Background(), 20*time.Second)
	ups, err := n.Poll(ctx0, -1, 0) // 丢掉之前积压的消息
	cancel0()
	if err == nil {
		for _, u := range ups {
			p.offset = u.ID + 1
		}
	}
	for {
		select {
		case <-p.stop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		ups, err := n.Poll(ctx, p.offset, 25)
		cancel()
		if err != nil {
			if strings.Contains(err.Error(), "409") {
				e.mu.Lock()
				first := e.tgConflict.IsZero()
				e.tgConflict = s.now()
				e.mu.Unlock()
				if first {
					go s.standbyNotify("ℹ️ Telegram 显示另一个程序正在用这个机器人收消息:主机的机器人可能还在运行,主机也许还活着。确认后再接任。")
				}
			}
			select {
			case <-p.stop:
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}
		for _, u := range ups {
			p.offset = u.ID + 1
			if u.ChatID != chat || !confirmText(u.Text, p.code) {
				continue
			}
			if s.LeaseAgo() < SuspectAfter {
				go s.standbyNotify("主机已恢复联系,不接任。")
				return
			}
			logger.Warning("容灾:Telegram 上确认了接任")
			if err := s.Takeover(TakeoverOpts{By: "telegram"}); err != nil {
				go s.standbyNotify("🔴 接任没做成:" + notify.Esc(err.Error()))
			}
			return
		}
	}
}

// confirmText 回复的是不是"/takeover 码"或"接任 码"。
func confirmText(text, code string) bool {
	f := strings.Fields(strings.TrimSpace(text))
	if len(f) != 2 || f[1] != code {
		return false
	}
	cmd := strings.ToLower(f[0])
	if i := strings.Index(cmd, "@"); i > 0 {
		cmd = cmd[:i]
	}
	return cmd == "/takeover" || cmd == "接任"
}

func randCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(9000))
	if err != nil {
		return "4821"
	}
	return fmt.Sprintf("%04d", n.Int64()+1000)
}

// autoTakeover 自动接任:三台以上要过半副机投票同意,一主一副要境外测点也说主机不通。
func (e *engine) autoTakeover(ctx context.Context, st State, lease time.Duration) {
	s := e.s
	e.mu.Lock()
	conflict := !e.tgConflict.IsZero() && s.now().Sub(e.tgConflict) < 5*time.Minute
	w := e.witness
	e.mu.Unlock()
	if conflict {
		e.setReason("tgConflict")
		return
	}
	size := st.ClusterSize
	if size < 2 {
		size = 2
	}
	if size <= 2 {
		if w == nil || w.Verdict != "down" {
			e.setReason("needWitness")
			return
		}
	} else {
		granted, err := s.requestVotes(ctx, s.Epoch()+1)
		if err != nil {
			e.setReason("vote")
			return
		}
		if granted+1 < quorum(size) {
			e.setReason("noQuorum")
			logger.Info("容灾:要票 ", granted+1, "/", quorum(size), " 没过半,不接任")
			return
		}
		if w != nil && w.Verdict == "up" {
			e.setReason("witnessUp")
			return
		}
	}
	if s.LeaseAgo() < SuspectAfter {
		return
	}
	logger.Warning("容灾:主机失联 ", int(lease.Minutes()), " 分钟,自动接任")
	if err := s.Takeover(TakeoverOpts{By: "auto"}); err != nil {
		logger.Warning("容灾:自动接任失败: ", err)
		e.setReason("takeoverFailed")
	}
}
