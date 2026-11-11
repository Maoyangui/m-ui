package ha

import (
	"net/url"
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
	SignFP     string               `json:"signFp,omitempty"`      // 本机(主机)签名公钥的指纹
	DropPend   []DropPendingView    `json:"dropPending,omitempty"` // 停用 / 删除的副机还没确认删掉副本
	// DNSAPIOverride 改 DNS 用的是测试地址(环境变量 M_UI_TEST_CF_API),不是 Cloudflare
	DNSAPIOverride string `json:"dnsApiOverride,omitempty"`

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

// Check 就绪检查的一项(界面按 ha.ready.<Code>.<Level> 翻译,不通过的给一句话修法)。
// 带名单的项:Names 是数组(界面按语言拼接),Args["names"] 是用 ", " 拼好的兜底。
type Check struct {
	Code  string            `json:"code"`
	Level string            `json:"level"` // ok | warn | bad | info
	Args  map[string]string `json:"args,omitempty"`
	Names []string          `json:"names,omitempty"`
}

// View 当前状态。full 为真时主机侧附上就绪检查(设置页要,服务器列表不要)。
func (s *Service) View(full bool) View {
	v := View{Role: s.Role(), Epoch: s.Epoch(), SelfId: s.SelfId(), SelfName: s.selfName(), Version: s.d.Version, Mode: s.Mode(),
		Notices: s.Notices(), PanelURL: s.panelURL(), StandbyIds: []uint{}, DNSAPIOverride: s.d.DNSAPI}
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
		v.SignFP = s.signFP()
		v.DropPend = s.DropPending()
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
	// 证据:先确认再接任模式下要有未过期的确认提示(同伴、见证、名次、暂停都已判过)且主机仍失联;
	// 自动模式由系统判定,不给按钮(人工用命令行 --force)
	if ok, why := s.takeoverVerdict(); !ok {
		return false, why
	}
	return true, ""
}

// signFP 本机签名公钥的指纹(没有签名密钥时为空;不在这里生成)。
func (s *Service) signFP() string {
	k, err := SignKey(s.setting(KeySignKey))
	if err != nil {
		return ""
	}
	return Fingerprint(PubOf(k))
}

// readiness 就绪检查:逐项打勾,不通过的给一句话修法(界面按代码翻译)。
func (s *Service) readiness(c Config, infos []StandbyInfo) []Check {
	var out []Check
	add := func(code, level string, names []string, kv ...string) {
		ch := Check{Code: code, Level: level, Names: names}
		if len(kv) > 1 || len(names) > 0 {
			ch.Args = map[string]string{}
			for i := 0; i+1 < len(kv); i += 2 {
				ch.Args[kv[i]] = kv[i+1]
			}
			if len(names) > 0 {
				ch.Args["names"] = strings.Join(names, ", ")
			}
		}
		out = append(out, ch)
	}
	L := s.Epoch()
	eligible := 0
	var plain, old, keep, plainOff, pubLater, portClash []string
	subPort := s.settingInt("subPort", 2056, 1, 65535)
	for _, i := range infos {
		if i.Eligible {
			eligible++
			if i.Plain && i.Rank > 0 {
				plain = append(plain, i.Name)
			}
		}
		for _, r := range i.Reasons {
			switch r {
			case reasonVersion:
				old = append(old, i.Name)
			case reasonVerKeep:
				keep = append(keep, i.Name)
			case reasonPlain:
				plainOff = append(plainOff, i.Name)
			}
		}
		if L == 0 && i.Eligible && i.PubFP == "" && i.PinnedFP == "" {
			pubLater = append(pubLater, i.Name)
		}
		if (i.Rank > 0 || i.Eligible) && ((i.WebPort > 0 && i.WebPort == subPort) || (i.ResellerPort > 0 && i.ResellerPort == subPort)) {
			portClash = append(portClash, i.Name)
		}
	}
	switch {
	case eligible == 0:
		add("standbys", "bad", nil)
	case eligible < c.Standbys:
		add("standbys", "warn", nil, "n", itoa(eligible), "want", itoa(c.Standbys))
	default:
		add("standbys", "ok", nil, "n", itoa(min(eligible, c.Standbys)))
	}
	if len(pubLater) > 0 { // 纪元 0:副机开启后才报备用公钥,不是问题
		add("pubLater", "info", pubLater)
	}
	if len(old) > 0 {
		add("versions", "warn", old)
	} else {
		add("versions", "ok", nil)
	}
	if len(keep) > 0 { // 先升了主机:这些备用保留名次与旧副本,升级后恢复
		add("versionsKeep", "warn", keep)
	}
	if c.Mode != ModeOff && L > 0 {
		synced, total := 0, 0
		var failing []string
		failErr := ""
		for _, i := range infos {
			if i.Rank == 0 {
				continue
			}
			total++
			if i.Replica != nil && i.Replica.Epoch == L && i.Replica.Err == "" && i.Replica.VerifiedAt > 0 {
				synced++
				continue
			}
			// 没就绪的,有原因就说原因:主机发送失败、备用自检失败
			why := ""
			if i.Ack != nil && i.Ack.Err != "" {
				why = i.Ack.Err
			} else if i.Replica != nil && i.Replica.Err != "" {
				why = i.Replica.Err
			}
			if why != "" {
				failing = append(failing, i.Name)
				if failErr == "" {
					failErr = why
				}
			}
		}
		s.rep.mu.Lock()
		buildErr := s.rep.lastErr
		s.rep.mu.Unlock()
		switch {
		case buildErr != "":
			add("replicaFail", "bad", nil, "err", buildErr)
		case len(failing) > 0:
			add("replicaFail", "warn", failing, "err", failErr)
		case total == 0:
		case synced == total:
			add("replica", "ok", nil)
		default:
			add("replica", "warn", nil, "n", itoa(synced), "total", itoa(total))
		}
	}
	if len(plain) > 0 {
		add("https", "warn", plain)
	} else if eligible > 0 {
		add("https", "ok", nil)
	}
	if len(plainOff) > 0 { // 明文链接的副机默认不当备用:要用它们,在下面打开开关(或改用 https)
		add("plainOptIn", "info", plainOff)
	}
	if len(portClash) > 0 { // 它当主机后面板 / 代理面板占着本机的订阅端口:订阅地址会变
		add("standbyPorts", "warn", portClash, "port", itoa(subPort))
	}
	if l := s.selfLink(); l.ApiUrl == "" {
		add("selfLink", "bad", nil)
	} else if u, err := url.Parse(l.ApiUrl); err == nil && isIPHost(u.Hostname()) && strings.TrimSpace(s.setting("webDomain")) != "" {
		// 面板域名会随主机迁移:原主机变副机后按 IP 连
		add("selfLinkIp", "info", nil, "url", l.ApiUrl)
	} else {
		add("selfLink", "ok", nil, "url", l.ApiUrl)
	}
	if h := s.subHost(); h == "" || isIPHost(h) {
		add("subDomain", "warn", nil, "host", h)
	} else {
		add("subDomain", "ok", nil, "host", h)
	}
	tg := strings.EqualFold(s.setting("tgEnabled"), "true") && s.setting("tgToken") != "" && s.setting("tgChatId") != ""
	if tg {
		add("telegram", "ok", nil)
	} else {
		add("telegram", "warn", nil)
	}
	if c.Mode == ModeAuto && s.clusterSize() < 3 && !c.Witness {
		add("auto", "bad", nil)
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
			add("peers", "ok", nil)
		} else {
			add("peers", "warn", nil)
		}
	}
	if c.Witness {
		add("witness", "ok", nil)
	} else {
		add("witness", "info", nil)
	}
	if c.DnsTokenSet && c.DnsNames != "" {
		names := strings.Split(c.DnsNames, ",")
		add("dns", "ok", names)
	} else {
		add("dns", "info", nil)
	}
	if s.d.DNSAPI != "" { // 改 DNS 用的是测试地址
		add("dnsOverride", "warn", nil, "url", s.d.DNSAPI)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

// Report 本机随报告上报的容灾信息。没进过纪元也报版本、磁盘、角色:主机据此判断这台能不能被设为主机
// (老主机不认这个字段,忽略)。
func (s *Service) Report() *hub.HAReport {
	L := s.Epoch()
	rep := &hub.HAReport{Epoch: L, LeaseAgo: int64(s.LeaseAgo().Seconds()), DiskFree: s.diskFree(), Role: s.Role(), Version: s.d.Version,
		ResellerPort: s.settingInt("resellerPort", 2054, 1, 65535),
		// 被设为主机 / 接任后会用的端口与代理面板地址:主机据此提前查端口冲突、在确认框里给新地址
		WebPort: s.settingInt("webPort", 2053, 1, 65535), SubPort: s.settingInt("subPort", 2056, 1, 65535),
		WarpPort: s.settingInt("warpPort", 0, 0, 65535), ResellerURL: s.resellerURL(),
		Instance: s.Instance(), SignFP: s.signFP()}
	if L > 0 && s.IsNode() {
		if k, err := s.standbyKey(); err == nil {
			rep.Pub = k.PubString()
		}
		if ri, ok := s.replicaInfo(); ok {
			rep.Replica = &hub.HAReplica{Epoch: ri.Epoch, Seq: ri.Seq, At: ri.CreatedAt, Size: ri.Size, AsOf: ri.AsOf,
				VerifiedAt: ri.VerifiedAt, Err: ri.VerifyErr}
		}
		rep.ReplicaBytes = s.replicaBytes()
		rep.Peers = s.eng.peerSummary()
	}
	return rep
}

// Summary 页面顶部、角色标签要的小结(随 /api/status 每次刷新带上)。
type Summary struct {
	Role       string `json:"role"`
	Epoch      uint64 `json:"epoch,omitempty"`
	Mode       string `json:"mode"`
	Frozen     int64  `json:"frozenUntil,omitempty"`
	FrozenText string `json:"frozenText,omitempty"`
	// FrozenTarget 冻结中:正被设为主机的那台(界面自己拼提示,不用 FrozenText 的中文)
	FrozenTarget string   `json:"frozenTarget,omitempty"`
	Lost         int64    `json:"lost,omitempty"` // 副机:主机失联多少秒了(没失联为 0)
	MasterName   string   `json:"masterName,omitempty"`
	Rank         int      `json:"rank,omitempty"`
	CanTakeover  bool     `json:"canTakeover,omitempty"`
	Notices      []Notice `json:"notices"`
}

// Summary 当前小结。
func (s *Service) Summary() Summary {
	sm := Summary{Role: s.Role(), Epoch: s.Epoch(), Mode: s.Mode(), Notices: s.Notices()}
	if s.Frozen() {
		sm.Frozen, sm.FrozenText = s.freezeUntil(), s.FrozenText()
		sw := s.switchState()
		sm.FrozenTarget = sw.TargetName
		if sm.FrozenTarget == "" && sw.Target != 0 {
			sm.FrozenTarget = "#" + strconv.FormatUint(uint64(sw.Target), 10)
		}
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
