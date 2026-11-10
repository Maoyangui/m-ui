package ha

import (
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/hub"
)

// View 面板要的全部容灾状态(GET /api/ha)。按本机角色只填相关的部分。
type View struct {
	Role       string   `json:"role"` // master / node / demoted / promoting
	Epoch      uint64   `json:"epoch"`
	SelfId     uint     `json:"selfId"`
	SelfName   string   `json:"selfName"`
	MasterId   uint     `json:"masterId,omitempty"`
	MasterName string   `json:"masterName,omitempty"`
	Version    string   `json:"version"`
	Mode       string   `json:"mode"`
	Frozen     int64    `json:"frozenUntil,omitempty"`
	Notices    []Notice `json:"notices"`
	PanelURL   string   `json:"panelUrl,omitempty"`

	// 主机
	Config     *Config              `json:"config,omitempty"`
	Standbys   []StandbyInfo        `json:"standbys,omitempty"`
	StandbyIds []uint               `json:"standbyIds"`
	Became     *Became              `json:"became,omitempty"`
	Former     *Former              `json:"former,omitempty"`
	Switch     *SwitchState         `json:"switch,omitempty"`
	ReplicaErr string               `json:"replicaErr,omitempty"`
	Replies    map[string]replyView `json:"replies,omitempty"`
	Ready      []Check              `json:"ready,omitempty"`

	// 副机
	Node    *NodeHA  `json:"node,omitempty"`
	Demoted *Demoted `json:"demoted,omitempty"`
}

// NodeHA 副机看到的容灾状态。
type NodeHA struct {
	LeaseAgo    int64          `json:"leaseAgo"` // 秒
	Lost        bool           `json:"lost"`     // 主机已经 60 秒以上没联系本机
	Rank        int            `json:"rank"`     // 本机是第几个备用,0 = 不是备用
	Standbys    []uint         `json:"standbys"`
	PauseUntil  int64          `json:"pauseUntil,omitempty"`
	Replica     *ReplicaInfo   `json:"replica,omitempty"`
	PubFP       string         `json:"pubFp,omitempty"`
	CanTakeover bool           `json:"canTakeover"`
	Why         string         `json:"why,omitempty"` // 不能接任的原因代码
	Prompt      *PromptView    `json:"prompt,omitempty"`
	Evidence    *EvidenceView  `json:"evidence,omitempty"`
	Incoming    *IncomingState `json:"incoming,omitempty"`
	Grant       *Grant         `json:"grant,omitempty"`
	Switch      *SwitchState   `json:"switch,omitempty"` // 本机是原主机:设为主机的结果(新面板地址)
}

// Check 就绪检查的一项(界面按 Code 翻译,不通过的给一句话修法)。
type Check struct {
	Code  string            `json:"code"`
	Level string            `json:"level"` // ok | warn | bad | info
	Args  map[string]string `json:"args,omitempty"`
}

// View 当前状态。full 为真时主机侧附上就绪检查(设置页要,服务器列表不要)。
func (s *Service) View(full bool) View {
	v := View{Role: s.Role(), Epoch: s.Epoch(), SelfId: s.SelfId(), SelfName: s.selfName(), Version: s.d.Version, Mode: s.Mode(),
		Notices: s.Notices(), PanelURL: s.panelURL(), StandbyIds: []uint{}}
	v.MasterId, _ = s.masterId()
	if f := s.freezeUntil(); f > s.now().Unix() && !s.IsNode() {
		v.Frozen = f
	}
	var d Demoted
	if s.getJSON(KeyDemoted, &d) {
		v.Demoted = &d
	}
	if !s.IsNode() {
		c := s.Config()
		v.Config = &c
		v.MasterId, v.MasterName = v.SelfId, v.SelfName
		var b Became
		if s.getJSON(KeyBecame, &b) {
			v.Became = &b
		}
		var f Former
		if s.getJSON(KeyFormer, &f) && f.Id != 0 {
			v.Former = &f
		}
		if sw := s.switchState(); sw.Id != "" {
			v.Switch = &sw
		}
		if v.Epoch > 0 && c.Mode != ModeOff {
			v.Standbys = s.rep.Standbys()
			v.StandbyIds = s.rep.standbyIds()
			v.Replies = s.replyViews()
			s.rep.mu.Lock()
			v.ReplicaErr = s.rep.lastErr
			s.rep.mu.Unlock()
		} else if full {
			v.Standbys, _ = s.rep.compute(c)
		}
		if full {
			v.Ready = s.readiness(c, v.Standbys)
		}
		return v
	}
	nv := &NodeHA{LeaseAgo: int64(s.LeaseAgo().Seconds()), Standbys: []uint{}}
	if st, ok := s.NodeState(); ok {
		v.MasterName, v.Mode, nv.Rank, nv.PauseUntil = st.MasterName, st.Mode, st.rank(), st.PauseUntil
		if st.Standbys != nil {
			nv.Standbys = st.Standbys
		}
	} else {
		v.Mode = ModeOff
		v.MasterName = s.nodeName(s.masterOfRecord())
	}
	nv.Lost = v.Epoch > 0 && s.LeaseAgo() >= SuspectAfter
	var g Grant
	if s.getJSON(KeyGrant, &g) {
		nv.Grant = &g
	}
	if ri, ok := s.replicaInfo(); ok {
		nv.Replica = &ri
	}
	if v.Epoch > 0 {
		if k, err := s.standbyKey(); err == nil {
			nv.PubFP = Fingerprint(k.PubString())
		}
	}
	nv.CanTakeover, nv.Why = s.canTakeover()
	nv.Prompt = s.eng.promptView()
	nv.Evidence = s.eng.evidenceView()
	nv.Incoming = s.incomingView()
	if sw := s.switchState(); sw.Id != "" {
		nv.Switch = &sw
	}
	v.Node = nv
	return v
}

// canTakeover 本机现在能不能在面板上点「接任为主机」,不能的话为什么(代码给界面翻译)。
func (s *Service) canTakeover() (bool, string) {
	st, ok := s.NodeState()
	switch {
	case !s.IsNode():
		return false, "master"
	case s.setting(KeyRole) == RolePromoting:
		return false, "promoting"
	case !ok || st.Mode == ModeOff:
		return false, "off"
	case st.rank() == 0:
		return false, "notStandby"
	}
	if _, ok := s.replicaInfo(); !ok {
		return false, "noReplica"
	}
	if s.LeaseAgo() < SuspectAfter {
		return false, "masterOnline"
	}
	if st.PauseUntil > s.now().Unix() {
		return false, "paused"
	}
	return true, ""
}

// readiness 就绪检查:逐项打勾,不通过的给一句话修法(界面按代码翻译)。
func (s *Service) readiness(c Config, infos []StandbyInfo) []Check {
	var out []Check
	add := func(code, level string, kv ...string) {
		ch := Check{Code: code, Level: level}
		if len(kv) > 1 {
			ch.Args = map[string]string{}
			for i := 0; i+1 < len(kv); i += 2 {
				ch.Args[kv[i]] = kv[i+1]
			}
		}
		out = append(out, ch)
	}
	eligible, plain := 0, []string{}
	var old []string
	for _, i := range infos {
		if i.Eligible {
			eligible++
			if i.Plain && (i.Rank > 0 || len(out) < c.Standbys) {
				plain = append(plain, i.Name)
			}
		}
		for _, r := range i.Reasons {
			if r == reasonVersion {
				old = append(old, i.Name)
			}
		}
	}
	switch {
	case eligible == 0:
		add("standbys", "bad")
	case eligible < c.Standbys:
		add("standbys", "warn", "n", itoa(eligible), "want", itoa(c.Standbys))
	default:
		add("standbys", "ok", "n", itoa(min(eligible, c.Standbys)))
	}
	if len(old) > 0 {
		add("versions", "warn", "names", strings.Join(old, "、"))
	} else {
		add("versions", "ok")
	}
	if c.Mode != ModeOff && s.Epoch() > 0 {
		synced, total := 0, 0
		for _, i := range infos {
			if i.Rank == 0 {
				continue
			}
			total++
			if i.Replica != nil && i.Replica.Epoch == s.Epoch() && i.Replica.Err == "" && i.Replica.VerifiedAt > 0 {
				synced++
			}
		}
		switch {
		case total == 0:
		case synced == total:
			add("replica", "ok")
		default:
			add("replica", "warn", "n", itoa(synced), "total", itoa(total))
		}
	}
	if len(plain) > 0 {
		add("https", "warn", "names", strings.Join(plain, "、"))
	} else if eligible > 0 {
		add("https", "ok")
	}
	if l := s.selfLink(); l.ApiUrl == "" {
		add("selfLink", "warn")
	} else {
		add("selfLink", "ok", "url", l.ApiUrl)
	}
	if h := s.subHost(); h == "" || isIPHost(h) {
		add("subDomain", "warn", "host", h)
	} else {
		add("subDomain", "ok", "host", h)
	}
	tg := strings.EqualFold(s.setting("tgEnabled"), "true") && s.setting("tgToken") != "" && s.setting("tgChatId") != ""
	if tg {
		add("telegram", "ok")
	} else {
		add("telegram", "warn")
	}
	if c.Mode == ModeAuto && s.clusterSize() < 3 && !c.Witness {
		add("auto", "bad")
	}
	peersOK, peersTotal := 0, 0
	for _, i := range infos {
		if i.Rank > 0 && i.Peers != nil && i.Peers.Total > 0 {
			peersTotal++
			if i.Peers.OK == i.Peers.Total {
				peersOK++
			}
		}
	}
	if peersTotal > 0 {
		if peersOK == peersTotal {
			add("peers", "ok")
		} else {
			add("peers", "warn")
		}
	}
	if c.Witness {
		add("witness", "ok")
	} else {
		add("witness", "info")
	}
	if c.DnsTokenSet && c.DnsNames != "" {
		add("dns", "ok", "names", c.DnsNames)
	} else {
		add("dns", "info")
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

// Report 本机随报告上报的容灾信息。没进过纪元也报版本、磁盘、角色:主机据此判断这台能不能被设为主机
// (老主机不认这个字段,忽略)。
func (s *Service) Report() *hub.HAReport {
	L := s.Epoch()
	rep := &hub.HAReport{Epoch: L, LeaseAgo: int64(s.LeaseAgo().Seconds()), DiskFree: s.diskFree(), Role: s.Role(), Version: s.d.Version}
	if L > 0 && s.IsNode() {
		if k, err := s.standbyKey(); err == nil {
			rep.Pub = k.PubString()
		}
		if ri, ok := s.replicaInfo(); ok {
			rep.Replica = &hub.HAReplica{Epoch: ri.Epoch, Seq: ri.Seq, At: ri.CreatedAt, Size: ri.Size, AsOf: ri.AsOf,
				VerifiedAt: ri.VerifiedAt, Err: ri.VerifyErr}
		}
		rep.Peers = s.eng.peerSummary()
	}
	return rep
}

// Summary 页面顶部、角色标签要的小结(随 /api/status 每次刷新带上)。
type Summary struct {
	Role        string   `json:"role"`
	Epoch       uint64   `json:"epoch,omitempty"`
	Mode        string   `json:"mode"`
	Frozen      int64    `json:"frozenUntil,omitempty"`
	FrozenText  string   `json:"frozenText,omitempty"`
	Lost        int64    `json:"lost,omitempty"` // 副机:主机失联多少秒了(没失联为 0)
	MasterName  string   `json:"masterName,omitempty"`
	Rank        int      `json:"rank,omitempty"`
	CanTakeover bool     `json:"canTakeover,omitempty"`
	Notices     []Notice `json:"notices"`
}

// Summary 当前小结。
func (s *Service) Summary() Summary {
	sm := Summary{Role: s.Role(), Epoch: s.Epoch(), Mode: s.Mode(), Notices: s.Notices()}
	if s.Frozen() {
		sm.Frozen, sm.FrozenText = s.freezeUntil(), s.FrozenText()
	}
	if s.IsNode() && sm.Epoch > 0 {
		if ago := s.LeaseAgo(); ago >= SuspectAfter {
			sm.Lost = int64(ago.Seconds())
		}
		if st, ok := s.NodeState(); ok {
			sm.MasterName, sm.Mode, sm.Rank = st.MasterName, st.Mode, st.rank()
		}
		sm.CanTakeover, _ = s.canTakeover()
	}
	return sm
}
