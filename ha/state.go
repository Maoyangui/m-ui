package ha

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/render"

	"gorm.io/gorm"
)

// State 主机发给副机的容灾状态(POST agent/ha/state)。很小,不进快照、不动修订号:
// 只在变了时推(另每 10 分钟重推一次),副机只在修订变了时写一行本机设置。
type State struct {
	Epoch       uint64       `json:"epoch"`
	MasterId    uint         `json:"masterId"`
	MasterName  string       `json:"masterName"`
	MasterPub   string       `json:"masterPub"`
	Self        uint         `json:"self"` // 接收方在服务器表里的 id
	Mode        string       `json:"mode"`
	Candidates  []uint       `json:"candidates"`  // 候选,按名次
	ClusterSize int          `json:"clusterSize"` // 启用的服务器总数(含主机)
	ConfirmMin  int          `json:"confirmMin"`
	AutoMin     int          `json:"autoMin"`
	RankStepMin int          `json:"rankStepMin"`
	ReplicaMin  int          `json:"replicaMin"`
	PauseUntil  int64        `json:"pauseUntil,omitempty"` // 这之前候选不得发起接管(手动暂停、升级前、迁回冻结)
	Witness     bool         `json:"witness"`
	Probe       *Probe       `json:"probe,omitempty"`      // 见证测哪里:主机的公网 IP 与端口
	ClusterKey  string       `json:"clusterKey,omitempty"` // 副机之间互相确认的 HMAC 密钥
	Recovery    bool         `json:"recovery,omitempty"`
	SinceChange uint64       `json:"sinceChange,omitempty"` // 这个纪元接管所用副本的最大审计 id(副机回 stale 时带给旧主机)
	Handover    *Grant       `json:"handover,omitempty"`    // 迁回交接:纪元 Epoch 的新主机是 To
	Notify      string       `json:"notify,omitempty"`      // 只发候选:用候选公钥密封的通知设置
	Replica     *ReplicaMark `json:"replica,omitempty"`     // 只发候选:主机确认它手里的副本到什么时候为止仍是最新的
	Rev         string       `json:"rev"`
}

// ReplicaMark 候选手里序号为 Seq 的副本,内容截至 At 都没变(主机这之后生成的副本和它一样,就不重发了)。
type ReplicaMark struct {
	Seq uint64 `json:"seq"`
	At  int64  `json:"at"`
}

// Probe 见证要测的主机地址。
type Probe struct {
	IP    string `json:"ip,omitempty"`
	IP6   string `json:"ip6,omitempty"`
	Ports []int  `json:"ports,omitempty"`
}

// notifyKeys 候选发通知要用的设置(主机只发给候选,密封在候选公钥下)。
var notifyKeys = []string{"tgEnabled", "tgToken", "tgChatId", "tgProxy", "tgApiBase", "webhookUrl", "webhookSecret"}

// ---- 主机侧 ----

// signKey 本机签名密钥:没有就生成(开着容灾的主机、临时主机才会用到)。返回公钥。
func (s *Service) signKey() (string, error) {
	seed := s.setting(KeySignKey)
	if k, err := SignKey(seed); err == nil {
		return PubOf(k), nil
	}
	seed = NewSignKey()
	if err := s.set(map[string]string{KeySignKey: seed}); err != nil {
		return "", err
	}
	k, _ := SignKey(seed)
	return PubOf(k), nil
}

// signer 用本机签名密钥签名的函数。
func (s *Service) signer() (func([]byte) []byte, error) {
	if _, err := s.signKey(); err != nil {
		return nil, err
	}
	k, err := SignKey(s.setting(KeySignKey))
	if err != nil {
		return nil, err
	}
	return func(b []byte) []byte { return signEd25519(k, b) }, nil
}

// clusterKey 集群密钥:没有就生成。
func (s *Service) clusterKey() string {
	if k := s.setting(KeyClusterKey); k != "" {
		return k
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	k := base64.StdEncoding.EncodeToString(b)
	_ = s.set(map[string]string{KeyClusterKey: k})
	return k
}

// RotateClusterKey 停用 / 删除副机时换集群密钥:它手里的旧密钥不能再拿来冒充同伴投票。
func (s *Service) RotateClusterKey() {
	if s.Epoch() == 0 || s.IsNode() {
		return
	}
	_ = s.set(map[string]string{KeyClusterKey: ""})
	s.clusterKey()
	s.invalidate()
}

func (s *Service) pauseUntil() int64 {
	p, _ := strconv.ParseInt(s.setting(KeyPauseUntil), 10, 64)
	if f := s.freezeUntil(); f > p {
		p = f
	}
	return p
}

func (s *Service) freezeUntil() int64 {
	f, _ := strconv.ParseInt(s.setting(KeyFreezeUntil), 10, 64)
	return f
}

// Frozen 临时主机正在迁回:管理员、代理、外部 API、Telegram 机器人的写操作一律暂停。
func (s *Service) Frozen() bool {
	return !s.IsNode() && s.freezeUntil() > s.now().Unix()
}

// probe 见证要测的地址:主机的公网 IP、面板与订阅端口、主机上第一条 TCP 线路的端口。
func (s *Service) probe() *Probe {
	p := &Probe{IP: s.setting("publicIp"), IP6: s.setting("publicIp6")}
	if strings.Contains(p.IP, ":") { // 纯 IPv6 机器 publicIp 记的是 IPv6
		p.IP6, p.IP = p.IP, ""
	}
	add := func(port int) {
		if port <= 0 || port > 65535 {
			return
		}
		for _, x := range p.Ports {
			if x == port {
				return
			}
		}
		p.Ports = append(p.Ports, port)
	}
	add(s.settingInt("webPort", 2053, 1, 65535))
	add(s.settingInt("subPort", 2056, 1, 65535))
	self := s.SelfId()
	var lines []model.Line
	s.d.DB.Where("enabled = ?", true).Order("sort asc, id asc").Find(&lines)
	for _, l := range lines {
		if render.UDPOnly(l.Protocol) || !render.LineOnNode(l, self) {
			continue
		}
		add(l.Port)
		break
	}
	if p.IP == "" && p.IP6 == "" {
		return nil
	}
	return p
}

// baseState 这一轮发给所有副机的状态(不含接收方相关的字段)。
func (s *Service) baseState() (State, error) {
	pub, err := s.signKey()
	if err != nil {
		return State{}, err
	}
	cfg := s.Config()
	st := State{
		Epoch: s.Epoch(), MasterId: s.SelfId(), MasterName: s.selfName(), MasterPub: pub, Mode: cfg.Mode,
		Candidates: s.rep.candidateIds(), ClusterSize: s.clusterSize(),
		ConfirmMin: cfg.ConfirmMinutes, AutoMin: cfg.AutoMinutes, RankStepMin: cfg.RankStepMinutes, ReplicaMin: cfg.ReplicaMinutes,
		PauseUntil: s.pauseUntil(), Witness: cfg.Witness, Recovery: cfg.Recovery,
	}
	if cfg.Mode != ModeOff {
		st.Probe = s.probe()
		st.ClusterKey = s.clusterKey()
	}
	var pf PromotedFrom
	if s.getJSON(KeyPromotedFrom, &pf) {
		st.SinceChange = pf.MaxChangeId
	}
	var g Grant
	if s.getJSON(KeyHandoverGrant, &g) && g.Epoch > st.Epoch {
		st.Handover = &g
	}
	return st, nil
}

// baseCached 同 baseState,几秒内重用:hub 每 5 秒对每台副机都要一份,不必每台各查二十来次库。
func (s *Service) baseCached() (State, error) {
	s.mu.Lock()
	if s.base != nil && s.now().Sub(s.baseAt) < 4*time.Second {
		st := *s.base
		s.mu.Unlock()
		return st, nil
	}
	s.mu.Unlock()
	st, err := s.baseState()
	if err != nil {
		return st, err
	}
	s.mu.Lock()
	cp := st
	s.base, s.baseAt = &cp, s.now()
	s.mu.Unlock()
	return st, nil
}

// invalidate 配置、名单、纪元变了:下一轮重新组装状态,并让 hub 尽快重推。
func (s *Service) invalidate() {
	s.mu.Lock()
	s.base = nil
	s.mu.Unlock()
	if s.d.Hub != nil {
		s.d.Hub.ForgetHAState()
	}
}

// clusterSize 启用的服务器总数(含主机)。
func (s *Service) clusterSize() int {
	var n int64
	s.d.DB.Model(&model.Node{}).Where("enabled = ?", true).Count(&n)
	if n < 1 {
		n = 1
	}
	return int(n)
}

// StateFor 这一轮给副机 n 的容灾状态与它的修订号(给 hub 用;rev 为空 = 不推)。
// 本机纪元为 0(没开过容灾)时一律不推:主副机之间的请求和以前一字不差。
func (s *Service) StateFor(n model.Node) ([]byte, string) {
	if s.Epoch() == 0 || s.IsNode() {
		return nil, ""
	}
	st, err := s.baseCached()
	if err != nil {
		logger.Warning("组装容灾状态失败: ", err)
		return nil, ""
	}
	st.Self = n.Id
	plainSig := ""
	if s.rep.rankOf(n.Id) > 0 && st.Mode != ModeOff {
		if pub := s.setting(KeyPubPrefix + strconv.FormatUint(uint64(n.Id), 10)); pub != "" {
			st.Notify, plainSig = s.notifyBlock(pub)
		}
		st.Replica = s.rep.markFor(n.Id)
	}
	st.Rev = stateRev(st, plainSig)
	b, _ := json.Marshal(st)
	s.mu.Lock()
	s.stateRev[n.Id] = st.Rev
	s.mu.Unlock()
	return b, st.Rev
}

// stateRev 状态的修订号:通知块按明文算(密封每次都不同)。
func stateRev(st State, notifySig string) string {
	st.Rev, st.Notify = "", notifySig
	b, _ := json.Marshal(st)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// notifyBlock 给候选的通知块(密封在它的公钥下)与明文指纹;明文没变就用缓存的密文。
func (s *Service) notifyBlock(pub string) (string, string) {
	m := map[string]string{}
	for _, k := range notifyKeys {
		m[k] = s.setting(k)
	}
	m["masterName"] = s.selfName()
	plain, _ := json.Marshal(m)
	sum := sha256.Sum256(append([]byte(pub+"\x00"), plain...))
	sig := hex.EncodeToString(sum[:8])
	s.mu.Lock()
	cached, ok := s.sealCache[sig]
	s.mu.Unlock()
	if ok {
		return cached, sig
	}
	k, err := ParsePub(pub)
	if err != nil {
		return "", ""
	}
	sealed, err := SealTo(plain, k)
	if err != nil {
		return "", ""
	}
	s.mu.Lock()
	if len(s.sealCache) > 64 {
		s.sealCache = map[string]string{}
	}
	s.sealCache[sig] = sealed
	s.mu.Unlock()
	return sealed, sig
}

// ---- 副机侧 ----

// canAdopt 只有这两个动作能把副机的纪元升上去:其余动作(拉报告、踢线、设备租约……)都得等它先切过来。
func canAdopt(action string) bool { return action == "apply" || action == "ha/state" }

// CheckAgent 副机按纪元规则判定一个令牌已经验过的请求(agent 鉴权里调用)。返回 nil 放行;否则调用方回 409 和这个正文。
// 本机是主机(含临时主机)时也会判:被接管的旧主机来请求,收到的是 409 stale_epoch 而不是 403,它据此降级。
func (s *Service) CheckAgent(h http.Header, action string) *Reject {
	e, m := Header(h)
	L := s.Epoch()
	if e == 0 && L == 0 {
		return nil
	}
	reject := func(code string) *Reject {
		r := &Reject{Error: RejectText(code), Code: code, Epoch: L}
		var st State
		if s.getJSON(KeyState, &st) {
			r.SinceChange = st.SinceChange
		}
		r.MasterId, _ = s.masterId()
		if !s.IsNode() {
			r.MasterId = s.SelfId()
			var pf PromotedFrom
			if s.getJSON(KeyPromotedFrom, &pf) {
				r.SinceChange = pf.MaxChangeId
			}
		}
		return r
	}
	if !s.IsNode() {
		switch {
		case e < L, e == L && m != s.SelfId():
			return reject(CodeStale)
		case e > L:
			return reject(CodeFollowing) // 本机还是主机:不会因为别人推送就让位(迁回要走提交)
		}
		return nil
	}
	mid, _ := s.masterId()
	v := NodeView{Epoch: L, MasterId: mid, Fenced: s.setting(KeyRole) == RoleFenced}
	s.mu.Lock()
	v.LastContact = s.lastContact
	s.mu.Unlock()
	var vote Vote
	if s.getJSON(KeyVote, &vote) {
		v.Vote = &vote
	}
	var grant Grant
	if s.getJSON(KeyGrant, &grant) {
		v.Grant = &grant
	}
	verdict := Decide(v, e, m, canAdopt(action), s.now())
	if !verdict.OK() {
		return reject(verdict.Code)
	}
	if verdict.Adopt {
		if err := s.adopt(e, m); err != nil {
			logger.Warning("切换到纪元 ", e, " 失败: ", err)
			return &Reject{Error: "切换纪元失败: " + err.Error(), Code: CodeNotAdopted, Epoch: L}
		}
	}
	if verdict.Contact {
		s.touch()
	}
	return nil
}

func (s *Service) masterId() (uint, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(s.setting(KeyMasterId)), 10, 32)
	return uint(n), err == nil && n > 0
}

// touch 刷新租约:收到了当前纪元主机的请求。
func (s *Service) touch() {
	s.mu.Lock()
	s.lastContact = s.now()
	s.mu.Unlock()
	s.eng.contact()
}

// LeaseAgo 距上次收到当前主机请求多久了。
func (s *Service) LeaseAgo() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now().Sub(s.lastContact)
}

// adopt 副机切到更高的纪元:同一事务里写新纪元、新主机,快照序号基线清零(新主机的序号不和旧主机的比)。
func (s *Service) adopt(e uint64, m uint) error {
	old := s.Epoch()
	err := s.d.DB.Transaction(func(tx *gorm.DB) error {
		values := map[string]string{
			KeyEpoch: strconv.FormatUint(e, 10), KeyMasterId: strconv.FormatUint(uint64(m), 10),
			"hubSnapshotSequence": "", KeyMasterPub: "", KeyState: "", KeyVote: "",
		}
		var role string
		tx.Raw("SELECT value FROM settings WHERE key = ?", KeyRole).Scan(&role)
		if role == RoleFenced {
			values[KeyRole] = "" // 已降级的原主机作为副机加入了新主机:横幅撤掉,降级记录(分叉改动)留着
		}
		var g Grant
		var raw string
		tx.Raw("SELECT value FROM settings WHERE key = ?", KeyGrant).Scan(&raw)
		if json.Unmarshal([]byte(raw), &g) == nil && g.Epoch <= e {
			values[KeyGrant] = "" // 交接标记用过了
		}
		return upsert(tx, values)
	})
	if err == nil {
		logger.Info("容灾:本机切到纪元 ", e, "(原 ", old, "),主机是服务器 #", m)
		s.mu.Lock()
		s.lastContact = s.now()
		s.mu.Unlock()
		s.eng.adopted(e, m)
	}
	return err
}

// ApplyState 副机收下主机推来的容灾状态(纪元已经在 CheckAgent 里对齐)。
func (s *Service) ApplyState(st State) error {
	L := s.Epoch()
	if st.Epoch != L {
		return fmt.Errorf("容灾状态的纪元 %d 与本机的 %d 不一致", st.Epoch, L)
	}
	if _, err := ParseSignPub(st.MasterPub); err != nil {
		return err
	}
	notify, key, handover := st.Notify, st.ClusterKey, st.Handover
	st.Notify, st.ClusterKey = "", ""
	values := map[string]string{KeyState: mustJSON(st), KeyMasterPub: st.MasterPub, KeyNotify: notify, KeyClusterKeyLocal: key}
	if handover != nil && handover.Epoch > L {
		handover.At = s.now().Unix()
		values[KeyGrant] = mustJSON(handover)
		logger.Info("容灾:收到迁回交接标记,纪元 ", handover.Epoch, " 的主机是服务器 #", handover.To)
	}
	if err := s.set(values); err != nil {
		return err
	}
	if st.Replica != nil {
		s.noteReplicaMark(*st.Replica)
	}
	if !containsId(st.Candidates, st.Self) || st.Mode == ModeOff {
		if err := s.DropReplica(); err != nil {
			logger.Warning("删除本机副本失败: ", err)
		}
	}
	return nil
}

// NodeState 副机最近一次收到的容灾状态。
func (s *Service) NodeState() (State, bool) {
	var st State
	ok := s.getJSON(KeyState, &st)
	return st, ok
}

// rank 本机在候选名单里的名次(1 起;不是候选为 0)。
func (st State) rank() int {
	for i, id := range st.Candidates {
		if id == st.Self {
			return i + 1
		}
	}
	return 0
}

func containsId(ids []uint, id uint) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func sortedIds(m map[uint]bool) []uint {
	out := make([]uint, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
