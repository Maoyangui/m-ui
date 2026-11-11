package ha

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/selfupdate"
)

// 设为主机(计划内换主机),在当前主机上发起,对一台副机执行:
//
//	(纪元 0 的集群先进入纪元 1、推给全部副机)→ 冻结写操作并等在途的写做完 → 向目标要一次性公钥与它以后的签名公钥
//	(经现有的服务器链接:令牌鉴权 + 钉住的证书指纹;目标必须已经认了本机的签名公钥)→ 导出整库与主机身份 →
//	用一次性公钥加密、本机签名,推过去 → 目标验证、解开、组好新库暂存 → 给所有副机发交接标记(纪元 E+1 的主机是目标,
//	到冻结截止作废)→ 让目标提交:它重启、以纪元 E+1 接手 → 本机变为副机(保留自己的线路继续服务)、重启。
//
// 冻结时长按库大小定(15–60 分钟)。交接与提交只在冻结有效期内做(各留 2 分钟余量),冻结到期绝不再提交。
// 中途任何一步失败:通知目标丢掉暂存、撤销已发的交接标记、解冻,本机照旧是主机,不丢数据。
// 提交那一步的结论只有三种:已提交(变副机)/ 确认没提交(目标确认丢掉了暂存:提交与丢弃在目标上同一把锁,丢掉了就不可能再生效)
// / 不明 —— 不明时保持冻结(期间本机不收写,目标后来起来也不丢东西),每 10 秒问一次目标;到冻结截止或管理员点「放弃」,
// 先以更高的纪元隔离目标(副机改认本机的新纪元,目标之后以旧的新纪元起来会被拒、自动变为副机)再解冻。

// 设为主机的阶段。
const (
	StagePrepare = "prepare" // 向目标要一次性公钥
	StageExport  = "export"  // 导出整库、加密
	StageSend    = "send"    // 推给目标
	StageGrant   = "grant"   // 给副机发交接标记
	StageCommit  = "commit"  // 目标提交
	StageUnknown = "unknown" // 提交结果不明:保持冻结,继续问目标
	StageDone    = "done"
	StageFailed  = "failed"
	StageFenced  = "fenced" // 结果不明,已以更高纪元隔离目标、解冻(终态)
)

// 时限(测试里调短)。
var (
	switchFreezeMin   = 15 * time.Minute // 冻结最短
	switchFreezeMax   = 60 * time.Minute // 冻结最长
	switchCommitWait  = 90 * time.Second // 提交没回话时最多当场问目标多久,之后转为"不明"
	switchMargin      = 2 * time.Minute  // 交接、提交前冻结至少还要剩这么久(提交 30 秒 + 当场问 90 秒)
	switchSendMargin  = 3 * time.Minute  // 发送最晚在冻结截止前这么久结束
	switchTTLExtra    = 5 * time.Minute  // 目标暂存比冻结多留这么久
	switchProbeEvery  = 10 * time.Second // 结果不明时多久问一次目标
	holdAfterDeadline = 10 * time.Minute // 结果不明时写闸比冻结截止多关多久(隔离做完就解冻)
	writeDrainWait    = 10 * time.Second // 置冻结后最多等在途的写做完多久
)

// switchFreezeFor 按库大小定冻结时长:2×(60 秒 + 每 MB 2 秒)+ 5 分钟,夹在 15–60 分钟之间。
func switchFreezeFor(size int64) time.Duration {
	d := 2*(60*time.Second+time.Duration(size>>20)*2*time.Second) + 5*time.Minute
	return min(max(d, switchFreezeMin), switchFreezeMax)
}

// SwitchState 设为主机的进度与结果(本机设置 haSwitch:原主机变为副机、重启之后照样看得到)。
type SwitchState struct {
	Id         string `json:"id"`
	Target     uint   `json:"target"`
	TargetName string `json:"targetName"`
	Stage      string `json:"stage"`
	Err        string `json:"err,omitempty"`
	StartedAt  int64  `json:"startedAt"`
	UpdatedAt  int64  `json:"updatedAt"`
	DoneAt     int64  `json:"doneAt,omitempty"`
	Epoch      uint64 `json:"epoch"` // 目标接手后的纪元
	NewPanel   string `json:"newPanel,omitempty"`
	SubHost    string `json:"subHost,omitempty"`
	SubKept    bool   `json:"subKept,omitempty"`
	DnsAuto    bool   `json:"dnsAuto,omitempty"`
	TargetIP   string `json:"targetIp,omitempty"`
	Actor      string `json:"actor,omitempty"`
	Size       int64  `json:"size,omitempty"`
	// 契约 A0 新增
	Unknown     bool   `json:"unknown,omitempty"`     // 提交结果曾经不明
	FreezeUntil int64  `json:"freezeUntil,omitempty"` // 冻结截止(交接标记同时作废)
	NewReseller string `json:"newReseller,omitempty"` // 目标的代理面板地址
	// 进程重启后接着收尾要用的
	Nonce      string `json:"nonce,omitempty"`      // 这一次的编号(重启后照样能让目标丢掉暂存)
	Grant      *Grant `json:"grant,omitempty"`      // 发出去的交接标记
	GrantSent  bool   `json:"grantSent,omitempty"`  // 交接标记已经推出去过:失败收尾时要撤销
	FenceEpoch uint64 `json:"fenceEpoch,omitempty"` // 隔离后本机的纪元
	Backup     string `json:"backup,omitempty"`     // 变为副机后留的备份
}

// Active 还在进行中(含结果不明)。
func (st SwitchState) Active() bool {
	return st.Id != "" && st.Stage != StageDone && st.Stage != StageFailed && st.Stage != StageFenced
}

type switcher struct {
	s         *Service
	mu        sync.Mutex
	running   bool
	fencing   bool       // 正在以更高纪元隔离目标:这一轮状态要带撤销
	lastProbe time.Time  // 结果不明时上次问目标的时间
	lastFence time.Time  // 上次隔离没做成的时间(隔离要推全部副机:失败后隔一会儿再试)
	blockedId string     // 已经通知过"撤销确认不够、暂不隔离"的那次设为主机
	saveMu    sync.Mutex // 进度的写入:变为副机之后不再被别处的进度盖掉
	unkMu     sync.Mutex // 结果不明的收尾(定时问目标、放弃)同一时间只做一件
}

func (s *Service) switchState() SwitchState {
	var st SwitchState
	s.getJSON(KeySwitch, &st)
	return st
}

// saveSwitch 记进度(连同 extra 里的设置一起写)。本机已经在变为副机时不写:那边写的才是结果。
func (s *Service) saveSwitch(st SwitchState, extra map[string]string) bool {
	w := s.sw
	w.saveMu.Lock()
	defer w.saveMu.Unlock()
	s.mu.Lock()
	demoting := s.demoting
	s.mu.Unlock()
	if demoting {
		return false
	}
	st.UpdatedAt = s.now().Unix()
	values := map[string]string{KeySwitch: mustJSON(st)}
	for k, v := range extra {
		values[k] = v
	}
	if err := s.set(values); err != nil {
		logger.Warning("容灾:记录设为主机的进度失败: ", err)
		return false
	}
	return true
}

// grant 这一轮随容灾状态推出去的交接标记:交接那一步以后、结论出来以前,冻结截止前有效。
func (w *switcher) grant() *Grant {
	st := w.s.switchState()
	if st.Grant == nil {
		return nil
	}
	switch st.Stage {
	case StageGrant, StageCommit, StageUnknown:
	default:
		return nil
	}
	if st.Grant.Expires > 0 && w.s.now().Unix() >= st.Grant.Expires {
		return nil
	}
	g := *st.Grant
	return &g
}

// revoke 这一轮随容灾状态推出去的撤销:设为主机没做成(或正在隔离目标)而交接标记已经发过,副机清掉纪元不高于它的标记。
// 交接标记本身到冻结截止就作废,之后不必再带。
func (w *switcher) revoke() uint64 {
	st := w.s.switchState()
	if !st.GrantSent || st.Epoch == 0 {
		return 0
	}
	if st.FreezeUntil > 0 && w.s.now().Unix() > st.FreezeUntil+60 {
		return 0
	}
	w.mu.Lock()
	fencing := w.fencing
	w.mu.Unlock()
	if fencing || st.Stage == StageFailed || st.Stage == StageFenced {
		return st.Epoch
	}
	return 0
}

// PlanItem 设为主机前的一项检查结果(界面按 Code 翻译,Args 是要填进去的值,Names 是名单)。
type PlanItem struct {
	Code  string            `json:"code"`
	Args  map[string]string `json:"args,omitempty"`
	Names []string          `json:"names,omitempty"`
}

// SetMasterPlan 点「设为主机」时确认框里要讲清楚的事。
type SetMasterPlan struct {
	Target     uint       `json:"target"`
	TargetName string     `json:"targetName"`
	SelfName   string     `json:"selfName"`
	NewPanel   string     `json:"newPanel"`
	OldPanel   string     `json:"oldPanel"`
	SubHost    string     `json:"subHost"`
	SubKept    bool       `json:"subKept"`
	DnsAuto    bool       `json:"dnsAuto"`
	DnsNames   []string   `json:"dnsNames,omitempty"`
	TargetIP   string     `json:"targetIp,omitempty"`
	Blockers   []PlanItem `json:"blockers"`
	Warnings   []PlanItem `json:"warnings"`
	Size       int64      `json:"size"`
	Ok         bool       `json:"ok"`
	// 契约 A0 新增
	NewReseller string `json:"newReseller,omitempty"` // 目标的代理面板地址(换主机后代理们用它)
	PlainAck    bool   `json:"plainAck,omitempty"`    // 目标是明文 http:要管理员勾选确认(AllowPlain)才能做
	FreezeMin   int    `json:"freezeMin,omitempty"`   // 写操作最长暂停多少分钟
}

func item(code string, kv ...string) PlanItem {
	it := PlanItem{Code: code}
	if len(kv) > 1 {
		it.Args = map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			it.Args[kv[i]] = kv[i+1]
		}
	}
	return it
}

// PlanError 不能设为主机:带着检查结果(接口按 Code 返回,界面自己翻译)。
type PlanError struct{ Plan SetMasterPlan }

func (e *PlanError) Error() string {
	return fmt.Errorf("「%s」现在不能设为主机:%s", e.Plan.TargetName, planText(e.Plan.Blockers[0])).Error()
}

// dbSize 本机库的大小(含 WAL),估整库数据有多大。
func (s *Service) dbSize() int64 {
	var n int64
	for _, p := range []string{s.d.DBPath, s.d.DBPath + "-wal"} {
		if st, err := os.Stat(p); err == nil {
			n += st.Size()
		}
	}
	return n
}

// localDiskFree 本机数据目录所在盘的剩余字节(测试里换掉)。
var localDiskFree = func(s *Service) int64 { return s.diskFree() }

// localExportNeed 导出整库要的本机空间:先在本机写一份整库临时库(约 1.3 倍库大小)再写 zip、信封,至少 1.5 倍 + 64 MB。
func localExportNeed(size int64) int64 { return size + size/2 + (64 << 20) }

// targetPorts 目标自己会监听的服务端口(按它上报的;老版本不报的按服务端口清单)。0 = 不知道。
type targetPorts struct{ web, sub, reseller, warp int }

func nodeTargetPorts(n model.Node, ha *hub.HAReport, resellerOn bool) targetPorts {
	var tp targetPorts
	for _, sp := range nodeServicePorts(n) {
		switch sp.Label {
		case "面板":
			if tp.web == 0 {
				tp.web = sp.Port
			}
		case "订阅":
			if tp.sub == 0 {
				tp.sub = sp.Port
			}
		}
	}
	if ha != nil {
		if ha.WebPort > 0 {
			tp.web = ha.WebPort
		}
		if ha.SubPort > 0 {
			tp.sub = ha.SubPort
		}
		if resellerOn && ha.ResellerPort > 0 {
			tp.reseller = ha.ResellerPort
		}
		tp.warp = ha.WarpPort
	}
	return tp
}

// subPortClash 迁来的订阅端口 port 在新主机上被谁占着(和 applyIdentity 同一个规则):服务("panel" / "reseller" / "warp")
// 或线路名;都没有返回空。
func subPortClash(port int, tp targetPorts, lines []model.Line, target uint) (svc, line string) {
	switch {
	case port <= 0, port == tp.sub: // 和它自己的订阅端口相同:套用身份后什么都不变
		return "", ""
	case port == tp.web:
		return "panel", ""
	case port == tp.reseller:
		return "reseller", ""
	case port == tp.warp:
		return "warp", ""
	}
	for _, l := range lines {
		if l.Port == port && !render.UDPOnly(l.Protocol) && render.LineOnNode(l, target) {
			return "", l.Name
		}
	}
	return "", ""
}

// SetMasterPlan 检查能不能把服务器 id 设为主机,讲清楚会发生什么。
func (s *Service) SetMasterPlan(id uint) (SetMasterPlan, error) {
	p := SetMasterPlan{Target: id, SelfName: s.selfName(), OldPanel: s.panelURL(), Size: s.dbSize(), Blockers: []PlanItem{}, Warnings: []PlanItem{}}
	p.FreezeMin = int(switchFreezeFor(p.Size) / time.Minute)
	if s.IsNode() {
		return p, errors.New("本机是副机:到主机面板上操作")
	}
	var n model.Node
	if err := s.d.DB.Where("id = ?", id).First(&n).Error; err != nil {
		return p, errors.New("服务器不存在")
	}
	p.TargetName, p.NewPanel = n.Name, strings.TrimSpace(n.ApiUrl)
	block := func(code string, kv ...string) { p.Blockers = append(p.Blockers, item(code, kv...)) }
	warn := func(code string, kv ...string) { p.Warnings = append(p.Warnings, item(code, kv...)) }
	if n.IsLocal {
		block("self")
		return p, nil
	}
	if sw := s.switchState(); sw.Active() {
		block("busy")
	}
	if !n.Enabled {
		block("disabled")
	}
	if p.NewPanel == "" || n.Token == "" {
		block("noLink")
	}
	var st hub.NodeStatus
	ok := false
	if s.d.Hub != nil {
		st, ok = s.d.Hub.StatusOf(id)
	}
	now := time.Now().Unix() // 和 hub 记的"最近联系"同一个钟
	switch {
	case !ok || !st.OK || now-st.LastSeen > 60:
		block("offline")
	case !st.Synced:
		block("unsynced")
	}
	if ok && st.Version != "" && !s.versionOK(st.Version) {
		block("version", "v", st.Version, "need", maxVer(MinVersion, s.d.Version))
	} else if ok && st.OK && st.HA == nil && st.Version != "" {
		block("version", "v", st.Version, "need", maxVer(MinVersion, s.d.Version))
	}
	if ok && st.HA != nil && st.HA.DiskFree >= 0 && st.HA.DiskFree < 3*p.Size+(64<<20) {
		block("disk", "free", strconv.FormatInt(st.HA.DiskFree>>20, 10), "need", strconv.FormatInt((3*p.Size+(64<<20))>>20, 10))
	}
	// 本机:导出整库要先在本机写一份临时库,磁盘写满时主库也写不进去
	if free, need := localDiskFree(s), localExportNeed(p.Size); free >= 0 && free < need {
		block("localDisk", "free", strconv.FormatInt(free>>20, 10), "need", strconv.FormatInt(need>>20, 10))
	}
	// 原主机变为副机后新主机怎么连它:拼不出地址,换完以后就没人管它了
	if l := s.selfLink(); l.ApiUrl == "" {
		block("noSelfLink")
	}
	// 明文 http:链路上的主动中间人能换掉一次性公钥、拿到并改写整库 —— 要管理员明确勾选
	if strings.HasPrefix(strings.ToLower(p.NewPanel), "http://") {
		p.PlainAck = true
		warn("plainAck")
	}
	// 订阅地址:域名的,DNS 指过去以后用户的订阅地址不变;IP 的一定会变
	p.SubHost = s.subHost()
	subPort := s.settingInt("subPort", 2056, 1, 65535)
	p.SubKept = p.SubHost != "" && !isIPHost(p.SubHost)
	if p.SubHost != "" && isIPHost(p.SubHost) {
		warn("subIp", "host", p.SubHost)
	}
	var lines []model.Line
	s.d.DB.Where("enabled = ?", true).Order("sort asc, id asc").Find(&lines)
	resellerOn := !strings.EqualFold(s.setting("resellerEnabled"), "false")
	var rep *hub.HAReport
	if ok {
		rep = st.HA
	}
	tp := nodeTargetPorts(n, rep, resellerOn)
	// 迁来的订阅端口在目标上被占着(它的面板 / 代理面板 / WARP / 线路):目标会继续用它自己的订阅端口,订阅地址会变
	own := strconv.Itoa(tp.sub)
	if tp.sub == 0 {
		own = "?"
	}
	if svc, line := subPortClash(subPort, tp, lines, id); svc != "" {
		warn("subPortSvc", "port", strconv.Itoa(subPort), "svc", svc, "own", own)
		p.SubKept = false
	} else if line != "" {
		warn("subPort", "port", strconv.Itoa(subPort), "line", line, "own", own)
		p.SubKept = false
	}
	// 代理面板:副机上不监听,成为主机后才起;端口被它的线路占着就起不来
	if resellerOn && tp.reseller > 0 {
		for _, l := range lines {
			if l.Port == tp.reseller && !render.UDPOnly(l.Protocol) && render.LineOnNode(l, id) {
				warn("resellerPort", "port", strconv.Itoa(tp.reseller), "line", l.Name)
				break
			}
		}
	}
	if resellerOn && rep != nil {
		p.NewReseller = rep.ResellerURL
	}
	// 版本旧的副机:照常同步,只是不认纪元
	var old []string
	for _, rn := range s.remoteNodes() {
		if rn.Id == id {
			continue
		}
		if rs, ok := s.d.Hub.StatusOf(rn.Id); ok && rs.Version != "" && !s.versionOK(rs.Version) {
			old = append(old, rn.Name)
		}
	}
	if len(old) > 0 {
		sort.Strings(old)
		it := item("oldNodes", "names", strings.Join(old, ", "))
		it.Names = old
		p.Warnings = append(p.Warnings, it)
	}
	// DNS:新主机改的是它自己探测到的公网 IP(publicIp,纯 IPv6 时 publicIp6);订阅主机名在名单里才会自动改
	cfg := s.Config()
	p.DnsNames = splitNames(cfg.DnsNames)
	p.DnsAuto = cfg.DnsTokenSet && p.SubKept && containsName(p.DnsNames, p.SubHost)
	p.TargetIP = n.PublicIP
	if p.TargetIP == "" {
		p.TargetIP = n.PublicIP6
	}
	if a := strings.TrimSpace(n.Addr); a != "" && net.ParseIP(a) != nil && !sameIP(a, p.TargetIP) {
		warn("dnsAddr", "ip", p.TargetIP, "addr", a)
	}
	p.Ok = len(p.Blockers) == 0
	return p, nil
}

func containsName(names []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, n := range names {
		if strings.EqualFold(n, host) {
			return true
		}
	}
	return false
}

func nodeServicePorts(n model.Node) []model.ServicePort {
	var out []model.ServicePort
	if len(n.ServicePorts) > 0 {
		_ = json.Unmarshal(n.ServicePorts, &out)
	}
	return out
}

func maxVer(a, b string) string {
	if b != "" && b != "dev" && selfupdate.Newer(b, a) {
		return b
	}
	return a
}

func splitNames(raw string) []string {
	var out []string
	for _, f := range strings.Split(raw, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// SetMasterOpts 设为主机的选项。AllowPlain:目标是明文 http 时管理员已勾选确认风险。
type SetMasterOpts struct{ AllowPlain bool }

// SetMaster 把服务器 id 设为主机(在后台进行,进度看 SwitchState)。actor 记进审计与通知。
func (s *Service) SetMaster(id uint, actor string, o SetMasterOpts) (SwitchState, error) {
	if !s.promoteMu.TryLock() {
		return SwitchState{}, errTakeoverBusy
	}
	plan, err := s.SetMasterPlan(id)
	if err != nil {
		s.promoteMu.Unlock()
		return SwitchState{}, err
	}
	if !plan.Ok {
		s.promoteMu.Unlock()
		return SwitchState{}, &PlanError{Plan: plan}
	}
	if plan.PlainAck && !o.AllowPlain {
		s.promoteMu.Unlock()
		return SwitchState{}, errPlainNotAcked
	}
	st := SwitchState{Id: randID(), Target: id, TargetName: plan.TargetName, Stage: StagePrepare, StartedAt: s.now().Unix(),
		NewPanel: plan.NewPanel, SubHost: plan.SubHost, SubKept: plan.SubKept, DnsAuto: plan.DnsAuto,
		TargetIP: plan.TargetIP, Actor: actor, Size: plan.Size, NewReseller: plan.NewReseller}
	if !s.saveSwitch(st, nil) {
		s.promoteMu.Unlock()
		return st, errors.New("记录设为主机的进度失败")
	}
	s.sw.mu.Lock()
	s.sw.running = true
	s.sw.mu.Unlock()
	logger.Warning("容灾:开始把主机换到 ", plan.TargetName, "(", actor, " 发起)")
	go func() {
		defer s.promoteMu.Unlock()
		defer func() {
			s.sw.mu.Lock()
			s.sw.running = false
			s.sw.mu.Unlock()
		}()
		s.sw.run(st)
	}()
	return st, nil
}

var (
	errPlainNotAcked = errors.New("目标机器的 API 地址是明文 http:链路上的中间人能拿到并改写整库。确认风险后勾选「仍然继续」再做")
	errFreezeShort   = errors.New("写操作的冻结快到期了:为保证数据不丢,这次不交接、不提交")
	errTargetNotPin  = errors.New("目标机器还没认本机的签名公钥(没收到本机的容灾状态):等一两分钟它同步上以后再试")
)

// planText 一项检查的中文说明(命令行、错误里用;界面按 Code 自己翻译)。
func planText(it PlanItem) string {
	a := it.Args
	switch it.Code {
	case "self":
		return "它已经是主机"
	case "busy":
		return "正在进行别的换主机"
	case "disabled":
		return "它已停用"
	case "noLink":
		return "没有填 API 地址或令牌"
	case "offline":
		return "它离线"
	case "unsynced":
		return "它的配置还没同步上"
	case "version":
		return "它的版本 v" + a["v"] + " 太旧,先升级到 v" + a["need"] + " 或更新"
	case "disk":
		return "它的磁盘剩余 " + a["free"] + " MB,不够(至少要 " + a["need"] + " MB)"
	case "localDisk":
		return "本机磁盘剩余 " + a["free"] + " MB,不够导出整库(至少要 " + a["need"] + " MB)"
	case "noSelfLink":
		return "本机没有面板域名也没探测到公网 IP,变为副机后新主机连不上本机"
	}
	return it.Code
}

// run 设为主机的各步(在后台)。
func (w *switcher) run(st SwitchState) {
	s := w.s
	var n model.Node
	if err := s.d.DB.Where("id = ?", st.Target).First(&n).Error; err != nil {
		w.fail(st, StagePrepare, errors.New("服务器不存在"))
		return
	}
	// 0. 没进过纪元的集群先进入纪元 1、推给全部副机:目标要先认本机的签名公钥,之后的交接、提交结果不明时的判定都靠纪元
	if s.Epoch() == 0 {
		if _, err := s.EnterEpoch(); err != nil {
			w.fail(st, StagePrepare, err)
			return
		}
		got := s.pushAll(s.remoteNodes(), 15*time.Second)
		logger.Info("容灾:设为主机前进入纪元 1,已推给 ", got, " 台副机")
	}
	L := s.Epoch()
	next, err := NextEpoch(L)
	if err != nil {
		w.fail(st, StagePrepare, err)
		return
	}
	own, err := s.SignPub()
	if err != nil {
		w.fail(st, StagePrepare, err)
		return
	}
	st.Epoch = next
	// 1. 冻结写操作(按库大小定时长),等已经进来的写做完
	freeze := switchFreezeFor(st.Size)
	st.FreezeUntil = s.now().Add(freeze).Unix()
	if err := s.freezeAndDrain(st); err != nil {
		w.fail(st, StagePrepare, err)
		return
	}
	logger.Warning("容灾:设为主机,写操作暂停到 ", time.Unix(st.FreezeUntil, 0).In(s.loc()).Format("15:04:05"))
	// 2. 向目标要一次性公钥、它以后的签名公钥;它要已经认了本机(纪元、签名公钥)
	var prep PrepareReply
	req, _ := json.Marshal(PrepareRequest{Master: s.selfLink(), Version: s.d.Version, Size: st.Size, TTL: int64((freeze + switchTTLExtra) / time.Second)})
	if err := s.d.Hub.Send(n, http.MethodPost, "ha/prepare", bytes.NewReader(req), int64(len(req)), nil, 30*time.Second, &prep); err != nil {
		w.fail(st, StagePrepare, err)
		return
	}
	st.Nonce = prep.Nonce
	pub, err := ParsePub(prep.Pub)
	if _, perr := ParseSignPub(prep.SignPub); err != nil || perr != nil || prep.Nonce == "" {
		_ = w.abort(n, prep.Nonce)
		w.fail(st, StagePrepare, errors.New("目标机器回的一次性公钥不对"))
		return
	}
	if prep.MasterReach != "" {
		_ = w.abort(n, prep.Nonce)
		w.fail(st, StagePrepare, fmt.Errorf("目标机器连不上本机的面板地址 %s(%s):换完以后新主机管不了本机。先让它能连上(放行面板端口)再试", s.selfLink().ApiUrl, prep.MasterReach))
		return
	}
	if prep.Epoch != L || prep.MasterPubFP != Fingerprint(own) {
		_ = w.abort(n, prep.Nonce)
		w.fail(st, StagePrepare, errTargetNotPin)
		return
	}
	// 3. 导出整库、加密
	st.Stage = StageExport
	s.saveSwitch(st, nil)
	if err := ensureDir(s.Dir()); err != nil {
		_ = w.abort(n, prep.Nonce)
		w.fail(st, StageExport, err)
		return
	}
	dir, err := os.MkdirTemp(s.Dir(), "switch-")
	if err != nil {
		_ = w.abort(n, prep.Nonce)
		w.fail(st, StageExport, err)
		return
	}
	defer os.RemoveAll(dir)
	envPath, size, err := s.sealBundle(dir, pub, prep.Nonce)
	if err != nil {
		_ = w.abort(n, prep.Nonce)
		w.fail(st, StageExport, err)
		return
	}
	st.Size = size
	// 4. 推给目标:最晚在冻结截止前 3 分钟结束
	st.Stage = StageSend
	s.saveSwitch(st, nil)
	limit := time.Unix(st.FreezeUntil, 0).Add(-switchSendMargin).Sub(s.now())
	if limit <= 0 {
		_ = w.abort(n, prep.Nonce)
		w.fail(st, StageSend, errFreezeShort)
		return
	}
	if err := s.sendBundle(n, envPath, size, limit); err != nil {
		_ = w.abort(n, prep.Nonce)
		w.fail(st, StageSend, err)
		return
	}
	// 5. 冻结还够 → 给副机发交接标记:纪元 E+1 的主机是目标(带它以后的签名公钥,冻结截止作废)
	if !w.inTime(st) {
		_ = w.abort(n, prep.Nonce)
		w.fail(st, StageGrant, errFreezeShort)
		return
	}
	st.Stage = StageGrant
	st.Grant = &Grant{Epoch: next, To: st.Target, ToPub: prep.SignPub, From: s.SelfId(), FromEpoch: L, Expires: st.FreezeUntil, At: s.now().Unix()}
	st.GrantSent = true
	s.saveSwitch(st, nil)
	s.invalidate()
	got := s.pushAll(s.remoteNodes(), 15*time.Second)
	logger.Info("容灾:交接标记已发给 ", got, " 台副机")
	// 6. 冻结还够 → 目标提交
	if !w.inTime(st) {
		_ = w.abort(n, prep.Nonce)
		w.fail(st, StageGrant, errFreezeShort)
		return
	}
	st.Stage = StageCommit
	// 提交请求发出之后、结论明确之前写闸一直关着:当场追问可能越过冻结截止,写闸不能在那时自己打开(有结论时 fail / becomeNode 清掉)
	if !s.saveSwitch(st, map[string]string{KeyFreezeUntil: strconv.FormatInt(st.FreezeUntil+int64(holdAfterDeadline/time.Second), 10)}) {
		_ = w.abort(n, prep.Nonce)
		w.fail(st, StageCommit, errors.New("记录设为主机的进度失败"))
		return
	}
	b, _ := json.Marshal(map[string]string{"nonce": prep.Nonce})
	if err := s.d.Hub.Send(n, http.MethodPost, "ha/commit", bytes.NewReader(b), int64(len(b)), nil, 30*time.Second, nil); err != nil {
		limit := min(switchCommitWait, time.Unix(st.FreezeUntil, 0).Sub(s.now())) // 当场追问不越过冻结截止:之后交给定时的问询与隔离
		switch w.waitCommitted(n, st, limit) {
		case commitNot:
			w.fail(st, StageCommit, fmt.Errorf("目标没有接手: %w", err))
			return
		case commitUnknown:
			w.enterUnknown(st, err)
			return
		}
	}
	// 7. 本机变为副机
	s.becomeNode(st)
}

// inTime 冻结还够不够做交接 / 提交(各留 2 分钟余量);冻结到期以后绝不再提交。
func (w *switcher) inTime(st SwitchState) bool {
	return w.s.now().Add(switchMargin).Unix() < st.FreezeUntil
}

// freezeAndDrain 置冻结(和写闸同一把锁:之后进来的写一律被挡),再等已经进来的写做完(最多 writeDrainWait)。
// 等不到就返回错误(调用方失败收尾、解冻)。
func (s *Service) freezeAndDrain(st SwitchState) error {
	s.gate.mu.Lock()
	ok := s.saveSwitch(st, map[string]string{KeyFreezeUntil: strconv.FormatInt(st.FreezeUntil, 10)})
	s.gate.mu.Unlock()
	if !ok {
		return errors.New("记录设为主机的进度失败")
	}
	s.invalidate() // 冻结期间备用不得接任:随状态推出去
	deadline := time.Now().Add(writeDrainWait)
	for {
		s.gate.mu.Lock()
		n := s.gate.inflight
		s.gate.mu.Unlock()
		if n == 0 {
			return nil
		}
		if time.Now().After(deadline) || s.stopping() {
			return fmt.Errorf("还有 %d 个写操作没做完(批量导入、还原之类):等它们做完再试", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// abort 让目标丢掉暂存。目标确认了(200)返回 nil:提交与丢弃在目标上同一把锁,丢掉以后这一次就不可能再生效。
func (w *switcher) abort(n model.Node, nonce string) error {
	if nonce == "" {
		return errors.New("编号对不上")
	}
	b, _ := json.Marshal(map[string]string{"nonce": nonce})
	return w.s.d.Hub.Send(n, http.MethodPost, "ha/abort", bytes.NewReader(b), int64(len(b)), nil, 15*time.Second, nil)
}

// fail 设为主机没做成:撤销已发的交接标记、解冻,本机照旧是主机。只在确定目标不会接手时调用。
func (w *switcher) fail(st SwitchState, stage string, err error) {
	s := w.s
	logger.Warning("容灾:设为主机在「", stage, "」这一步失败: ", err)
	st.Stage, st.Err = StageFailed, err.Error()
	// 失败与解冻一起写:外面看到「没有完成」时写操作已经恢复
	if !s.saveSwitch(st, map[string]string{KeyFreezeUntil: ""}) {
		return
	}
	s.invalidate()
	if st.GrantSent { // 已经发过交接标记:马上推一轮带撤销的状态,不等 hub 的下一轮
		got := s.pushAll(s.remoteNodes(), 10*time.Second)
		logger.Info("容灾:已向 ", got, " 台副机撤销交接标记")
	}
	s.addNotice(Notice{Kind: "switchFailed", Name: st.TargetName, Warn: []string{err.Error()}})
	s.audit("ha", "set-master-failed", map[string]string{"target": st.TargetName, "stage": stage, "err": err.Error()})
	s.notify("🔴 <b>设为主机没有完成</b>:" + notify.Esc(st.TargetName) + "\n" + notify.Esc(err.Error()) + "\n本机照旧是主机,写操作已恢复,数据没有变化。")
}

// 提交没回话时问目标的结论。
type commitResult int

const (
	commitUnknown commitResult = iota // 问不到,或说不清
	commitDone                        // 目标已经提交(或已经以新纪元当了主机)
	commitNot                         // 确认没提交:目标丢掉了暂存
)

// probeCommit 问一次目标:它已经以新纪元当了主机(本机的请求收到 409 stale,纪元不低于 E+1、主机就是它),
// 或说已经提交 → 已提交;说还没提交(暂存着、或什么都没有)就让它丢掉,它确认了 → 确认没提交;其余 → 不明。
func (w *switcher) probeCommit(n model.Node, st SwitchState) commitResult {
	s := w.s
	var out struct {
		Stage string `json:"stage"`
	}
	err := s.d.Hub.Send(n, http.MethodGet, "ha/incoming", nil, 0, nil, 8*time.Second, &out)
	var ee *hub.EpochError
	switch {
	case errors.As(err, &ee):
		if ee.Code == CodeStale && ee.Epoch >= st.Epoch && ee.MasterId == st.Target {
			return commitDone
		}
		return commitUnknown
	case err != nil:
		return commitUnknown
	case out.Stage == incCommitted:
		return commitDone
	}
	if w.abort(n, st.Nonce) == nil {
		return commitNot
	}
	return commitUnknown
}

// waitCommitted 提交请求没回话时当场问目标,最多 limit(至少问一次)。
func (w *switcher) waitCommitted(n model.Node, st SwitchState, limit time.Duration) commitResult {
	deadline := time.Now().Add(limit) // 等的是网络:按真实时间
	for {
		if r := w.probeCommit(n, st); r != commitUnknown {
			return r
		}
		if time.Now().After(deadline) || w.s.stopping() {
			return commitUnknown
		}
		time.Sleep(max(min(3*time.Second, limit/4), 10*time.Millisecond))
	}
}

// enterUnknown 提交结果不明:保持冻结与交接标记,之后每 10 秒问一次目标(tick);冻结到期或管理员放弃时隔离目标。
func (w *switcher) enterUnknown(st SwitchState, cause error) {
	s := w.s
	st.Stage, st.Unknown = StageUnknown, true
	st.Err = "目标没有回话,不确定它接手没有:写操作保持暂停,继续问它;冻结到期仍不明,本机以更高的纪元隔离它后恢复写操作"
	// 写闸一直关到有结论(隔离成功才解冻):冻结截止之后、隔离做完之前也不能收写
	if !s.saveSwitch(st, map[string]string{KeyFreezeUntil: strconv.FormatInt(st.FreezeUntil+int64(holdAfterDeadline/time.Second), 10)}) {
		return
	}
	logger.Warning("容灾:设为主机提交结果不明: ", cause)
	s.audit("ha", "set-master-unknown", map[string]interface{}{"target": st.TargetName, "err": cause.Error(), "freezeUntil": st.FreezeUntil})
	s.notify(fmt.Sprintf("🟠 <b>设为主机结果不明</b>:提交「%s」时没有回话,也问不到它。\n写操作保持暂停,最晚到 %s;这期间它起来了就照常完成换主机。到时仍不明,本机以更高的纪元隔离它后恢复写操作(它之后起来会被副机拒绝并自动变为副机,它那份数据不会生效)。也可以在面板上点「放弃」立刻这样做。",
		notify.Esc(st.TargetName), time.Unix(st.FreezeUntil, 0).In(s.loc()).Format("2006-01-02 15:04:05")))
}

// becomeNode 设为主机完成:本机变为副机(保留自己的线路继续服务)。先把角色写好(期间收到的 stale 不会再走"失联被接任"),
// 再留一份备份,发提示与通知,重启。只做一次。
func (s *Service) becomeNode(st SwitchState) {
	w := s.sw
	w.saveMu.Lock()
	if !s.beginDemote() {
		w.saveMu.Unlock()
		return
	}
	now := s.now()
	st.Stage, st.DoneAt, st.Err = StageDone, now.Unix(), ""
	g := Grant{Epoch: st.Epoch, To: st.Target, From: s.SelfId(), At: now.Unix(), Expires: now.Add(time.Hour).Unix()}
	if st.Grant != nil {
		g.ToPub, g.FromEpoch = st.Grant.ToPub, st.Grant.FromEpoch
	}
	st.UpdatedAt = now.Unix()
	values := map[string]string{
		"nodeMode": "true", KeyRole: "", KeyMasterId: strconv.FormatUint(uint64(st.Target), 10),
		KeyGrant:           mustJSON(g),
		hub.LedgerEpochKey: hub.NewLedgerEpoch(), // 本机的流量账本换一本:新主机只建基线
		KeySignKey:         "", KeyFreezeUntil: "", KeyFormer: "", KeyStandbyList: "",
		KeySwitch: mustJSON(st),
	}
	err := s.set(values)
	w.saveMu.Unlock()
	if err != nil {
		logger.Warning("容灾:变为副机失败: ", err)
		s.mu.Lock()
		s.demoting = false
		s.mu.Unlock()
		return
	}
	backup := ""
	if s.d.Backup != nil {
		if name, err := s.d.Backup("before-set-master-"); err == nil {
			backup = name
			st.Backup = name
			_ = s.set(map[string]string{KeySwitch: mustJSON(st)})
		} else {
			logger.Warning("容灾:变为副机后的备份失败(继续): ", err)
		}
	}
	dnsState := ""
	if st.SubKept && !st.DnsAuto {
		dnsState = "todo" // 要管理员自己改 DNS
	}
	s.addNotice(Notice{Kind: "switched", Reason: "switched", Name: st.TargetName, URL: st.NewPanel, Host: st.SubHost, IP: st.TargetIP,
		Dns: st.DnsAuto, SubKept: st.SubKept, DnsState: dnsState, Reseller: st.NewReseller, File: backup})
	s.audit("ha", "became-node", map[string]interface{}{"to": st.TargetName, "epoch": st.Epoch, "by": st.Actor, "backup": backup})
	logger.Warning("容灾:主机已换到 ", st.TargetName, ",本机变为副机;新面板 ", st.NewPanel)
	msg := fmt.Sprintf("✅ <b>主机已换到「%s」</b>,本机「%s」已变为副机,继续用自己的线路服务。\n新面板:%s(账号、密码、两步验证不变,需要重新登录)",
		notify.Esc(st.TargetName), notify.Esc(s.selfName()), notify.Esc(st.NewPanel))
	if st.NewReseller != "" {
		msg += "\n代理面板:" + notify.Esc(st.NewReseller)
	}
	switch {
	case st.SubKept && st.DnsAuto:
		msg += "\nDNS:新主机起来后会自动把 " + notify.Esc(st.SubHost) + " 改指向它,用户的订阅地址不变。"
	case st.SubKept:
		msg += fmt.Sprintf("\nDNS:把 %s 改指向新主机的 IP %s,用户的订阅地址就不变。", notify.Esc(st.SubHost), notify.Esc(st.TargetIP))
	default:
		msg += "\n用户的订阅地址会变:新主机起来后在它的面板上看新的订阅地址,让用户重新导入。"
	}
	s.notify(msg)
	s.restart(3 * time.Second)
}

// switchTargetUp 降级判定看到设为主机的目标已以新纪元起来(提交那一步结果不明、或还在当场问):按"换主机完成"收尾,
// 而不是"失联期间被接任"。返回是否接手了这次判定。
// 冻结截止之后也认:结果不明期间写闸一直关着(隔离做成才解冻),目标以新纪元起来只可能是提交生效了,跟它走不丢写。
// 正在问目标 / 隔离时(持 unkMu)不在这里收尾:那边自己看证据,免得一边升纪元一边变为副机。
func (s *Service) switchTargetUp(target uint, epoch uint64) bool {
	w := s.sw
	if !w.unkMu.TryLock() {
		return false
	}
	defer w.unkMu.Unlock()
	st := s.switchState()
	if st.Target == 0 || st.Target != target || epoch < st.Epoch {
		return false
	}
	switch st.Stage {
	case StageCommit, StageUnknown:
	default:
		return false
	}
	logger.Info("容灾:设为主机的目标已以纪元 ", epoch, " 起来,按换主机完成收尾")
	s.becomeNode(st)
	return true
}

// AbandonSwitch 管理员放弃结果不明的设为主机:以更高的纪元隔离目标,然后恢复写操作。
func (s *Service) AbandonSwitch(actor string) error {
	w := s.sw
	w.unkMu.Lock()
	defer w.unkMu.Unlock()
	st := s.switchState()
	if st.Stage != StageUnknown || w.isRunning() {
		return errors.New("没有结果不明的设为主机可以放弃")
	}
	return w.fence(st, actor)
}

// fence 以更高纪元隔离目标(FenceAbandon:撤销交接标记、给本机发自交接、本机纪元再升),成功后解冻。
// 隔离之前先看目标是不是其实已经接手了:当场问它一次,再给其余副机推一轮状态拿到它们最新的回复 ——
// 目标自己说已是主机、或有副机正被它以新纪元推着,就按换主机完成收尾(冻结期间没收写,不丢数据),不隔离。
// 撤销要有足够的副机确认收到才升纪元、解冻:冻结截止前(管理员放弃)要目标以外的每一台都确认 —— 没收到撤销的副机手里
// 的交接标记到截止前一直有效,目标那时起来它们会跟它;截止后交接标记都已作废,要目标以外的副机连同本机过半。
// 不够就保持冻结、记下原因,下一轮再试(结果不明时不能恢复写)。
func (w *switcher) fence(st SwitchState, actor string) error {
	s := w.s
	var tn model.Node
	if s.d.Hub != nil && s.d.DB.Where("id = ?", st.Target).Limit(1).Find(&tn).Error == nil && tn.Id != 0 {
		switch w.probeCommit(tn, st) {
		case commitDone:
			s.becomeNode(st)
			return nil
		case commitNot:
			w.fail(st, StageCommit, errors.New("目标确认没有接手(已丢掉暂存的数据)"))
			return nil
		}
	}
	since := time.Unix(st.StartedAt, 0)
	others := s.nodesExcept(st.Target)
	before := s.now()
	got := s.pushAll(others, 15*time.Second) // 回复经 NoteReply 记下
	if s.leaving() {
		return nil // 推的回复里看到目标已是主机,已经按换主机完成收尾
	}
	if s.targetUpSeen(st.Target, st.Epoch, since) {
		s.becomeNode(st)
		return nil
	}
	voters := len(others) - s.legacyAmong(others, before)
	need := (voters + 1) / 2
	if s.now().Unix() < st.FreezeUntil {
		need = voters
	}
	if got < need {
		return w.fenceBlocked(st, got, need)
	}
	w.mu.Lock()
	w.fencing = true
	w.mu.Unlock()
	e, err := s.FenceAbandon(st.Target, st.Epoch, func(granted int) error {
		if s.targetUpSeen(st.Target, st.Epoch, since) {
			return errTargetUp
		}
		if granted < need {
			return fenceShort(granted, need)
		}
		return nil
	})
	w.mu.Lock()
	w.fencing = false
	w.mu.Unlock()
	if errors.Is(err, errTargetUp) {
		s.becomeNode(st)
		return nil
	}
	if err != nil {
		if s.leaving() {
			return nil
		}
		var short *fenceShortError
		if errors.As(err, &short) {
			return w.fenceBlocked(st, short.got, short.need)
		}
		w.mu.Lock()
		w.lastFence = time.Now()
		w.mu.Unlock()
		logger.Warning("容灾:隔离设为主机的目标失败(保持冻结,稍后再试): ", err)
		w.holdFreeze()
		return err
	}
	st.Stage, st.DoneAt, st.FenceEpoch = StageFenced, s.now().Unix(), e
	st.Err = fmt.Errorf("结果不明:本机以纪元 %d 继续当主机,目标若之后以纪元 %d 起来会被副机拒绝并自动变为副机", e, st.Epoch).Error()
	if !s.saveSwitch(st, map[string]string{KeyFreezeUntil: ""}) {
		return nil
	}
	s.invalidate()
	by := nonEmpty(actor, "timeout")
	s.addNotice(Notice{Kind: "switchFailed", Reason: "fenced", Name: st.TargetName, N: int64(e)})
	s.audit("ha", "set-master-fenced", map[string]interface{}{"target": st.TargetName, "epoch": e, "by": by})
	logger.Warning("容灾:设为主机结果不明,已以纪元 ", e, " 隔离目标 ", st.TargetName, "(", by, "),写操作恢复")
	s.notify(fmt.Sprintf("🟠 <b>设为主机结果不明,已放弃</b>:本机以纪元 %d 继续当主机,写操作已恢复。\n「%s」若之后以纪元 %d 起来,会被副机拒绝并自动变为副机,它那份数据不会生效。",
		e, notify.Esc(st.TargetName), st.Epoch))
	return nil
}

// errTargetUp 隔离途中看到目标已经以新纪元当了主机。
var errTargetUp = errors.New("目标已经以新纪元当了主机")

// fenceShortError 撤销确认的台数不够。
type fenceShortError struct{ got, need int }

func (e *fenceShortError) Error() string {
	return fmt.Sprintf("只有 %d 台副机确认收到撤销,要 %d 台", e.got, e.need)
}

func fenceShort(got, need int) error { return &fenceShortError{got: got, need: need} }

// fenceBlocked 撤销确认的台数不够:不升纪元、保持冻结,记下原因(界面显示),每次设为主机只通知一次。
func (w *switcher) fenceBlocked(st SwitchState, got, need int) error {
	s := w.s
	w.mu.Lock()
	w.lastFence = time.Now()
	first := w.blockedId != st.Id
	w.blockedId = st.Id
	w.mu.Unlock()
	w.holdFreeze()
	err := fmt.Errorf("只有 %d 台副机确认收到撤销(要 %d 台):为免目标其实已经接手、两台主机同时收写,先不隔离,写操作保持暂停,稍后自动再试", got, need)
	if cur := s.switchState(); cur.Id == st.Id && cur.Stage == StageUnknown && cur.Err != err.Error() {
		cur.Err = err.Error()
		s.saveSwitch(cur, nil)
	}
	logger.Warning("容灾:", err)
	if first {
		s.notify(fmt.Sprintf("🟠 <b>设为主机结果不明,暂不隔离</b>:「%s」%s。连不上的副机恢复后会自动完成;冻结截止前放弃要目标以外的副机全部确认,截止后过半即可。",
			notify.Esc(st.TargetName), notify.Esc(err.Error())))
	}
	return err
}

// holdFreeze 结果不明、隔离没做成:写闸至少再关 2 分钟。
func (w *switcher) holdFreeze() {
	s := w.s
	if until := s.now().Add(2 * time.Minute).Unix(); s.freezeUntil() < until {
		_ = s.set(map[string]string{KeyFreezeUntil: strconv.FormatInt(until, 10)})
		s.invalidate()
	}
}

// leaving 本机已经(或正在)变为副机。
func (s *Service) leaving() bool {
	s.mu.Lock()
	d := s.demoting
	s.mu.Unlock()
	return d || s.IsNode()
}

// tick 每 5 秒:结果不明的继续问目标、到期隔离;进程在设为主机途中重启过的,把半截的记录收尾;残留的冻结到期解冻。
func (w *switcher) tick() {
	s := w.s
	if s.IsNode() || w.isRunning() {
		return
	}
	st := s.switchState()
	switch {
	case st.Stage == StageCommit || st.Stage == StageUnknown:
		w.tickUnknown()
		return
	case st.Active():
		// 不在进行(进程重启过),还没到提交:一定没提交。让目标丢掉暂存(问不到它也会到期丢掉),撤销交接标记,解冻
		var n model.Node
		if st.Nonce != "" && s.d.Hub != nil && s.d.DB.Where("id = ?", st.Target).Limit(1).Find(&n).Error == nil && n.Id != 0 {
			_ = w.abort(n, st.Nonce)
		}
		w.fail(st, st.Stage, errors.New("本机在设为主机途中重启了,这次没有完成"))
		return
	}
	if f := s.freezeUntil(); f != 0 && f <= s.now().Unix() {
		_ = s.set(map[string]string{KeyFreezeUntil: ""})
		s.invalidate()
		logger.Info("容灾:写操作冻结到期,已解冻")
	}
}

// tickUnknown 提交结果不明(或进程在提交途中重启过):每 10 秒问一次目标;冻结截止仍不明就隔离目标。
func (w *switcher) tickUnknown() {
	s := w.s
	w.unkMu.Lock()
	defer w.unkMu.Unlock()
	st := s.switchState()
	if st.Stage != StageCommit && st.Stage != StageUnknown {
		return
	}
	if st.Stage == StageCommit { // 进程在提交途中重启过:结论不明
		w.enterUnknown(st, errors.New("本机在提交途中重启了"))
		st = s.switchState()
	}
	if s.now().Unix() >= st.FreezeUntil { // 冻结截止(交接标记也作废了)仍不明:不再等它,隔离
		w.mu.Lock()
		recent := !w.lastFence.IsZero() && time.Since(w.lastFence) < 30*time.Second
		w.mu.Unlock()
		if !recent {
			_ = w.fence(st, "")
		}
		return
	}
	w.mu.Lock()
	if !w.lastProbe.IsZero() && time.Since(w.lastProbe) < switchProbeEvery { // 问的是网络:按真实时间限频
		w.mu.Unlock()
		return
	}
	w.lastProbe = time.Now()
	w.mu.Unlock()
	var n model.Node
	if s.d.Hub != nil && s.d.DB.Where("id = ?", st.Target).Limit(1).Find(&n).Error == nil && n.Id != 0 {
		switch w.probeCommit(n, st) {
		case commitDone:
			s.becomeNode(st)
		case commitNot:
			w.fail(st, StageCommit, errors.New("目标确认没有接手(已丢掉暂存的数据)"))
		}
	}
}

// sealBundle 导出整库与主机身份,用目标的一次性公钥加密、本机签名,写成信封文件。返回路径与字节数。
func (s *Service) sealBundle(dir string, pub *[32]byte, nonce string) (string, int64, error) {
	zipPath, meta, err := s.buildArchive(dir, true)
	if err != nil {
		return "", 0, err
	}
	zst, err := os.Stat(zipPath)
	if err != nil {
		return "", 0, err
	}
	sign, err := s.signer()
	if err != nil {
		return "", 0, err
	}
	dataKey := NewDataKey()
	wrap, err := SealTo(dataKey, pub)
	if err != nil {
		return "", 0, err
	}
	pubB64 := (BoxKey{Pub: pub}).PubString()
	hdr := EnvHeader{Kind: KindBundle, Epoch: s.Epoch(), CreatedAt: meta.CreatedAt, MasterId: meta.MasterId, MasterName: meta.MasterName,
		Version: meta.Version, MaxChangeId: meta.MaxChangeId, Wrap: wrap, To: Fingerprint(pubB64), Nonce: nonce}
	envPath := filepath.Join(dir, "bundle.env")
	zf, err := os.Open(zipPath)
	if err != nil {
		return "", 0, err
	}
	defer zf.Close()
	ef, err := os.OpenFile(envPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	err = WriteEnvelope(ef, hdr, dataKey, zf, zst.Size(), sign)
	if cerr := ef.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	_ = os.Remove(zipPath)
	est, err := os.Stat(envPath)
	if err != nil {
		return "", 0, err
	}
	return envPath, est.Size(), nil
}

// sendBundle 把整库数据推给目标:时限按大小算(60 秒 + 每 MB 2 秒,含目标验证、组库),不超过 limit(冻结剩下的)。
func (s *Service) sendBundle(n model.Node, envPath string, size int64, limit time.Duration) error {
	f, err := os.Open(envPath)
	if err != nil {
		return err
	}
	defer f.Close()
	timeout := min(60*time.Second+time.Duration(size>>20)*2*time.Second, limit)
	h := http.Header{}
	h.Set("Content-Type", "application/octet-stream")
	var out struct {
		OK string `json:"ok"`
	}
	return s.d.Hub.Send(n, http.MethodPost, "ha/handover", f, size, h, timeout, &out)
}

func (w *switcher) isRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

// Switching 设为主机的后台步骤还在跑:进度写成完成或没有完成之后,还要收尾一小会儿(发通知、安排重启、清临时文件)。
func (s *Service) Switching() bool { return s.sw.isRunning() }

// ---- 写闸 ----

// writeGate 管理员、代理、外部 API、Telegram 机器人、订阅页的写请求都经它:冻结时挡住,否则登记在途。
// 置冻结与登记在同一把锁下,冻结之后进来的写一律被挡;置冻结后等在途的做完再导出(见 freezeAndDrain)。
type writeGate struct {
	mu       sync.Mutex
	inflight int
}

// BeginWrite 写请求开始:冻结中返回 ok=false(调用方回 503 和 FrozenText);否则登记在途,做完一定要调 done。
func (s *Service) BeginWrite() (done func(), ok bool) {
	g := &s.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if s.Frozen() {
		return func() {}, false
	}
	g.inflight++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.inflight--
			g.mu.Unlock()
		})
	}, true
}

// SetSwitchCommitWait 测试用:提交没回话时当场问目标多久(进程内多实例测试不必干等 90 秒)。返回恢复原值的函数。
func SetSwitchCommitWait(d time.Duration) (restore func()) {
	old := switchCommitWait
	switchCommitWait = d
	return func() { switchCommitWait = old }
}
