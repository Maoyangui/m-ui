package ha

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
	"github.com/Maoyangui/m-ui/selfupdate"
)

// 主机侧的副本调度。
//
// 备用 = 按服务器列表顺序(sort,再按 id)取前 N 台合格的副机:启用、5 分钟内在线、版本够、报过备用公钥且公钥没变、
// 磁盘够。整份加密副本只发给它们;不在名单里的,主机让它删掉手里的副本。链接是 http 也能当备用(副本端到端加密,
// 被动窃听者解不开),就绪检查里提示改用 https。
//
// 节奏:每隔 haReplicaMinutes(默认 5 分钟)一份;审计日志有新改动时 30 秒防抖后补一份;纪元或名单变了立即一份;
// 两份之间至少隔 60 秒,间隔再按上一次的耗时自适应放宽。内容没变就不重发,只告诉备用"你那份到现在还是最新的"。

var (
	replicaMinGap    = 60 * time.Second
	replicaDebounce  = 30 * time.Second
	replicaWarmup    = 90 * time.Second // 主机刚启动:各副机的状态还没拉齐,先用上次记下的名单
	replicaWarnSize  = int64(64 << 20)
	replicaOnlineFor = int64(300)
)

// 不能当备用的原因(界面按代码翻译)。
const (
	reasonDisabled   = "disabled"   // 停用了
	reasonNoLink     = "noLink"     // 没填 API 地址或令牌
	reasonOffline    = "offline"    // 5 分钟内没联系上
	reasonVersion    = "version"    // 版本太旧:要先升级
	reasonNoPub      = "noPub"      // 还没报过备用公钥(没切到本机的纪元)
	reasonPubChanged = "pubChanged" // 公钥和记住的不一样(那台重装过):到它的角色卡片里点「重新信任」
	reasonDisk       = "disk"       // 磁盘剩余不到副本的 4 倍
)

// StandbyInfo 一台副机当备用的情况(面板显示)。
type StandbyInfo struct {
	Id       uint           `json:"id"`
	Name     string         `json:"name"`
	Rank     int            `json:"rank"` // 接任顺序(1 起),0 = 不是备用
	Eligible bool           `json:"eligible"`
	Reasons  []string       `json:"reasons,omitempty"`
	Version  string         `json:"version,omitempty"`
	PubFP    string         `json:"pubFp,omitempty"`    // 它现在报的公钥指纹
	PinnedFP string         `json:"pinnedFp,omitempty"` // 记住的公钥指纹
	Plain    bool           `json:"plain,omitempty"`    // 链接是明文 http
	Replica  *hub.HAReplica `json:"replica,omitempty"`  // 它报的手里那份副本
	Ack      *AckView       `json:"ack,omitempty"`      // 主机这边最近一次发送
	LeaseAgo int64          `json:"leaseAgo"`
	DiskFree int64          `json:"diskFree"`
	Peers    *hub.HAPeers   `json:"peers,omitempty"`
	Epoch    uint64         `json:"epoch"`
}

// AckView 最近一次给这台发副本的结果。
type AckView struct {
	Seq    uint64 `json:"seq"`
	SentAt int64  `json:"sentAt"`
	AsOf   int64  `json:"asOf"`
	Size   int64  `json:"size"`
	Err    string `json:"err,omitempty"`
	ErrAt  int64  `json:"errAt,omitempty"`
}

type ack struct {
	epoch  uint64
	seq    uint64
	sum    string
	size   int64
	sentAt int64
	asOf   int64
	err    string
	errAt  int64
}

type replicator struct {
	s       *Service
	mu      sync.Mutex
	started time.Time
	kickc   chan struct{}
	running bool
	// 上一次生成
	lastBuild time.Time
	lastCost  time.Duration
	lastMax   uint64
	changedAt time.Time
	lastEpoch uint64
	lastSum   string
	lastSize  int64
	lastErr   string
	warnedBig time.Time
	// 名单
	infos    []StandbyInfo
	ids      []uint
	computed bool
	acks     map[uint]*ack
	gates    map[uint]chan struct{}
	alerted  map[uint]string
	dropAt   map[uint]time.Time
}

func newReplicator(s *Service) *replicator {
	return &replicator{s: s, kickc: make(chan struct{}, 1), acks: map[uint]*ack{}, gates: map[uint]chan struct{}{},
		alerted: map[uint]string{}, dropAt: map[uint]time.Time{}, started: s.now()}
}

// kick 尽快生成一份(设置、名单、纪元变了)。
func (r *replicator) kick() {
	select {
	case r.kickc <- struct{}{}:
	default:
	}
}

// standbyIds 当前的备用名单(按接任顺序)。主机刚启动、各副机状态还没拉齐时,用上次记下的名单,
// 免得第一轮容灾状态里名单是空的、备用把手里的副本删了。
func (r *replicator) standbyIds() []uint {
	r.mu.Lock()
	computed, ids := r.computed, append([]uint(nil), r.ids...)
	r.mu.Unlock()
	if computed {
		if ids == nil {
			ids = []uint{}
		}
		return ids
	}
	saved := r.savedIds()
	if saved == nil {
		saved = []uint{}
	}
	return saved
}

func (r *replicator) rankOf(id uint) int {
	for i, x := range r.standbyIds() {
		if x == id {
			return i + 1
		}
	}
	return 0
}

// markFor 告诉备用:你手里序号为 Seq 的那份,到 At 为止仍是最新的。
func (r *replicator) markFor(id uint) *ReplicaMark {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.acks[id]
	if a == nil || a.seq == 0 || a.epoch != r.lastEpoch {
		return nil
	}
	return &ReplicaMark{Seq: a.seq, At: a.asOf}
}

// Standbys 各副机当备用的情况(面板显示)。
func (r *replicator) Standbys() []StandbyInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]StandbyInfo, len(r.infos))
	copy(out, r.infos)
	for i := range out {
		if a := r.acks[out[i].Id]; a != nil {
			out[i].Ack = &AckView{Seq: a.seq, SentAt: a.sentAt, AsOf: a.asOf, Size: a.size, Err: a.err, ErrAt: a.errAt}
		}
	}
	return out
}

// versionOK 版本够不够当备用 / 被设为主机:认得容灾接口,而且不比主机旧(副本、整库数据的表结构它得认得)。
func (s *Service) versionOK(v string) bool {
	if v == "" {
		return false
	}
	return !selfupdate.Newer(MinVersion, v) && !selfupdate.Newer(s.d.Version, v)
}

// compute 按服务器列表顺序算出各副机当备用的情况与名单。
func (r *replicator) compute(cfg Config) ([]StandbyInfo, []uint) {
	s := r.s
	var nodes []model.Node
	s.d.DB.Where("is_local = ?", false).Order("sort asc, id asc").Find(&nodes)
	now := time.Now().Unix() // 和 hub 记的"最近联系"同一个钟
	r.mu.Lock()
	lastSize := r.lastSize
	r.mu.Unlock()
	var out []StandbyInfo
	var ids []uint
	for _, n := range nodes {
		ci := StandbyInfo{Id: n.Id, Name: n.Name, DiskFree: -1, Plain: strings.HasPrefix(strings.ToLower(strings.TrimSpace(n.ApiUrl)), "http://")}
		var st hub.NodeStatus
		ok := false
		if s.d.Hub != nil {
			st, ok = s.d.Hub.StatusOf(n.Id)
		}
		ci.Version = st.Version
		if !n.Enabled {
			ci.Reasons = append(ci.Reasons, reasonDisabled)
		}
		if strings.TrimSpace(n.ApiUrl) == "" || n.Token == "" {
			ci.Reasons = append(ci.Reasons, reasonNoLink)
		}
		if !ok || st.LastSeen == 0 || now-st.LastSeen > replicaOnlineFor {
			ci.Reasons = append(ci.Reasons, reasonOffline)
		}
		if ok && st.Version != "" && !s.versionOK(st.Version) {
			ci.Reasons = append(ci.Reasons, reasonVersion)
		}
		pinKey := KeyPubPrefix + strconv.FormatUint(uint64(n.Id), 10)
		pinned := s.setting(pinKey)
		ci.PinnedFP = Fingerprint(pinned)
		if st.HA != nil {
			ci.Replica, ci.LeaseAgo, ci.DiskFree, ci.Peers, ci.Epoch = st.HA.Replica, st.HA.LeaseAgo, st.HA.DiskFree, st.HA.Peers, st.HA.Epoch
		}
		switch {
		case st.HA == nil || st.HA.Pub == "":
			if ok && st.LastSeen > 0 && n.Enabled {
				ci.Reasons = append(ci.Reasons, reasonNoPub)
			}
		case pinned == "":
			if _, err := ParsePub(st.HA.Pub); err == nil && n.Enabled {
				if err := s.set(map[string]string{pinKey: st.HA.Pub}); err == nil {
					logger.Info("容灾:记住了副机 ", n.Name, " 的备用公钥 ", Fingerprint(st.HA.Pub))
					pinned, ci.PinnedFP = st.HA.Pub, Fingerprint(st.HA.Pub)
				}
			}
			ci.PubFP = Fingerprint(st.HA.Pub)
		default:
			ci.PubFP = Fingerprint(st.HA.Pub)
			if pinned != st.HA.Pub {
				ci.Reasons = append(ci.Reasons, reasonPubChanged)
				r.alertOnce(n.Id, "pub:"+ci.PubFP, "🟠 <b>备用公钥变了</b>:"+notify.Esc(n.Name)+" 现在报的备用公钥("+ci.PubFP+
					")和主机记住的("+ci.PinnedFP+")不一样,不再给它发数据副本。\n如果是你重装了那台的 m-ui,在服务器列表里点它的角色标签,选「重新信任」;否则那台可能被人动过。")
			}
		}
		if lastSize > 0 && st.HA != nil && st.HA.DiskFree >= 0 && st.HA.DiskFree < 4*lastSize {
			ci.Reasons = append(ci.Reasons, reasonDisk)
		}
		ci.Eligible = len(ci.Reasons) == 0
		if ci.Eligible && len(ids) < cfg.Standbys {
			ids = append(ids, n.Id)
			ci.Rank = len(ids)
		}
		out = append(out, ci)
	}
	return out, ids
}

func (r *replicator) alertOnce(id uint, what, text string) {
	r.mu.Lock()
	done := r.alerted[id] == what
	r.alerted[id] = what
	r.mu.Unlock()
	if !done {
		r.s.notify(text)
	}
}

// Repin 忘掉记住的备用公钥:下一次见到时重新记(那台重装过 m-ui 时用)。
func (s *Service) Repin(id uint) error {
	if s.IsNode() {
		return errors.New("副机上不能改主机容灾:到主机面板改")
	}
	if err := s.set(map[string]string{KeyPubPrefix + strconv.FormatUint(uint64(id), 10): ""}); err != nil {
		return err
	}
	s.rep.mu.Lock()
	delete(s.rep.alerted, id)
	s.rep.mu.Unlock()
	s.rep.kick()
	return nil
}

// tick 每 10 秒:更新名单,该生成就生成一份(在后台,不挡这一轮)。
func (r *replicator) tick() {
	s := r.s
	if s.Epoch() == 0 || s.IsNode() || s.Frozen() {
		return
	}
	cfg := s.Config()
	if cfg.Mode == ModeOff {
		r.setList(nil, nil, true)
		return
	}
	warm := s.now().Sub(r.started) >= replicaWarmup
	infos, ids := r.compute(cfg)
	if !warm {
		// 刚启动:名单先用上次记下的,只在拉齐状态后才可能变
		ids = r.standbyIds()
		for i := range infos {
			infos[i].Rank = 0
			for j, id := range ids {
				if id == infos[i].Id {
					infos[i].Rank = j + 1
				}
			}
		}
	}
	changed := r.setList(infos, ids, warm)
	r.dropOthers(infos, ids)
	if len(ids) == 0 {
		return
	}
	if r.due(cfg, changed) {
		r.mu.Lock()
		if r.running {
			r.mu.Unlock()
			return
		}
		r.running = true
		r.mu.Unlock()
		go func() {
			defer func() {
				r.mu.Lock()
				r.running = false
				r.mu.Unlock()
			}()
			r.run(ids, cfg)
		}()
	}
}

// setList 记下名单;名单变了推一轮容灾状态、存进本机设置(下次启动先用它)。返回名单变了没有。
func (r *replicator) setList(infos []StandbyInfo, ids []uint, persist bool) bool {
	saved := r.savedIds()
	r.mu.Lock()
	prev := r.ids
	if !r.computed {
		prev = saved
	}
	r.infos, r.ids, r.computed = infos, ids, true
	r.mu.Unlock()
	if persist && idsKey(saved) != idsKey(ids) {
		b, _ := json.Marshal(ids)
		if ids == nil {
			b = []byte("[]")
		}
		_ = r.s.set(map[string]string{KeyStandbyList: string(b)})
	}
	if idsKey(ids) == idsKey(prev) {
		return false
	}
	logger.Info("容灾:备用名单 [", idsKey(ids), "]")
	r.s.invalidate()
	r.kick()
	return true
}

func (r *replicator) savedIds() []uint {
	var saved []uint
	_ = json.Unmarshal([]byte(r.s.setting(KeyStandbyList)), &saved)
	return saved
}

func idsKey(ids []uint) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(uint64(id), 10)
	}
	return strings.Join(parts, ",")
}

// dropOthers 不在名单里、手里却还有副本的副机:让它删掉(10 分钟最多催一次)。
func (r *replicator) dropOthers(infos []StandbyInfo, ids []uint) {
	for _, c := range infos {
		if c.Rank > 0 || c.Replica == nil || containsId(ids, c.Id) {
			continue
		}
		r.mu.Lock()
		last := r.dropAt[c.Id]
		if r.s.now().Sub(last) < 10*time.Minute {
			r.mu.Unlock()
			continue
		}
		r.dropAt[c.Id] = r.s.now()
		r.mu.Unlock()
		var n model.Node
		if r.s.d.DB.Where("id = ?", c.Id).Limit(1).Find(&n).Error != nil || n.Id == 0 {
			continue
		}
		go r.s.DropOn(n)
	}
}

// DropOn 让一台副机删掉手里的副本(停用、删除、移出名单时;尽力而为)。
func (s *Service) DropOn(n model.Node) {
	if s.d.Hub == nil || n.ApiUrl == "" || n.Token == "" || s.Epoch() == 0 || s.IsNode() {
		return
	}
	if err := s.d.Hub.Send(n, http.MethodPost, "ha/drop", nil, 0, nil, 15*time.Second, nil); err != nil {
		logger.Warning("容灾:让副机 ", n.Name, " 删掉副本失败: ", err)
		return
	}
	logger.Info("容灾:副机 ", n.Name, " 已删掉手里的副本")
}

// due 该不该生成一份了。
func (r *replicator) due(cfg Config, listChanged bool) bool {
	s := r.s
	now := s.now()
	var maxChange uint64
	s.d.DB.Raw("SELECT COALESCE(MAX(id), 0) FROM changes").Scan(&maxChange)
	kicked := false
	select {
	case <-r.kickc:
		kicked = true
	default:
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if maxChange != r.lastMax && !r.lastBuild.IsZero() && r.changedAt.IsZero() {
		r.changedAt = now
	}
	if kicked && !r.lastBuild.IsZero() && now.Sub(r.lastBuild) < replicaMinGap {
		r.kickLater()
	}
	if !r.lastBuild.IsZero() && now.Sub(r.lastBuild) < replicaMinGap {
		return false
	}
	interval := time.Duration(cfg.ReplicaMinutes) * time.Minute
	if adapt := 4 * r.lastCost; adapt > interval {
		interval = adapt
	}
	switch {
	case r.lastBuild.IsZero(), kicked, listChanged, s.Epoch() != r.lastEpoch:
		return true
	case now.Sub(r.lastBuild) >= interval:
		return true
	case !r.changedAt.IsZero() && now.Sub(r.changedAt) >= replicaDebounce:
		return true
	}
	return false
}

// kickLater 闸还没到:记着下一次到点时补一份(调用方持有 r.mu)。
func (r *replicator) kickLater() {
	select {
	case r.kickc <- struct{}{}:
	default:
	}
}

// ReplicateNow 立刻生成一份并发给当前名单(测试用;跳过节奏控制)。
func (s *Service) ReplicateNow() error {
	if s.Epoch() == 0 || s.IsNode() {
		return errors.New("本机不是开着主机容灾的主机")
	}
	cfg := s.Config()
	if cfg.Mode == ModeOff {
		return errors.New("主机容灾没有开启")
	}
	infos, ids := s.rep.compute(cfg)
	s.rep.setList(infos, ids, true)
	if len(ids) == 0 {
		return errors.New("没有合格的备用")
	}
	return s.rep.run(ids, cfg)
}

// run 生成一份副本,加密一次,发给名单里的每台备用。
func (r *replicator) run(ids []uint, cfg Config) error {
	s := r.s
	start := s.now()
	epoch := s.Epoch()
	if err := ensureDir(s.Dir()); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.Dir(), "build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	zipPath, meta, err := s.buildReplica(dir)
	if err != nil {
		r.mu.Lock()
		r.lastErr, r.lastBuild = err.Error(), start
		r.mu.Unlock()
		logger.Warning("容灾:生成副本失败: ", err)
		return err
	}
	zst, err := os.Stat(zipPath)
	if err != nil {
		return err
	}
	size := zst.Size()
	if size > replicaWarnSize {
		r.mu.Lock()
		warn := s.now().Sub(r.warnedBig) > 24*time.Hour
		if warn {
			r.warnedBig = s.now()
		}
		r.mu.Unlock()
		if warn {
			s.notify(fmt.Sprintf("🟡 <b>数据副本偏大</b>:%.1f MB。副本每隔几分钟就发一次给备用;用户或审计日志特别多时可以在「高级」里把副本间隔调长。", float64(size)/(1<<20)))
		}
	}
	// 内容没变、备用手里就是上一份的:不重发,只告诉它"到现在还是最新的"
	var send []uint
	marked := false
	r.mu.Lock()
	for _, id := range ids {
		a := r.acks[id]
		known, have := false, uint64(0)
		if s.d.Hub != nil {
			if st, ok := s.d.Hub.StatusOf(id); ok && st.HA != nil {
				known = true
				if st.HA.Replica != nil && st.HA.Replica.Epoch == epoch {
					have = st.HA.Replica.Seq
				}
			}
		}
		if a != nil && a.epoch == epoch && a.sum == meta.Sum && a.err == "" && (!known || have == a.seq) {
			a.asOf = meta.CreatedAt
			marked = true
			continue
		}
		send = append(send, id)
	}
	r.lastSum, r.lastSize, r.lastErr = meta.Sum, size, ""
	r.mu.Unlock()
	var sendErr error
	if len(send) > 0 {
		sendErr = r.encryptAndSend(dir, zipPath, size, meta, epoch, send)
	}
	r.mu.Lock()
	r.lastBuild, r.lastCost, r.lastMax, r.lastEpoch = start, s.now().Sub(start), meta.MaxChangeId, epoch
	r.changedAt = time.Time{}
	r.mu.Unlock()
	if marked || len(send) > 0 {
		s.invalidate() // 把"截至"推给备用
	}
	return sendErr
}

// nextSeq 副本序号:只增,不低于当前时钟(主机重启、时钟回拨都不会重复)。
func (s *Service) nextSeq() (uint64, error) {
	last, _ := strconv.ParseUint(s.setting(KeyReplicaSeq), 10, 64)
	seq := uint64(s.now().UnixNano())
	if seq <= last {
		seq = last + 1
	}
	return seq, s.set(map[string]string{KeyReplicaSeq: strconv.FormatUint(seq, 10)})
}

// encryptAndSend 明文 zip 只加密一次(同一个数据密钥),每台备用只换头部里包给它的密钥和签名。
func (r *replicator) encryptAndSend(dir, zipPath string, size int64, meta ReplicaMeta, epoch uint64, ids []uint) error {
	s := r.s
	seq, err := s.nextSeq()
	if err != nil {
		return err
	}
	sign, err := s.signer()
	if err != nil {
		return err
	}
	dataKey := NewDataKey()
	ctPath := filepath.Join(dir, "replica.ct")
	zf, err := os.Open(zipPath)
	if err != nil {
		return err
	}
	cf, err := os.OpenFile(ctPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		zf.Close()
		return err
	}
	sum, err := SealStream(cf, dataKey, zf, size)
	zf.Close()
	if cerr := cf.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	base := EnvHeader{Kind: KindReplica, Epoch: epoch, Seq: seq, CreatedAt: meta.CreatedAt, MasterId: meta.MasterId,
		MasterName: meta.MasterName, Version: meta.Version, Size: size, MaxChangeId: meta.MaxChangeId}
	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		var n model.Node
		if s.d.DB.Where("id = ?", id).Limit(1).Find(&n).Error != nil || n.Id == 0 {
			continue
		}
		pub := s.setting(KeyPubPrefix + strconv.FormatUint(uint64(id), 10))
		k, err := ParsePub(pub)
		if err != nil {
			errs[i] = err
			r.noteAck(id, epoch, 0, meta, size, err)
			continue
		}
		hdr := base
		hdr.To = Fingerprint(pub)
		if hdr.Wrap, err = SealTo(dataKey, k); err != nil {
			errs[i] = err
			continue
		}
		gate := r.gate(id)
		select {
		case gate <- struct{}{}:
		default:
			continue // 上一份还没送完:这一轮跳过它
		}
		wg.Add(1)
		go func(i int, n model.Node, hdr EnvHeader) {
			defer wg.Done()
			defer func() { <-gate }()
			// 先把最新的容灾状态推过去:名单刚变时备用还不知道自己是备用,hub 下一轮才会推
			err := s.pushState(n, 15*time.Second)
			if err == nil {
				err = s.sendReplica(n, ctPath, hdr, sum, sign)
			}
			errs[i] = err
			r.noteAck(n.Id, epoch, seq, meta, size, err)
			if err != nil {
				logger.Warning("容灾:给备用 ", n.Name, " 发副本失败: ", err)
			}
		}(i, n, hdr)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (r *replicator) gate(id uint) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gates[id]
	if g == nil {
		g = make(chan struct{}, 1)
		r.gates[id] = g
	}
	return g
}

func (r *replicator) noteAck(id uint, epoch, seq uint64, meta ReplicaMeta, size int64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.acks[id]
	if a == nil {
		a = &ack{}
		r.acks[id] = a
	}
	if err != nil {
		a.err, a.errAt = err.Error(), r.s.now().Unix()
		return
	}
	*a = ack{epoch: epoch, seq: seq, sum: meta.Sum, size: size, sentAt: r.s.now().Unix(), asOf: meta.CreatedAt}
}

// sendReplica 把一份信封推给一台备用。
func (s *Service) sendReplica(n model.Node, ctPath string, hdr EnvHeader, sum [32]byte, sign func([]byte) []byte) error {
	total, err := EnvelopeSize(hdr, 64)
	if err != nil {
		return err
	}
	ct, err := os.Open(ctPath)
	if err != nil {
		return err
	}
	defer ct.Close()
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(WriteEnvelopeCT(pw, hdr, ct, sum, sign))
	}()
	defer pr.Close()
	timeout := 30*time.Second + time.Duration(total>>20)*2*time.Second
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	var out struct {
		OK  string `json:"ok"`
		Seq uint64 `json:"seq"`
	}
	h := http.Header{}
	h.Set("Content-Type", "application/octet-stream")
	if err := s.d.Hub.Send(n, http.MethodPost, "ha/replica", pr, total, h, timeout, &out); err != nil {
		return err
	}
	if out.Seq != hdr.Seq {
		return fmt.Errorf("备用确认的序号 %d 和发出去的 %d 对不上", out.Seq, hdr.Seq)
	}
	return nil
}
