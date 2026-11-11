package ha

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/render"
)

// State 主机发给副机的容灾状态(POST agent/ha/state)。很小,不进快照、不动修订号:
// 只在变了时推(另每 10 分钟重推一次),副机只在修订变了时写一行本机设置。
// 线上传的是 SignedState:正文由主机的签名密钥签过,副机只认本纪元钉住的那把公钥(见 ReceiveState)。
type State struct {
	Epoch       uint64       `json:"epoch"`
	MasterId    uint         `json:"masterId"`
	MasterName  string       `json:"masterName"`
	MasterPub   string       `json:"masterPub"`
	Instance    string       `json:"inst"`               // 主机的实例号:副机回 409 时报给别人(同一份库是不是在另一台机器上当主机)
	BecameAt    int64        `json:"becameAt,omitempty"` // 主机在这个纪元当上主机的时间
	Self        uint         `json:"self"`               // 接收方在服务器表里的 id
	Mode        string       `json:"mode"`
	Standbys    []uint       `json:"standbys"`    // 备用,按接任顺序
	ClusterSize int          `json:"clusterSize"` // 启用的服务器总数(含主机)
	ConfirmMin  int          `json:"confirmMin"`
	AutoMin     int          `json:"autoMin"`
	RankStepMin int          `json:"rankStepMin"`
	ReplicaMin  int          `json:"replicaMin"`
	PauseUntil  int64        `json:"pauseUntil,omitempty"` // 这之前备用不得接任(手动暂停、升级与重启前、设为主机进行中)
	Witness     bool         `json:"witness"`
	Probe       *Probe       `json:"probe,omitempty"`      // 见证测哪里:主机的公网 IP 与端口
	ClusterKey  string       `json:"clusterKey,omitempty"` // 副机之间互相确认的 HMAC 密钥
	Former      *FormerMark  `json:"former,omitempty"`     // 失联的原主机与接任所用数据的最大审计 id(它回来时副机据此报"失联期间多收的改动")
	Handover    *Grant       `json:"handover,omitempty"`   // 交接标记:纪元 Epoch 的主机是 To(设为主机、收回、围栏)
	Revoke      uint64       `json:"revoke,omitempty"`     // 撤销:副机清掉纪元不超过它的交接标记
	Notify      string       `json:"notify,omitempty"`     // 只发备用:用备用公钥密封的通知设置
	Replica     *ReplicaMark `json:"replica,omitempty"`    // 只发备用:主机确认它手里的副本到什么时候为止仍是最新的
	Rev         string       `json:"rev"`
}

// StateV 线上容灾状态的版本:副机只收这个版本、签过名的。
const StateV = 2

// stateSigContext 容灾状态签名的上下文前缀(签的是正文原始字节,不重新序列化)。
const stateSigContext = "m-ui/ha/state/v2\n"

// SignedState 线上传的容灾状态:Body 是 State 的 JSON,Sig = base64(ed25519(haSignKey, "m-ui/ha/state/v2\n" + Body))。
type SignedState struct {
	V    int             `json:"v"`
	Body json.RawMessage `json:"body"`
	Sig  string          `json:"sig"`
}

// FormerMark 状态里带的失联原主机:它回来时副机报给它"接任所用数据截至哪条审计"。
type FormerMark struct {
	Id          uint   `json:"id"`
	SinceChange uint64 `json:"sinceChange"`
}

// ReplicaMark 备用手里序号为 Seq 的副本,内容截至 At 都没变(主机这之后生成的副本和它一样,就不重发了)。
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

// notifyKeys 备用发通知要用的设置(主机只发给备用,密封在备用公钥下)。
var notifyKeys = []string{"tgEnabled", "tgToken", "tgChatId", "tgProxy", "tgApiBase", "webhookUrl", "webhookSecret"}

// ---- 主机侧 ----

// signKey 本机签名密钥:没有就生成。返回公钥。
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

// SignPub 本机的签名公钥(没有就生成):主机签容灾状态、副本、整库;候选人要票、被设为主机前先生成,当上主机后沿用它。
func (s *Service) SignPub() (string, error) { return s.signKey() }

// signPriv 本机签名私钥(没有就生成)。
func (s *Service) signPriv() (ed25519.PrivateKey, error) {
	if _, err := s.signKey(); err != nil {
		return nil, err
	}
	return SignKey(s.setting(KeySignKey))
}

// signer 用本机签名密钥签名的函数。
func (s *Service) signer() (func([]byte) []byte, error) {
	k, err := s.signPriv()
	if err != nil {
		return nil, err
	}
	return func(b []byte) []byte { return signEd25519(k, b) }, nil
}

// signState 把容灾状态的正文签好名,包成线上的 SignedState。
func signState(k ed25519.PrivateKey, body []byte) []byte {
	sig := ed25519.Sign(k, append([]byte(stateSigContext), body...))
	b, _ := json.Marshal(SignedState{V: StateV, Body: body, Sig: base64.StdEncoding.EncodeToString(sig)})
	return b
}

// verifyState 用 pub 验一份容灾状态正文的签名。
func verifyState(pub ed25519.PublicKey, body []byte, sig string) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sig))
	if err != nil || len(pub) != ed25519.PublicKeySize || len(raw) != ed25519.SignatureSize ||
		!ed25519.Verify(pub, append([]byte(stateSigContext), body...), raw) {
		return errStateSig
	}
	return nil
}

var (
	errStateSig      = errors.New("容灾状态的签名不对:不是本机认的主机签发的")
	errStatePub      = errors.New("容灾状态里的主机签名公钥和本机为这个纪元记下的不一样:不收(同一纪元不换签名公钥)")
	errStateUnsigned = errors.New("容灾状态没有签名,或版本不对:主机和本机的版本不一致,先把两边升级到同一版本")
)

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

// Frozen 正在设为主机:管理员、代理、外部 API、Telegram 机器人的写操作一律暂停(超时自动解冻)。
// 提交请求发出之后、结论明确之前(commit / unknown)一律算冻结:写闸不因冻结时间到了自己打开。
func (s *Service) Frozen() bool {
	if s.IsNode() {
		return false
	}
	if s.freezeUntil() > s.now().Unix() {
		return true
	}
	st := s.switchState()
	return st.Stage == StageCommit || st.Stage == StageUnknown
}

// FrozenText 写操作被暂停时给出的说明。
func (s *Service) FrozenText() string {
	sw := s.switchState()
	name := sw.TargetName
	if name == "" {
		name = "#" + strconv.FormatUint(uint64(sw.Target), 10) // 不拼中文名词:英文界面按整句换
	}
	return fmt.Errorf("正在把主机换到 %s,写操作暂停(最多几分钟,完成或失败后自动恢复)", name).Error()
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
		Epoch: s.Epoch(), MasterId: s.SelfId(), MasterName: s.selfName(), MasterPub: pub, Instance: s.Instance(), BecameAt: s.becameAt(),
		Mode: cfg.Mode, Standbys: s.rep.standbyIds(), ClusterSize: s.clusterSize(),
		ConfirmMin: cfg.ConfirmMinutes, AutoMin: cfg.AutoMinutes, RankStepMin: cfg.RankStepMinutes, ReplicaMin: cfg.ReplicaMinutes,
		PauseUntil: s.pauseUntil(), Witness: cfg.Witness,
	}
	if cfg.Mode == ModeOff {
		st.Standbys = []uint{}
	} else {
		st.Probe = s.probe()
		st.ClusterKey = s.clusterKey()
	}
	var f Former
	if s.getJSON(KeyFormer, &f) && f.Id != 0 && f.SinceChange > 0 {
		st.Former = &FormerMark{Id: f.Id, SinceChange: f.SinceChange}
	}
	st.Revoke = s.revoked()
	if r := s.sw.revoke(); r > st.Revoke { // 设为主机没做成 / 正在隔离目标:撤销已经发出去的交接标记
		st.Revoke = r
	}
	// 设为主机的交接标记(switch.go)与收回 / 围栏给自己的交接标记:取纪元高的那个(撤销过的不再发)
	for _, g := range []*Grant{s.sw.grant(), s.pendingGrant()} {
		if g != nil && g.Epoch > st.Epoch && g.Epoch > st.Revoke && (st.Handover == nil || g.Epoch > st.Handover.Epoch) {
			st.Handover = s.ownGrant(*g, st.Epoch, st.MasterId)
		}
	}
	return st, nil
}

// ownGrant 随状态推出去的交接标记一律以本机的名义、在当前纪元签发;没写时效的按冻结截止(设为主机)或 10 分钟算。
func (s *Service) ownGrant(g Grant, epoch uint64, self uint) *Grant {
	g.From, g.FromEpoch, g.At = self, epoch, 0
	if g.Expires == 0 {
		g.Expires = s.freezeUntil()
		if g.Expires <= s.now().Unix() {
			g.Expires = s.now().Add(grantTTL).Unix()
		}
	}
	return &g
}

// becameAt 本机在当前纪元当上主机的时间(同纪元两台主机比先后):接任 / 设为主机看 haBecame,收回 / 围栏看纪元记录。
func (s *Service) becameAt() int64 {
	L := s.Epoch()
	var b Became
	if s.getJSON(KeyBecame, &b) && b.Epoch == L && b.At > 0 {
		return b.At
	}
	if r := readEpochRecord(s.Dir()); r.Epoch == L && r.Master == s.SelfId() {
		return r.At
	}
	return 0
}

// baseCached 同 baseState(连同签名私钥),几秒内重用:hub 每 5 秒对每台副机都要一份,不必每台各查二十来次库。
func (s *Service) baseCached() (State, ed25519.PrivateKey, error) {
	c := s.core()
	s.mu.Lock()
	if s.base != nil && c.baseKey != nil && s.now().Sub(s.baseAt) < 4*time.Second {
		st, k := *s.base, c.baseKey
		s.mu.Unlock()
		return st, k, nil
	}
	s.mu.Unlock()
	st, err := s.baseState()
	if err != nil {
		return st, nil, err
	}
	k, err := s.signPriv()
	if err != nil {
		return st, nil, err
	}
	s.mu.Lock()
	cp := st
	s.base, c.baseKey, s.baseAt = &cp, k, s.now()
	s.mu.Unlock()
	return st, k, nil
}

// invalidate 设置、名单、纪元变了:下一轮重新组装状态,并让 hub 尽快重推。
func (s *Service) invalidate() {
	c := s.core()
	s.mu.Lock()
	s.base, c.baseKey = nil, nil
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

// StateFor 这一轮给副机 n 的容灾状态(签好名的 SignedState)与它的修订号(给 hub 用;rev 为空 = 不推)。
// 本机纪元为 0(没开过容灾、没换过主机)时一律不推:主副机之间的请求和以前一字不差。
func (s *Service) StateFor(n model.Node) ([]byte, string) { return s.stateFor(n, true) }

// stateFor trusted 为假时(还没确认回来的原主机)不带集群密钥、通知块与副本标记。
func (s *Service) stateFor(n model.Node, trusted bool) ([]byte, string) {
	if s.Epoch() == 0 || s.IsNode() {
		return nil, ""
	}
	st, key, err := s.baseCached()
	if err != nil {
		logger.Warning("组装容灾状态失败: ", err)
		return nil, ""
	}
	st.Self = n.Id
	plainSig := ""
	if !trusted {
		st.ClusterKey = ""
	} else if st.Mode != ModeOff && containsId(st.Standbys, n.Id) {
		if pub := s.setting(KeyPubPrefix + strconv.FormatUint(uint64(n.Id), 10)); pub != "" {
			st.Notify, plainSig = s.notifyBlock(pub)
		}
		st.Replica = s.rep.markFor(n.Id)
	}
	st.Rev = stateRev(st, plainSig)
	body, _ := json.Marshal(st)
	return signState(key, body), st.Rev
}

// stateRev 状态的修订号:通知块按明文算(密封每次都不同)。
func stateRev(st State, notifySig string) string {
	st.Rev, st.Notify = "", notifySig
	b, _ := json.Marshal(st)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// notifyBlock 给备用的通知块(密封在它的公钥下)与明文指纹;明文没变就用缓存的密文。
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

// pushState 立刻给一台副机推一份容灾状态(不等 hub 的下一轮)。
func (s *Service) pushState(n model.Node, timeout time.Duration) error {
	if s.d.Hub == nil {
		return errors.New("本机容灾服务未初始化")
	}
	body, rev := s.StateFor(n)
	if rev == "" {
		return nil
	}
	return s.d.Hub.Send(n, http.MethodPost, "ha/state", bytes.NewReader(body), int64(len(body)), nil, timeout, nil)
}

// remoteNodes 启用的远端副机(有地址和令牌的)。
func (s *Service) remoteNodes() []model.Node {
	var nodes []model.Node
	s.d.DB.Where("enabled = ? AND is_local = ? AND COALESCE(api_url, '') <> '' AND COALESCE(token, '') <> ''", true, false).
		Order("sort asc, id asc").Find(&nodes)
	return nodes
}

// nodesExcept 启用的远端副机,去掉 id 那台。
func (s *Service) nodesExcept(id uint) []model.Node {
	var out []model.Node
	for _, n := range s.remoteNodes() {
		if n.Id != id {
			out = append(out, n)
		}
	}
	return out
}

// pushAll 立刻给一组副机推容灾状态(并发、尽力而为),返回推成功的台数。
func (s *Service) pushAll(nodes []model.Node, timeout time.Duration) int {
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for _, n := range nodes {
		wg.Add(1)
		go func(n model.Node) {
			defer wg.Done()
			if err := s.pushState(n, timeout); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	return ok
}

// pushStandbys 立刻把状态推给备用(暂停、恢复时;重启就在眼前,等不到 hub 的下一轮)。
func (s *Service) pushStandbys(timeout time.Duration) {
	if s.Epoch() == 0 || s.IsNode() {
		return
	}
	ids := s.rep.standbyIds()
	var list []model.Node
	for _, n := range s.remoteNodes() {
		if containsId(ids, n.Id) {
			list = append(list, n)
		}
	}
	s.pushAll(list, timeout)
}

// pushStandbysSoon 同上,在后台做(界面上点了暂停不用等)。
func (s *Service) pushStandbysSoon() { go s.pushStandbys(10 * time.Second) }

// ---- 副机侧 ----

// canAdopt 只有这两个动作能把副机的纪元升上去:其余动作(拉报告、踢线、设备租约……)都得等它先切过来。
func canAdopt(action string) bool { return action == "apply" || action == "ha/state" }

// CheckAgent 按纪元规则判定一个令牌已经验过的请求(agent 鉴权里调用)。返回 nil 放行;否则调用方回 409 和这个正文。
// 本机是主机时也会判:原主机回来后带着旧纪元来请求,收到的是 409 stale_epoch 而不是 403,它据此变为副机。
// 凭交接标记 / 投票换主(钉新主机的签名公钥)时,这里只放行 ha/state、不换:新主机拿出用那把公钥签过名的状态才换
// (见 ReceiveState)—— 只拿到副机令牌的人冒充不了新主机。
func (s *Service) CheckAgent(h http.Header, action string) *Reject {
	e, m := Header(h)
	L := s.Epoch()
	if e == 0 && L == 0 {
		return nil
	}
	if !s.IsNode() {
		switch {
		case e < L, e == L && m != s.SelfId():
			return s.reject(CodeStale, m)
		case e > L:
			return s.reject(CodeFollowing, m) // 本机还是主机:不会因为别人推送就让位(换主机要走设为主机)
		}
		return nil
	}
	verdict, pin, hadVote := s.nodeVerdict(e, m, action)
	if !verdict.OK() {
		return s.reject(verdict.Code, m)
	}
	if verdict.Adopt {
		if pin != "" {
			if action != "ha/state" {
				return s.reject(CodeNotAdopted, m) // 先推容灾状态:验过新主机的签名才换
			}
			return nil
		}
		if err := s.adopt(e, m, ""); err != nil {
			logger.Warning("切换到纪元 ", e, " 失败: ", err)
			return &Reject{Error: "切换纪元失败: " + err.Error(), Code: CodeNotAdopted, Epoch: L, Role: s.Role()}
		}
	} else if verdict.Contact {
		if hadVote {
			// 重新联系上了当前主机:之前投的票作废(主机还在,那次没成的选举不该再生效)
			_ = s.set(map[string]string{KeyVote: ""})
		}
		if s.setting(KeyRole) == RoleDemoted {
			// 让给了同纪元的另一台主机(没有更高的纪元可换):它来管本机了,本机就是普通副机
			if err := s.set(map[string]string{KeyRole: ""}); err == nil {
				s.addNotice(Notice{Kind: "joined", At: s.now().Unix()})
			}
		}
	}
	if verdict.Contact {
		s.touch()
	}
	return nil
}

// nodeVerdict 副机按本机状态对 (e, m) 的判定;换主时返回要钉的新主机签名公钥(凭交接标记 / 投票换主才有),以及本机有没有票。
func (s *Service) nodeVerdict(e uint64, m uint, action string) (Verdict, string, bool) {
	mid, _ := s.masterId()
	v := NodeView{Epoch: s.Epoch(), MasterId: mid, Demoted: s.setting(KeyRole) == RoleDemoted}
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
	pin := ""
	if verdict.Adopt {
		switch verdict.Via {
		case "vote":
			pin = strings.TrimSpace(vote.CandPub)
		case "grant":
			pin = strings.TrimSpace(grant.ToPub)
		}
	}
	return verdict, pin, v.Vote != nil
}

// reject 409 正文:本机认的纪元与主机、角色、主机的实例号;请求方 m 是记下的失联原主机时带上接任所用数据截至哪条审计。
// 本机是主机时不填租约(主机没有"上次收到主机的请求",填了会被对方当成跟着一台已经不在的主机)。
func (s *Service) reject(code string, m uint) *Reject {
	L := s.Epoch()
	r := &Reject{Error: RejectText(code), Code: code, Epoch: L, Role: s.Role()}
	if !s.IsNode() {
		r.MasterId, r.MasterInst, r.BecameAt = s.SelfId(), s.Instance(), s.becameAt()
		var f Former
		if m != 0 && s.getJSON(KeyFormer, &f) && f.Id == m {
			r.SinceChange = f.SinceChange
		}
		return r
	}
	r.MasterId, _ = s.masterId()
	r.LeaseAgo = int64(s.LeaseAgo() / time.Second)
	var st State
	if s.getJSON(KeyState, &st) {
		r.MasterInst = st.Instance
		if st.Former != nil && m != 0 && st.Former.Id == m {
			r.SinceChange = st.Former.SinceChange
		}
	}
	return r
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
// pin 是凭交接标记 / 投票换主时新主机的签名公钥(钉住:它的第一份状态必须用这把验过);空 = 等这个纪元第一份状态时再记。
func (s *Service) adopt(e uint64, m uint, pin string) error {
	old := s.Epoch()
	wasDemoted := s.setting(KeyRole) == RoleDemoted
	more := map[string]string{"hubSnapshotSequence": "", KeyMasterPub: pin, KeyState: "", KeyVote: ""}
	if wasDemoted {
		more[KeyRole] = "" // 回来的原主机已经被新主机接上:是普通副机了(变为副机的记录留着)
	}
	var g Grant
	if s.getJSON(KeyGrant, &g) && g.Epoch <= e {
		more[KeyGrant] = "" // 交接标记用过了
	}
	err := s.setEpoch(e, m, more)
	if err == nil {
		s.notePinned(e, m, pin)
		logger.Info("容灾:本机切到纪元 ", e, "(原 ", old, "),主机是服务器 #", m)
		s.mu.Lock()
		s.lastContact = s.now()
		s.core().notListed = time.Time{}
		s.mu.Unlock()
		s.eng.adopted(e, m)
		if wasDemoted {
			s.addNotice(Notice{Kind: "joined", At: s.now().Unix()})
		}
	}
	return err
}

// ReceiveState 副机收下主机推来的容灾状态(POST agent/ha/state:h 是请求头,raw 是正文;纪元已经在 CheckAgent 里判过)。
// 只收签过名的:本纪元已经钉住主机签名公钥的,正文里的公钥必须是它、签名用它验;还没钉的(凭租约过期、本机纪元 0、
// 回来的原主机换的主)用正文里的公钥验过后钉住。同一纪元换公钥一律拒收。
// 凭交接标记 / 投票换主的:先用标记 / 票里新主机的公钥验过这份状态,才换到它的纪元。
func (s *Service) ReceiveState(h http.Header, raw []byte) error {
	var ss SignedState
	if err := json.Unmarshal(raw, &ss); err != nil || ss.V != StateV || len(ss.Body) == 0 || ss.Sig == "" {
		return errStateUnsigned
	}
	var st State
	if err := json.Unmarshal(ss.Body, &st); err != nil {
		return err
	}
	if e, m := Header(h); s.IsNode() && e > s.Epoch() {
		verdict, pin, _ := s.nodeVerdict(e, m, "ha/state")
		if !verdict.OK() || !verdict.Adopt || pin == "" {
			return fmt.Errorf("容灾状态的纪元 %d 与本机的 %d 不一致", e, s.Epoch())
		}
		if st.Epoch != e || st.MasterId != m || st.MasterPub != pin {
			return errStatePub
		}
		pub, err := ParseSignPub(pin)
		if err != nil {
			return err
		}
		if err := verifyState(pub, ss.Body, ss.Sig); err != nil {
			return err
		}
		if err := s.adopt(e, m, pin); err != nil {
			return err
		}
	}
	return s.applyState(st, ss.Body, ss.Sig)
}

// applyState 验过签名后收下容灾状态:交接标记只认当前主机在当前纪元签发、没过期的;撤销先于新的标记生效。
func (s *Service) applyState(st State, body []byte, sig string) error {
	L := s.Epoch()
	if st.Epoch != L {
		return fmt.Errorf("容灾状态的纪元 %d 与本机的 %d 不一致", st.Epoch, L)
	}
	if mid, ok := s.masterId(); ok && st.MasterId != mid {
		return fmt.Errorf("容灾状态来自服务器 #%d,不是本机认的主机 #%d", st.MasterId, mid)
	}
	pinned := strings.TrimSpace(s.setting(KeyMasterPub))
	if pinned != "" && st.MasterPub != pinned {
		return errStatePub
	}
	pub, err := ParseSignPub(st.MasterPub)
	if err != nil {
		return err
	}
	if err := verifyState(pub, body, sig); err != nil {
		return err
	}
	now := s.now()
	notify, key, handover := st.Notify, st.ClusterKey, st.Handover
	st.Notify, st.ClusterKey = "", ""
	values := map[string]string{KeyState: mustJSON(st), KeyNotify: notify, KeyPeerKey: key}
	if pinned == "" {
		values[KeyMasterPub] = st.MasterPub
		logger.Info("容灾:记下纪元 ", L, " 主机的签名公钥 ", Fingerprint(st.MasterPub))
	}
	var cur Grant
	hasCur := s.getJSON(KeyGrant, &cur)
	if hasCur && st.Revoke > 0 && cur.Epoch <= st.Revoke {
		values[KeyGrant] = "" // 主机撤销了没用上的交接标记(设为主机没做成、改为收回)
	}
	if h := handover; h != nil && h.Epoch > L && h.Epoch > st.Revoke && h.FromEpoch == L && h.From == st.MasterId &&
		h.To != 0 && h.Expires > now.Unix() {
		h.At = now.Unix()
		values[KeyGrant] = mustJSON(h)
		if !hasCur || cur.Epoch != h.Epoch || cur.To != h.To {
			logger.Info("容灾:收到交接标记,纪元 ", h.Epoch, " 的主机是服务器 #", h.To)
		}
	}
	if err := s.set(values); err != nil {
		return err
	}
	if pinned == "" {
		s.notePinned(L, st.MasterId, st.MasterPub)
	}
	if st.Replica != nil {
		s.noteReplicaMark(*st.Replica)
	}
	s.checkListed(st, now)
	return nil
}

// standbyDropGrace 备用名单整个是空的时候,连续多久才删手里的副本:主机自己断网恢复后的第一轮可能推一份空名单
// (还没拉到各台的报告)。名单里有别的备用、却没有本机(主机明确把本机换下来了),或主机关了容灾、明确让本机删(ha/drop、ha/leave)时立即删。
var standbyDropGrace = 10 * time.Minute

// checkListed 本机不在备用名单里:名单不空(换下来了)或主机关了容灾就删手里的副本;名单是空的,连续超过宽限期才删。
func (s *Service) checkListed(st State, now time.Time) {
	off := st.Mode == ModeOff
	c := s.core()
	s.mu.Lock() // notListed 归 Service.mu 管
	if !off && containsId(st.Standbys, st.Self) {
		c.notListed = time.Time{}
		s.mu.Unlock()
		return
	}
	if c.notListed.IsZero() {
		c.notListed = now
	}
	since := c.notListed
	s.mu.Unlock()
	if !off && len(st.Standbys) == 0 && now.Sub(since) < standbyDropGrace {
		return
	}
	if _, ok := s.replicaInfo(); !ok {
		return
	}
	if err := s.DropReplica(); err != nil {
		logger.Warning("删除本机副本失败: ", err)
	}
}

// NodeState 副机最近一次收到的容灾状态。
func (s *Service) NodeState() (State, bool) {
	var st State
	ok := s.getJSON(KeyState, &st)
	return st, ok
}

// rank 本机在备用名单里的名次(1 起;不是备用为 0)。
func (st State) rank() int {
	for i, id := range st.Standbys {
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
