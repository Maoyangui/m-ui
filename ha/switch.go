package ha

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
//	冻结写操作 → 向目标要一把一次性公钥(经现有的服务器链接:令牌鉴权 + 钉住的证书指纹)→ 导出整库与主机身份 →
//	用一次性公钥加密、本机签名,推过去 → 目标验证、解开、组好新库暂存 → 给所有副机发交接标记(纪元 E+1 的主机是目标)→
//	让目标提交:它重启、以纪元 E+1 接手 → 本机变为副机(保留自己的线路继续服务)、重启。
//
// 中途任何一步失败:通知目标丢掉暂存、解冻,本机照旧是主机,不丢数据。提交那一步结果不明(目标没回话)时,
// 去问目标:它已经以新纪元起来了就照常交接;问不到就等冻结超时后解冻 —— 那时目标若已接手,
// 它的推送会让多数副机对本机回 stale,本机照常变为副机,不会出现两台同时生效。

// 设为主机的阶段。
const (
	StagePrepare = "prepare" // 向目标要一次性公钥
	StageExport  = "export"  // 导出整库、加密
	StageSend    = "send"    // 推给目标
	StageGrant   = "grant"   // 给副机发交接标记
	StageCommit  = "commit"  // 目标提交
	StageDone    = "done"
	StageFailed  = "failed"
)

// switchFreeze 冻结写操作最多多久(超时自动解冻);switchCommitWait 提交没回话时最多问目标多久。
var (
	switchFreeze     = 15 * time.Minute
	switchCommitWait = 90 * time.Second
)

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
}

// Active 还在进行中。
func (st SwitchState) Active() bool {
	return st.Id != "" && st.Stage != StageDone && st.Stage != StageFailed
}

type switcher struct {
	s       *Service
	mu      sync.Mutex
	running bool
	grantTo *Grant
}

func (s *Service) switchState() SwitchState {
	var st SwitchState
	s.getJSON(KeySwitch, &st)
	return st
}

func (s *Service) saveSwitch(st SwitchState) {
	st.UpdatedAt = s.now().Unix()
	if err := s.set(map[string]string{KeySwitch: mustJSON(st)}); err != nil {
		logger.Warning("容灾:记录设为主机的进度失败: ", err)
	}
}

// grant 这一轮随容灾状态推出去的交接标记(设为主机进行到交接那一步以后)。
func (w *switcher) grant() *Grant {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.grantTo
}

// PlanItem 设为主机前的一项检查结果(界面按 Code 翻译,Args 是要填进去的值)。
type PlanItem struct {
	Code string            `json:"code"`
	Args map[string]string `json:"args,omitempty"`
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

// SetMasterPlan 检查能不能把服务器 id 设为主机,讲清楚会发生什么。
func (s *Service) SetMasterPlan(id uint) (SetMasterPlan, error) {
	p := SetMasterPlan{Target: id, SelfName: s.selfName(), OldPanel: s.panelURL(), Size: s.dbSize(), Blockers: []PlanItem{}, Warnings: []PlanItem{}}
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
	if strings.HasPrefix(strings.ToLower(p.NewPanel), "http://") {
		warn("plain")
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
	for _, l := range lines {
		if l.Port == subPort && !render.UDPOnly(l.Protocol) && render.LineOnNode(l, id) {
			warn("subPort", "port", strconv.Itoa(subPort), "line", l.Name)
			p.SubKept = false
			break
		}
	}
	for _, sp := range nodeServicePorts(n) {
		if sp.Port == subPort && sp.Label != "sub" {
			warn("subPort", "port", strconv.Itoa(subPort), "line", sp.Label)
			p.SubKept = false
			break
		}
	}
	if l := s.selfLink(); l.ApiUrl == "" {
		warn("noSelfLink")
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
		warn("oldNodes", "names", strings.Join(old, "、"))
	}
	cfg := s.Config()
	p.DnsNames = splitNames(cfg.DnsNames)
	p.DnsAuto = cfg.DnsTokenSet && len(p.DnsNames) > 0
	p.TargetIP = n.PublicIP
	if p.TargetIP == "" {
		p.TargetIP = n.PublicIP6
	}
	if n.Addr != "" {
		p.TargetIP = n.Addr
	}
	p.Ok = len(p.Blockers) == 0
	return p, nil
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

// SetMaster 把服务器 id 设为主机(在后台进行,进度看 SwitchState)。actor 记进审计与通知。
func (s *Service) SetMaster(id uint, actor string) (SwitchState, error) {
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
		return SwitchState{}, fmt.Errorf("「%s」现在不能设为主机:%s", plan.TargetName, planText(plan.Blockers[0]))
	}
	L := s.Epoch()
	st := SwitchState{Id: randID(), Target: id, TargetName: plan.TargetName, Stage: StagePrepare, StartedAt: s.now().Unix(),
		Epoch: L + 1, NewPanel: plan.NewPanel, SubHost: plan.SubHost, SubKept: plan.SubKept, DnsAuto: plan.DnsAuto,
		TargetIP: plan.TargetIP, Actor: actor, Size: plan.Size}
	if err := s.set(map[string]string{
		KeySwitch:      mustJSON(st),
		KeyFreezeUntil: strconv.FormatInt(s.now().Add(switchFreeze).Unix(), 10),
	}); err != nil {
		s.promoteMu.Unlock()
		return st, err
	}
	s.invalidate() // 冻结期间备用不得接任:随状态推出去
	s.sw.mu.Lock()
	s.sw.running = true
	s.sw.mu.Unlock()
	logger.Warning("容灾:开始把主机换到 ", plan.TargetName, "(", actor, " 发起),写操作暂停")
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
	}
	return it.Code
}

// run 设为主机的各步(在后台)。
func (w *switcher) run(st SwitchState) {
	s := w.s
	fail := func(stage string, err error) {
		logger.Warning("容灾:设为主机在「", stage, "」这一步失败: ", err)
		st.Stage, st.Err = StageFailed, err.Error()
		w.mu.Lock()
		w.grantTo = nil
		w.mu.Unlock()
		s.saveSwitch(st)
		_ = s.set(map[string]string{KeyFreezeUntil: ""})
		s.invalidate()
		s.addNotice(Notice{Kind: "switchFailed", Name: st.TargetName, Warn: []string{err.Error()}})
		s.notify("🔴 <b>设为主机没有完成</b>:" + notify.Esc(st.TargetName) + "\n" + notify.Esc(err.Error()) + "\n本机照旧是主机,写操作已恢复,数据没有变化。")
	}
	var n model.Node
	if err := s.d.DB.Where("id = ?", st.Target).First(&n).Error; err != nil {
		fail(StagePrepare, errors.New("服务器不存在"))
		return
	}
	// 1. 向目标要一次性公钥
	var prep PrepareReply
	req, _ := json.Marshal(PrepareRequest{Master: s.selfLink(), Version: s.d.Version, Size: st.Size})
	if err := s.d.Hub.Send(n, http.MethodPost, "ha/prepare", bytes.NewReader(req), int64(len(req)), nil, 30*time.Second, &prep); err != nil {
		fail(StagePrepare, err)
		return
	}
	pub, err := ParsePub(prep.Pub)
	if err != nil || prep.Nonce == "" {
		fail(StagePrepare, errors.New("目标机器回的一次性公钥不对"))
		return
	}
	abort := func() {
		b, _ := json.Marshal(map[string]string{"nonce": prep.Nonce})
		_ = s.d.Hub.Send(n, http.MethodPost, "ha/abort", bytes.NewReader(b), int64(len(b)), nil, 15*time.Second, nil)
	}
	// 2. 导出整库、加密
	st.Stage = StageExport
	s.saveSwitch(st)
	if err := ensureDir(s.Dir()); err != nil {
		abort()
		fail(StageExport, err)
		return
	}
	dir, err := os.MkdirTemp(s.Dir(), "switch-")
	if err != nil {
		abort()
		fail(StageExport, err)
		return
	}
	defer os.RemoveAll(dir)
	envPath, size, err := s.sealBundle(dir, pub, prep.Nonce)
	if err != nil {
		abort()
		fail(StageExport, err)
		return
	}
	st.Size = size
	// 3. 推给目标
	st.Stage = StageSend
	s.saveSwitch(st)
	if err := s.sendBundle(n, envPath, size); err != nil {
		abort()
		fail(StageSend, err)
		return
	}
	// 4. 给副机发交接标记:纪元 E+1 的主机是目标(还没进过纪元的集群不用:它们认任何纪元)
	st.Stage = StageGrant
	s.saveSwitch(st)
	if L := s.Epoch(); L > 0 {
		w.mu.Lock()
		w.grantTo = &Grant{Epoch: st.Epoch, To: st.Target, At: s.now().Unix()}
		w.mu.Unlock()
		s.invalidate()
		got := s.pushAll(s.remoteNodes(), 15*time.Second)
		logger.Info("容灾:交接标记已发给 ", got, " 台副机")
	}
	// 5. 目标提交
	st.Stage = StageCommit
	s.saveSwitch(st)
	b, _ := json.Marshal(map[string]string{"nonce": prep.Nonce})
	err = s.d.Hub.Send(n, http.MethodPost, "ha/commit", bytes.NewReader(b), int64(len(b)), nil, 30*time.Second, nil)
	if err != nil {
		committed, known := w.waitCommitted(n, st.Epoch, switchCommitWait)
		if !committed {
			if known {
				abort()
				fail(StageCommit, fmt.Errorf("目标没有接手: %w", err))
				return
			}
			// 问不到:不能确定它接没接手。不变成副机,也不解冻,等冻结超时(那时它若已接手,多数副机会对本机回 stale)
			st.Err = "目标没有回话,不确定它接手没有:等它恢复联系,或冻结超时后本机继续当主机"
			s.saveSwitch(st)
			logger.Warning("容灾:设为主机提交结果不明: ", err)
			return
		}
	}
	// 6. 本机变为副机
	st.Stage, st.DoneAt, st.Err = StageDone, s.now().Unix(), ""
	s.becomeNode(st)
}

// waitCommitted 提交请求没有回话时去问目标:返回(接手了没有, 问到了没有)。它已经以新纪元起来,
// 本机的请求会收到 409 stale 且纪元不低于 epoch;还是副机就看它暂存的那一步。
func (w *switcher) waitCommitted(n model.Node, epoch uint64, limit time.Duration) (bool, bool) {
	s := w.s
	deadline := time.Now().Add(limit) // 等的是网络:按真实时间
	known := false
	for {
		var out struct {
			Stage string `json:"stage"`
		}
		err := s.d.Hub.Send(n, http.MethodGet, "ha/incoming", nil, 0, nil, 8*time.Second, &out)
		var ee *hub.EpochError
		switch {
		case errors.As(err, &ee) && ee.Code == CodeStale && ee.Epoch >= epoch:
			return true, true
		case err == nil && out.Stage == incCommitted:
			known = true // 已提交,正在重启:再等一会儿
		case err == nil:
			return false, true // 它没有暂存的数据了(已丢弃或过期)
		}
		if time.Now().After(deadline) || s.stopping() {
			return false, known
		}
		time.Sleep(min(3*time.Second, limit/4))
	}
}

// becomeNode 设为主机完成:本机变为副机(保留自己的线路继续服务),留一份备份,发通知,重启。
func (s *Service) becomeNode(st SwitchState) {
	backup := ""
	if s.d.Backup != nil {
		if name, err := s.d.Backup("before-set-master-"); err == nil {
			backup = name
		} else {
			logger.Warning("容灾:变为副机前的备份失败(继续): ", err)
		}
	}
	values := map[string]string{
		"nodeMode": "true", KeyRole: "", KeyMasterId: strconv.FormatUint(uint64(st.Target), 10),
		KeyGrant:           mustJSON(Grant{Epoch: st.Epoch, To: st.Target, At: s.now().Unix()}),
		hub.LedgerEpochKey: hub.NewLedgerEpoch(), // 本机的流量账本换一本:新主机只建基线
		KeySignKey:         "", KeyFreezeUntil: "", KeyFormer: "", KeyStandbyList: "",
		KeySwitch: mustJSON(st),
	}
	if err := s.set(values); err != nil {
		logger.Warning("容灾:变为副机失败: ", err)
		return
	}
	s.sw.mu.Lock()
	s.sw.grantTo = nil
	s.sw.mu.Unlock()
	s.addNotice(Notice{Kind: "switched", Name: st.TargetName, URL: st.NewPanel, Host: st.SubHost, IP: st.TargetIP, Dns: st.DnsAuto, File: backup})
	logger.Warning("容灾:主机已换到 ", st.TargetName, ",本机变为副机;新面板 ", st.NewPanel)
	msg := fmt.Sprintf("✅ <b>主机已换到「%s」</b>,本机「%s」已变为副机,继续用自己的线路服务。\n新面板:%s(账号、密码、两步验证不变,需要重新登录)",
		notify.Esc(st.TargetName), notify.Esc(s.selfName()), notify.Esc(st.NewPanel))
	switch {
	case st.SubKept && st.DnsAuto:
		msg += "\nDNS:会自动把订阅域名改指向新主机,用户的订阅地址不变。"
	case st.SubKept:
		msg += fmt.Sprintf("\nDNS:把 %s 改指向新主机的 IP %s,用户的订阅地址就不变。", notify.Esc(st.SubHost), notify.Esc(st.TargetIP))
	}
	s.notify(msg)
	s.restart(3 * time.Second)
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

// sendBundle 把整库数据推给目标。
func (s *Service) sendBundle(n model.Node, envPath string, size int64) error {
	f, err := os.Open(envPath)
	if err != nil {
		return err
	}
	defer f.Close()
	timeout := 60*time.Second + time.Duration(size>>20)*2*time.Second
	if timeout > 30*time.Minute {
		timeout = 30 * time.Minute
	}
	h := http.Header{}
	h.Set("Content-Type", "application/octet-stream")
	var out struct {
		OK string `json:"ok"`
	}
	return s.d.Hub.Send(n, http.MethodPost, "ha/handover", f, size, h, timeout, &out)
}

// tick 每 5 秒:冻结超时自动解冻;进程在设为主机途中重启过的,把半截的记录收尾。
func (w *switcher) tick() {
	s := w.s
	if s.IsNode() {
		return
	}
	w.mu.Lock()
	running := w.running
	w.mu.Unlock()
	f := s.freezeUntil()
	if running || f == 0 {
		return
	}
	if f > s.now().Unix() {
		if st := s.switchState(); st.Active() && st.Stage != StageCommit {
			// 不在进行(进程重启过),又不在提交这一步:直接收尾
			st.Stage, st.Err = StageFailed, "本机在设为主机途中重启了,这次没有完成"
			s.saveSwitch(st)
			_ = s.set(map[string]string{KeyFreezeUntil: ""})
			s.invalidate()
		}
		return
	}
	// 冻结到期
	_ = s.set(map[string]string{KeyFreezeUntil: ""})
	if st := s.switchState(); st.Active() {
		st.Stage, st.Err = StageFailed, "等待超时:目标机器没有接手,本机继续当主机"
		s.saveSwitch(st)
		s.notify("🔴 <b>设为主机没有完成</b>:" + notify.Esc(st.TargetName) + " 一直没有接手,本机继续当主机,写操作已恢复。")
	}
	s.invalidate()
	logger.Info("容灾:写操作冻结到期,已解冻")
}

func (w *switcher) isRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}
