package ha

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
)

// ---- 修复者 B 的单测:设为主机流程、身份迁移、DNS、通知 ----

func shortCommitWait(t *testing.T) {
	t.Helper()
	w := switchCommitWait
	switchCommitWait = 2 * time.Second
	t.Cleanup(func() { switchCommitWait = w })
}

// agentHook 在假副机前面再挂一层:before 在转给它之前调;lose 为真的动作照常处理、但不回话(连接直接断)。
type agentHook struct {
	before func(action string)
	lose   func(action string) bool
}

// wrapAgent 主机改经这一层连第 i 台副机(重新记证书指纹)。
func wrapAgent(t *testing.T, cl *haCluster, i int, h agentHook) {
	t.Helper()
	a := cl.agents[i]
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Path
		if k := strings.Index(action, "/agent/"); k >= 0 {
			action = action[k+len("/agent/"):]
		}
		if h.before != nil {
			h.before(action)
		}
		if h.lose != nil && h.lose(action) {
			a.serve(httptest.NewRecorder(), r)
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
					return
				}
			}
			return
		}
		a.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	if err := cl.m.d.DB.Model(&model.Node{}).Where("id = ?", cl.ids[i]).Updates(map[string]interface{}{"api_url": srv.URL + "/app/", "cert_fp": ""}).Error; err != nil {
		t.Fatal(err)
	}
}

// readyCluster 开着容灾的主机 + n 台副机,同步两轮,主机有连得上的面板地址。
func readyCluster(t *testing.T, n int) *haCluster {
	t.Helper()
	cl := newCluster(t, n)
	masterPanel(t, cl.m)
	// 签名密钥先生成好:newCluster 直接写的纪元,没走开启容灾那一步;不然头一轮并发推送时各自生成一把,副机钉的可能不是同一把
	if _, err := cl.m.signKey(); err != nil {
		t.Fatal(err)
	}
	cl.h.RunOnce()
	cl.h.RunOnce()
	return cl
}

// 发送慢到越过冻结截止(交接前要留 2 分钟):不发交接标记、不提交,目标丢掉暂存,本机解冻、照旧是主机。
func TestSetMasterFreezeExpiryAborts(t *testing.T) {
	cl := readyCluster(t, 2)
	m, tgt := cl.m, cl.cands[0]
	wrapAgent(t, cl, 0, agentHook{before: func(action string) {
		if action == "ha/handover" {
			cl.clk.add(switchFreezeMax) // 推数据推了很久
		}
	}})
	cl.h.RunOnce()
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitchDone(t, m)
	if st.Stage != StageFailed || st.Err != errFreezeShort.Error() || st.GrantSent || m.Frozen() || m.IsNode() {
		t.Fatalf("冻结截止前来不及交接:应中止、解冻、照旧是主机: %+v", st)
	}
	if cl.agents[0].count("ha/commit") != 0 {
		t.Fatal("冻结到期后绝不能提交")
	}
	for i, c := range cl.cands {
		if c.setting(KeyGrant) != "" {
			t.Fatalf("交接标记不能发出去(副机 %d): %s", i, c.setting(KeyGrant))
		}
	}
	if tgt.IncomingStage() != "" {
		t.Fatal("目标的暂存要丢掉")
	}
}

// 提交请求没被处理:目标说还暂存着、并确认丢掉了 → 确定没提交:失败收尾、解冻,已发的交接标记撤销
// (之后目标就算以 E+1 出现,还联系得上主机的副机也不会凭残留的标记跟它走)。
func TestSetMasterAbortAckIsNotCommitted(t *testing.T) {
	shortCommitWait(t)
	cl := readyCluster(t, 2)
	m, tgt, other := cl.m, cl.cands[0], cl.cands[1]
	L := m.Epoch()
	cl.agents[0].mu.Lock()
	cl.agents[0].dropCommit = true
	cl.agents[0].mu.Unlock()
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitchDone(t, m)
	if st.Stage != StageFailed || !st.GrantSent || m.Frozen() || m.IsNode() || m.Epoch() != L {
		t.Fatalf("确定没提交:失败收尾、解冻、纪元不变: %+v", st)
	}
	if tgt.IncomingStage() != "" || cl.agents[0].count("ha/abort") == 0 {
		t.Fatal("目标要确认丢掉暂存")
	}
	if g := other.setting(KeyGrant); g != "" {
		t.Fatalf("交接标记要撤销: %s", g)
	}
	if rej := other.CheckAgent(hdr(L+1, cl.ids[0]), "ha/state"); rej == nil || rej.Code != CodeFollowing {
		t.Fatalf("撤销以后目标以 E+1 来,还联系得上主机的副机不跟它: %+v", rej)
	}
}

// 提交其实做了、只是回话丢了:去问,目标说已提交 → 照常变为副机;不发丢弃、不解冻成"没完成"。
func TestSetMasterCommittedSeenKeepsFrozen(t *testing.T) {
	shortCommitWait(t)
	cl := readyCluster(t, 1)
	m, tgt := cl.m, cl.cands[0]
	restarted := 0
	var mu sync.Mutex
	tgt.d.Restart = func(time.Duration) { mu.Lock(); restarted++; mu.Unlock() }
	m.d.Restart = func(time.Duration) {}
	wrapAgent(t, cl, 0, agentHook{lose: func(action string) bool { return action == "ha/commit" }})
	cl.h.RunOnce()
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitchDone(t, m)
	if st.Stage != StageDone || !m.IsNode() {
		t.Fatalf("目标说已提交:照常变为副机: %+v", st)
	}
	if tgt.IncomingStage() != incCommitted || cl.agents[0].count("ha/abort") != 0 {
		t.Fatal("看到已提交以后不能再让目标丢弃")
	}
	if _, err := os.Stat(promoteMarkFile(tgt.d.DBPath)); err != nil {
		t.Fatal("提交时要写下新库的时效标记")
	}
	var mark struct {
		Epoch   uint64 `json:"epoch"`
		How     string `json:"how"`
		Expires int64  `json:"expires"`
	}
	b, _ := os.ReadFile(promoteMarkFile(tgt.d.DBPath))
	if json.Unmarshal(b, &mark) != nil || mark.Epoch != st.Epoch || mark.How != HowSetMaster || mark.Expires <= time.Now().Unix() {
		t.Fatalf("时效标记: %s", b)
	}
	// 已提交的不能再丢弃;重启过却还是副机(换库没成,新库还在):说不出已提交,也不让丢弃 —— 原主机按结果不明处理、到期隔离
	if err := tgt.Abort("x"); err == nil {
		t.Fatal("已提交的不能丢弃")
	}
	tgt.inc.mu.Lock()
	tgt.inc.reset()
	tgt.inc.mu.Unlock()
	if tgt.IncomingStage() != "" || tgt.Abort("x") == nil {
		t.Fatal("换库没成、新库还在:不说已提交,也不能丢弃")
	}
	if _, err := tgt.Prepare(PrepareRequest{Version: m.d.Version}); err == nil {
		t.Fatal("新库还等着换上时不能再来一次")
	}
	mu.Lock()
	defer mu.Unlock()
	if restarted == 0 {
		t.Fatal("目标提交后要重启")
	}
}

// 推完数据目标就彻底失联:结果不明 —— 保持冻结(写闸关着),冻结截止前不解冻;截止后先以纪元 E+2 隔离目标再解冻,
// 副机跟着本机到 E+2、残留的交接标记作废,目标之后以 E+1 起来会被拒。
func TestSetMasterUnknownFencesTarget(t *testing.T) {
	shortCommitWait(t)
	cl := readyCluster(t, 2)
	m, other := cl.m, cl.cands[1]
	L := m.Epoch()
	cl.agents[0].mu.Lock()
	cl.agents[0].dieAfterHandover = true
	cl.agents[0].mu.Unlock()
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitchDone(t, m)
	if st.Stage != StageUnknown || !st.Unknown || m.IsNode() || !m.Frozen() {
		t.Fatalf("结果不明:不变成副机、保持冻结: %+v", st)
	}
	if _, ok := m.BeginWrite(); ok {
		t.Fatal("结果不明时写操作保持暂停")
	}
	m.sw.tick() // 冻结截止前:问不到就继续等
	if st := m.Switch(); st.Stage != StageUnknown || !m.Frozen() {
		t.Fatalf("截止前继续等: %+v", st)
	}
	if g := other.setting(KeyGrant); !strings.Contains(g, `"to":`+strconv.Itoa(int(cl.ids[0]))) {
		t.Fatalf("不明期间交接标记留着(目标在冻结期内起来照常完成): %s", g)
	}
	cl.clk.add(time.Duration(st.FreezeUntil-cl.clk.now().Unix()+1) * time.Second)
	m.sw.tick()
	st = m.Switch()
	if st.Stage != StageFenced || st.FenceEpoch != L+2 || m.Epoch() != L+2 || m.Frozen() || m.IsNode() {
		t.Fatalf("截止后以纪元 E+2 隔离目标再解冻: %+v epoch=%d", st, m.Epoch())
	}
	if done, ok := m.BeginWrite(); !ok {
		t.Fatal("隔离后写操作恢复")
	} else {
		done()
	}
	cl.h.RunOnce() // 本机以 E+2 推送:收过自交接标记的副机跟上来
	if other.Epoch() != L+2 {
		t.Fatalf("副机跟着本机到 E+2: %d", other.Epoch())
	}
	if rej := other.CheckAgent(hdr(L+1, cl.ids[0]), "ha/state"); rej == nil || rej.Code != CodeStale {
		t.Fatalf("目标之后以 E+1 起来要被拒: %+v", rej)
	}
}

// 管理员在结果不明时点「放弃」:立刻隔离目标、解冻;要放弃的得真是结果不明的那一次。
func TestAbandonSwitchFences(t *testing.T) {
	shortCommitWait(t)
	cl := readyCluster(t, 1)
	m := cl.m
	if err := m.AbandonSwitch("admin"); err == nil {
		t.Fatal("没有结果不明的设为主机时不能放弃")
	}
	L := m.Epoch()
	cl.agents[0].mu.Lock()
	cl.agents[0].dieAfterHandover = true
	cl.agents[0].mu.Unlock()
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	if st := waitSwitchDone(t, m); st.Stage != StageUnknown {
		t.Fatalf("前提:结果不明: %+v", st)
	}
	if err := m.AbandonSwitch("admin"); err != nil {
		t.Fatal(err)
	}
	if st := m.Switch(); st.Stage != StageFenced || m.Epoch() != L+2 || m.Frozen() {
		t.Fatalf("放弃 = 隔离目标后解冻: %+v", st)
	}
	var n int64
	m.d.DB.Model(&model.Change{}).Where("key = ? AND action = ?", "ha", "set-master-fenced").Count(&n)
	if n != 1 {
		t.Fatal("放弃要进操作审计")
	}
}

// 没开过容灾(纪元 0)的集群:设为主机先进入纪元 1、推给全部副机(目标钉住本机的签名公钥),目标以纪元 2 接手;
// 目标已经以新纪元当了主机时,本机的请求收到 409 stale(主机就是它)= 已提交。
func TestSetMasterEpochZeroCluster(t *testing.T) {
	cl := newCluster(t, 2)
	m, tgt, other := cl.m, cl.cands[0], cl.cands[1]
	masterPanel(t, m)
	m.set(map[string]string{KeyEpoch: "", KeyMode: ModeOff, KeyMasterId: ""})
	m.d.Restart = func(time.Duration) {}
	tgt.d.Restart = func(time.Duration) {}
	cl.h.RunOnce()
	cl.h.RunOnce()
	if m.Epoch() != 0 || tgt.Epoch() != 0 {
		t.Fatal("前提:纪元 0")
	}
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitchDone(t, m)
	if st.Stage != StageDone || st.Epoch != 2 || !m.IsNode() {
		t.Fatalf("纪元 0 的集群也能设为主机,目标以纪元 2 接手: %+v", st)
	}
	if tgt.Epoch() != 1 || tgt.setting(KeyMasterPub) == "" {
		t.Fatal("目标先认了纪元 1 与本机的签名公钥")
	}
	var g Grant
	if !other.getJSON(KeyGrant, &g) || g.Epoch != 2 || g.To != cl.ids[0] || g.ToPub == "" || g.Expires == 0 {
		t.Fatalf("别的副机收到交接标记(带目标的签名公钥、到期作废): %+v", g)
	}
	if !m.getJSON(KeyGrant, &g) || g.Epoch != 2 || g.To != cl.ids[0] {
		t.Fatalf("变为副机的原主机自己也记着跟谁: %+v", g)
	}
}

func TestProbeCommitSeesTargetAsMaster(t *testing.T) {
	cl := readyCluster(t, 2)
	m, tgt := cl.m, cl.cands[0]
	var n model.Node
	m.d.DB.First(&n, cl.ids[0])
	st := SwitchState{Epoch: 2, Target: cl.ids[0], Nonce: "x"}
	// 目标已经以纪元 2 当了主机:本机(纪元 1)的请求被回 stale,主机就是它
	tgt.set(map[string]string{"nodeMode": "false", KeyEpoch: "2", KeyMasterId: strconv.Itoa(int(cl.ids[0]))})
	if r := m.sw.probeCommit(n, st); r != commitDone {
		t.Fatalf("目标以新纪元当了主机 = 已提交: %v", r)
	}
	// 回 stale 的是一台跟着别的主机的副机:说不清
	tgt.set(map[string]string{"nodeMode": "true", KeyEpoch: "2", KeyMasterId: "99"})
	if r := m.sw.probeCommit(n, st); r != commitUnknown {
		t.Fatalf("跟着别的主机的不算: %v", r)
	}
}

// prepare 回来说目标连不上本机的面板地址:换完以后新主机管不了本机 —— 在这一步就失败,不往下做。
func TestSetMasterMasterReachFails(t *testing.T) {
	cl := readyCluster(t, 1)
	m, tgt := cl.m, cl.cands[0]
	m.d.PanelURL = func() string { return "https://127.0.0.1:1/app/" }
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitchDone(t, m)
	if st.Stage != StageFailed || !strings.Contains(st.Err, "连不上本机的面板地址") || m.Frozen() {
		t.Fatalf("目标连不上本机:失败并说清原因: %+v", st)
	}
	if tgt.IncomingStage() != "" || cl.agents[0].count("ha/handover") != 0 {
		t.Fatal("不往下推数据,目标丢掉暂存")
	}
}

// 本机磁盘不够导出整库:设为主机前就挡住(以代码返回原因)。
func TestSetMasterLocalDisk(t *testing.T) {
	cl := readyCluster(t, 1)
	m := cl.m
	old := localDiskFree
	localDiskFree = func(*Service) int64 { return 10 << 20 }
	t.Cleanup(func() { localDiskFree = old })
	p, err := m.SetMasterPlan(cl.ids[0])
	if err != nil || p.Ok || len(p.Blockers) == 0 || p.Blockers[0].Code != "localDisk" {
		t.Fatalf("本机磁盘不够要挡住: %+v %v", p.Blockers, err)
	}
	_, err = m.SetMaster(cl.ids[0], "admin", SetMasterOpts{})
	var pe *PlanError
	if !errors.As(err, &pe) || pe.Plan.Blockers[0].Code != "localDisk" || !strings.Contains(err.Error(), "本机磁盘剩余") {
		t.Fatalf("不能设为主机的原因带代码: %v", err)
	}
}

// 目标是明文 http:要管理员勾选确认才做;本机拼不出变为副机后的地址:挡住。
func TestSetMasterPlainAndSelfLink(t *testing.T) {
	cl := readyCluster(t, 1)
	m := cl.m
	p, _ := m.SetMasterPlan(cl.ids[0])
	if !p.Ok || p.PlainAck {
		t.Fatalf("https 的目标不用确认: %+v", p)
	}
	m.d.DB.Model(&model.Node{}).Where("id = ?", cl.ids[0]).Update("api_url", strings.Replace(cl.agents[0].url(), "https://", "http://", 1))
	p, _ = m.SetMasterPlan(cl.ids[0])
	if !p.PlainAck {
		t.Fatal("http 的目标要勾选确认")
	}
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != errPlainNotAcked {
		t.Fatalf("没勾选确认不能做: %v", err)
	}
	m.d.PanelURL = func() string { return "http://<本机IP>:2053/app/" }
	p, _ = m.SetMasterPlan(cl.ids[0])
	found := false
	for _, b := range p.Blockers {
		found = found || b.Code == "noSelfLink"
	}
	if !found || p.Ok {
		t.Fatalf("拼不出变为副机后的地址要挡住: %+v", p.Blockers)
	}
}

// 写闸:冻结前进来的写要做完才导出(最多等一会儿);冻结后进来的写一律挡住;等不到就失败、解冻。
func TestWriteGateDrain(t *testing.T) {
	cl := readyCluster(t, 1)
	m := cl.m
	done, ok := m.BeginWrite()
	if !ok {
		t.Fatal("没冻结时写放行")
	}
	st := SwitchState{Id: "x", Stage: StagePrepare, FreezeUntil: m.now().Add(time.Hour).Unix()}
	res := make(chan error, 1)
	go func() { res <- m.freezeAndDrain(st) }()
	deadline := time.Now().Add(5 * time.Second)
	for !m.Frozen() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := m.BeginWrite(); ok {
		t.Fatal("冻结以后进来的写要挡住")
	}
	select {
	case err := <-res:
		t.Fatalf("在途的写没做完不能导出: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	done()
	done() // 多调一次不出错
	if err := <-res; err != nil {
		t.Fatal(err)
	}
	m.set(map[string]string{KeyFreezeUntil: "", KeySwitch: ""})
	// 等不到:设为主机失败、解冻
	w := writeDrainWait
	writeDrainWait = 300 * time.Millisecond
	t.Cleanup(func() { writeDrainWait = w })
	hold, _ := m.BeginWrite()
	defer hold()
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	if st := waitSwitchDone(t, m); st.Stage != StageFailed || !strings.Contains(st.Err, "写操作没做完") || m.Frozen() {
		t.Fatalf("在途的写等不到:失败、解冻: %+v", st)
	}
}

// 变为副机:先把角色写好,期间收到的 stale(副机已经跟目标走了)不会再走"失联期间被接任"那条路。
func TestBecomeNodeNoSpuriousDemote(t *testing.T) {
	cl := readyCluster(t, 2)
	m := cl.m
	m.d.Restart = func(time.Duration) {}
	inBackup := make(chan struct{})
	release := make(chan struct{})
	m.d.Backup = func(prefix string) (string, error) {
		close(inBackup)
		<-release
		return prefix + "x.zip", nil
	}
	st := SwitchState{Id: "s1", Target: cl.ids[0], TargetName: "TW1", Stage: StageCommit, Epoch: 2, StartedAt: 1,
		FreezeUntil: m.now().Add(time.Hour).Unix(), NewPanel: "https://tw/app/", Grant: &Grant{Epoch: 2, To: cl.ids[0], ToPub: "pub", FromEpoch: 1}}
	m.set(map[string]string{KeySwitch: mustJSON(st)})
	go m.becomeNode(st)
	<-inBackup
	stale := hub.EpochReply{Aware: true, Code: CodeStale, Epoch: 2, MasterId: cl.ids[0]}
	m.NoteReply(model.Node{Id: cl.ids[0]}, stale)
	m.NoteReply(model.Node{Id: cl.ids[1]}, stale)
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for len(m.Notices()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !m.IsNode() || m.setting(KeyRole) != "" || m.setting(KeyDemoted) != "" {
		t.Fatalf("是计划内换主机:不是失联被接任 role=%q demoted=%q", m.setting(KeyRole), m.setting(KeyDemoted))
	}
	n := m.Notices()
	if len(n) != 1 || n[0].Kind != "switched" || n[0].File != "before-set-master-x.zip" {
		t.Fatalf("只有一条换主机完成的提示: %+v", n)
	}
	if got := m.Switch(); got.Stage != StageDone || got.Backup == "" {
		t.Fatalf("进度记成完成: %+v", got)
	}
}

// 迁来的订阅端口撞上新主机自己的面板 / 代理面板 / WARP / 线路:一律留新主机自己的订阅端口,如实告警(代码 + 参数),绝不让它起不来。
func TestApplyIdentityPortConflicts(t *testing.T) {
	cases := []struct {
		name, code, svc string
		kv              map[string]string
		line            bool
	}{
		{"panel", idwSubPortSvc, "panel", map[string]string{"webPort": "2096"}, false},
		{"reseller", idwSubPortSvc, "reseller", map[string]string{"resellerPort": "2096"}, false},
		{"warp", idwSubPortSvc, "warp", map[string]string{"warpPort": "2096"}, false},
		{"line", idwSubPortLine, "", map[string]string{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kv := map[string]string{"subPort": "2056"}
			for k, v := range c.kv {
				kv[k] = v
			}
			s := testService(t, true, kv)
			s.d.DB.Create(&model.Node{Id: 5, Name: "TW", IsLocal: true, Enabled: true})
			if c.line {
				s.d.DB.Create(&model.Line{Name: "vless-2096", Protocol: "vless", Port: 2096, Enabled: true, NodeIds: []byte(`[5]`)})
			}
			res, err := applyIdentity(s.d.DB, Identity{SubHost: "sub.example.com", SubPort: 2096, SubPath: "/s/"}, 5, t.TempDir(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if s.setting("subPort") != "2056" || res.SubURLKept || res.PrevSubPort != 2056 {
				t.Fatalf("留本机自己的订阅端口,订阅地址会变: port=%s %+v", s.setting("subPort"), res)
			}
			if len(res.Warnings) != 1 || res.Warnings[0].Code != c.code || res.Warnings[0].Args["own"] != "2056" || res.Warnings[0].Args["port"] != "2096" ||
				(c.svc != "" && res.Warnings[0].Args["svc"] != c.svc) || (c.line && res.Warnings[0].Args["line"] != "vless-2096") {
				t.Fatalf("告警带代码和参数: %+v", res.Warnings)
			}
			if txt := identityWarnText(res.Warnings[0]); !strings.Contains(txt, "2096") || !strings.Contains(txt, "2056") {
				t.Fatalf("Telegram 用的中文: %q", txt)
			}
		})
	}
	// 代理面板关着:它的端口不算占用
	s := testService(t, true, map[string]string{"subPort": "2056", "resellerPort": "2096", "resellerEnabled": "false"})
	s.d.DB.Create(&model.Node{Id: 5, Name: "TW", IsLocal: true, Enabled: true})
	res, _ := applyIdentity(s.d.DB, Identity{SubHost: "sub.example.com", SubPort: 2096}, 5, t.TempDir(), t.TempDir())
	if s.setting("subPort") != "2096" || !res.SubURLKept || len(res.Warnings) != 0 {
		t.Fatalf("代理面板关着时可以用它的端口: %+v", res)
	}
}

// 确认框说的和接手时做的一致:目标的面板端口 = 本机订阅端口 → 确认框说订阅会用它自己的端口(订阅地址会变)。
func TestPlanPortConflictMatchesBehavior(t *testing.T) {
	cl := readyCluster(t, 1)
	m := cl.m
	id := cl.ids[0]
	st, _ := m.d.Hub.StatusOf(id)
	if st.HA == nil || st.HA.WebPort == 0 {
		t.Fatalf("前提:目标报了自己的端口: %+v", st.HA)
	}
	m.set(map[string]string{"subPort": strconv.Itoa(st.HA.WebPort)})
	p, _ := m.SetMasterPlan(id)
	var w *PlanItem
	for i := range p.Warnings {
		if p.Warnings[i].Code == "subPortSvc" {
			w = &p.Warnings[i]
		}
	}
	if w == nil || w.Args["svc"] != "panel" || w.Args["own"] != strconv.Itoa(st.HA.SubPort) || p.SubKept || !p.Ok {
		t.Fatalf("目标面板端口被迁来的订阅端口撞上:提示但不挡,订阅地址会变: %+v %+v", p.Warnings, p)
	}
	// 同一规则套用身份
	tgt := cl.cands[0]
	res, err := applyIdentity(tgt.d.DB, Identity{SubHost: "sub.example.com", SubPort: st.HA.WebPort}, id, t.TempDir(), t.TempDir())
	if err != nil || len(res.Warnings) != 1 || res.Warnings[0].Code != idwSubPortSvc || tgt.setting("subPort") == strconv.Itoa(st.HA.WebPort) {
		t.Fatalf("接手时留自己的订阅端口: %+v %v", res, err)
	}
	// 目标的代理面板地址给出来
	if p.NewReseller != st.HA.ResellerURL {
		t.Fatalf("代理面板新地址: %q", p.NewReseller)
	}
}

// 原主机变为副机后别人怎么连:面板域名是随主机走的名字(= 订阅主机名,或在要跟着主机走的域名里)时改用公网 IP;
// 新主机库里原主机那一行的域名要是随主机走的,清空。
func TestSelfLinkAvoidsMovingName(t *testing.T) {
	s := testService(t, false, map[string]string{"publicIp": "203.0.113.5", "webPort": "8443", "webPath": "/p/",
		"webCertFile": "/x.crt", "webKeyFile": "/x.key", "webDomain": "panel.example.com", "subDomain": "sub.example.com"})
	s.d.PanelURL = func() string { return "https://panel.example.com:8443/p/" }
	if l := s.selfLink(); l.ApiUrl != "https://panel.example.com:8443/p/" {
		t.Fatalf("面板域名不随主机走:照用域名: %+v", l)
	}
	s.set(map[string]string{KeyDnsNames: "sub.example.com,panel.example.com"})
	if l := s.selfLink(); l.ApiUrl != "https://203.0.113.5:8443/p/" {
		t.Fatalf("面板域名在要跟着主机走的域名里:改用 IP: %+v", l)
	}
	s.set(map[string]string{KeyDnsNames: "", "subDomain": ""}) // 没单独设订阅域名:订阅主机名就是面板域名
	if l := s.selfLink(); l.ApiUrl != "https://203.0.113.5:8443/p/" {
		t.Fatalf("面板域名就是订阅主机名:改用 IP: %+v", l)
	}
	s.set(map[string]string{"publicIp": "2001:db8::5"})
	if l := s.selfLink(); l.ApiUrl != "https://[2001:db8::5]:8443/p/" {
		t.Fatalf("纯 IPv6:用 IPv6: %+v", l)
	}
	s.set(map[string]string{"publicIp": ""})
	if l := s.selfLink(); l.ApiUrl != "" {
		t.Fatalf("没有 IP:拼不出来(设为主机会挡住): %+v", l)
	}
	// 新主机库里原主机那一行
	n := testService(t, false, map[string]string{KeyDnsNames: "api.example.com"})
	n.d.DB.Create(&model.Node{Id: 1, Name: "HK", Domain: "sub.example.com", Enabled: true})
	n.d.DB.Create(&model.Node{Id: 2, Name: "TW", IsLocal: true, Enabled: true})
	n.d.DB.Create(&model.Node{Id: 3, Name: "JP", Domain: "api.example.com", Enabled: true})
	id := Identity{MasterName: "HK", SubHost: "sub.example.com", Self: Link{ApiUrl: "https://203.0.113.5:8443/p/", Token: "t", CertFP: "fp"}}
	if err := n.applyFormerRow(n.d.DB, 1, 2, id, HowSetMaster); err != nil {
		t.Fatal(err)
	}
	var row model.Node
	n.d.DB.First(&row, 1)
	if row.Domain != "" || row.ApiUrl != "https://203.0.113.5:8443/p/" || !row.Insecure || row.CertFP != "fp" || !row.Enabled {
		t.Fatalf("原主机那一行:按 IP 连、钉指纹,随主机走的域名清空: %+v", row)
	}
	n.d.DB.Model(&model.Node{}).Where("id = ?", 1).Update("domain", "hk.example.com")
	_ = n.applyFormerRow(n.d.DB, 1, 2, id, HowSetMaster)
	n.d.DB.First(&row, 1)
	if row.Domain != "hk.example.com" {
		t.Fatal("它自己的域名(不随主机走)留着")
	}
}

// 整库数据只认本机钉住的主机签名公钥签的:没钉住就拒收(不跳过验签)。
func TestBundleRejectedWithoutPinnedPub(t *testing.T) {
	cl := readyCluster(t, 1)
	m, s := cl.m, cl.cands[0]
	rep, err := s.Prepare(PrepareRequest{Version: m.d.Version})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Epoch != 1 || rep.MasterPubFP == "" || rep.SignPub == "" {
		t.Fatalf("prepare 回本机认的纪元、钉住的主机公钥指纹、本机以后的签名公钥: %+v", rep)
	}
	own, _ := m.SignPub()
	if rep.MasterPubFP != Fingerprint(own) {
		t.Fatal("钉住的就是主机的签名公钥")
	}
	pub, _ := ParsePub(rep.Pub)
	env, _, err := m.sealBundle(t.TempDir(), pub, rep.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(env)
	s.set(map[string]string{KeyMasterPub: ""})
	if _, err := s.ReceiveBundle(bytes.NewReader(raw)); err == nil || !strings.Contains(err.Error(), "钉住") {
		t.Fatalf("没钉住主机公钥要拒收: %v", err)
	}
	// 钉的是别的公钥:验签不过
	s.set(map[string]string{KeyMasterPub: PubOf(mustSignKey(t))})
	rep, _ = s.Prepare(PrepareRequest{Version: m.d.Version})
	pub, _ = ParsePub(rep.Pub)
	env, _, _ = m.sealBundle(t.TempDir(), pub, rep.Nonce)
	raw, _ = os.ReadFile(env)
	if _, err := s.ReceiveBundle(bytes.NewReader(raw)); err == nil {
		t.Fatal("不是钉住的主机签的要拒收")
	}
}

func mustSignKey(t *testing.T) []byte {
	t.Helper()
	k, err := SignKey(NewSignKey())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// 暂存期限按原主机给的(冻结时长再加 5 分钟);组新库时写进本机以后的签名密钥。
func TestPrepareTTLAndSignKey(t *testing.T) {
	cl := readyCluster(t, 1)
	m, s := cl.m, cl.cands[0]
	rep, err := s.Prepare(PrepareRequest{Version: m.d.Version, TTL: 3600})
	if err != nil {
		t.Fatal(err)
	}
	var st IncomingState
	s.getJSON(KeyIncoming, &st)
	if d := st.Expires - time.Now().Unix(); d < 3590 || d > 3610 {
		t.Fatalf("暂存期限按原主机给的: %d", d)
	}
	pub, _ := ParsePub(rep.Pub)
	env, _, _ := m.sealBundle(t.TempDir(), pub, rep.Nonce)
	raw, _ := os.ReadFile(env)
	if _, err := s.ReceiveBundle(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	db, err := openScratch(s.stagedFile())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB(db)
	var seed string
	db.Raw("SELECT value FROM settings WHERE key = ?", KeySignKey).Scan(&seed)
	k, err := SignKey(seed)
	if err != nil || PubOf(k) != rep.SignPub {
		t.Fatalf("新库里的签名密钥就是 prepare 时给出公钥的那把: %v", err)
	}
}

// 本机已有的同名订阅证书:比带来的早到期(或已过期)就换成带来的,续期退避清零;更新的照用本机的。
func TestPlaceCertNewerWins(t *testing.T) {
	dir := t.TempDir()
	ext := filepath.Join(dir, "ext")
	os.MkdirAll(filepath.Join(ext, "identity"), 0o700)
	carried := func(days int) {
		t.Helper()
		if err := certutil.GenerateSelfSigned([]string{"sub.example.com"}, filepath.Join(ext, "identity", "sub.crt"), filepath.Join(ext, "identity", "sub.key"), days); err != nil {
			t.Fatal(err)
		}
	}
	own := func(s *Service, days int) {
		t.Helper()
		crt, key := filepath.Join(dir, "own.crt"), filepath.Join(dir, "own.key")
		if err := certutil.GenerateSelfSigned([]string{"sub.example.com"}, crt, key, days); err != nil {
			t.Fatal(err)
		}
		s.d.DB.Create(&model.Cert{Name: "sub.example.com", Source: "acme", CertFile: crt, KeyFile: key, RenewFails: 5, NextRenewAt: 9e9})
	}
	cc := &CarriedCert{Name: "sub.example.com", Source: "acme", Method: "http", AutoRenew: true, CertZip: "identity/sub.crt", KeyZip: "identity/sub.key"}
	// 本机那张 1 天后到期,带来的 60 天:换成带来的
	s := testService(t, false, map[string]string{})
	own(s, 1)
	carried(60)
	crt, _, err := placeCert(s.d.DB, cc, ext, filepath.Join(dir, "data1"))
	if err != nil || crt == filepath.Join(dir, "own.crt") {
		t.Fatalf("本机的早到期:换成带来的: %s %v", crt, err)
	}
	var row model.Cert
	s.d.DB.Where("name = ?", "sub.example.com").First(&row)
	if row.CertFile != crt || row.RenewFails != 0 || row.NextRenewAt != 0 {
		t.Fatalf("那一行改指新文件、续期退避清零: %+v", row)
	}
	// 本机那张更新:照用本机的
	s2 := testService(t, false, map[string]string{})
	own(s2, 90)
	carried(30)
	crt, _, err = placeCert(s2.d.DB, cc, ext, filepath.Join(dir, "data2"))
	if err != nil || crt != filepath.Join(dir, "own.crt") {
		t.Fatalf("本机的更新:照用本机的: %s %v", crt, err)
	}
}

// 新主机已有自己的签证书令牌:不换掉,告警订阅证书续期要它能管那个区域。
func TestAcmeTokenKeptWarns(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "identity"), 0o700)
	if err := certutil.GenerateSelfSigned([]string{"sub.example.com"}, filepath.Join(dir, "identity", "sub.crt"), filepath.Join(dir, "identity", "sub.key"), 30); err != nil {
		t.Fatal(err)
	}
	id := Identity{SubHost: "sub.example.com", SubPort: 2056, AcmeCfToken: "theirs",
		SubCert: &CarriedCert{Name: "sub.example.com", Source: "acme", Method: "cloudflare", CertZip: "identity/sub.crt", KeyZip: "identity/sub.key"}}
	s := testService(t, true, map[string]string{"acmeCfToken": "mine"})
	s.d.DB.Create(&model.Node{Id: 5, Name: "TW", IsLocal: true, Enabled: true})
	res, err := applyIdentity(s.d.DB, id, 5, dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.setting("acmeCfToken") != "mine" || len(res.Warnings) != 1 || res.Warnings[0].Code != idwAcmeZone || res.Warnings[0].Args["name"] != "sub.example.com" {
		t.Fatalf("本机的令牌留着,提前告警: %+v", res.Warnings)
	}
	s2 := testService(t, true, map[string]string{})
	s2.d.DB.Create(&model.Node{Id: 5, Name: "TW", IsLocal: true, Enabled: true})
	res, _ = applyIdentity(s2.d.DB, id, 5, dir, t.TempDir())
	if s2.setting("acmeCfToken") != "theirs" || len(res.Warnings) != 0 {
		t.Fatalf("本机没有令牌:用带来的: %+v", res.Warnings)
	}
}

// 容灾通知不挂在「数据面停止 / 恢复」开关上;超长的截到 Telegram 发得出去的长度。
func TestNotifyIgnoresCoreToggle(t *testing.T) {
	s := testService(t, false, map[string]string{"tgOnCore": "false"})
	var toggle, text string
	s.d.Notify = func(tg, tx string) { toggle, text = tg, tx }
	s.notify("短的")
	if toggle != "" || text != "短的" {
		t.Fatalf("不经开关: %q %q", toggle, text)
	}
	long := strings.Repeat("改了一条记录\n", 1000)
	s.notify(long)
	if n := len([]rune(text)); n > notifyMax || !strings.HasSuffix(text, "…") {
		t.Fatalf("超长要截断: %d", n)
	}
}

// 自动改 DNS 的三态:本机只有 IPv4 时指向原主机的 AAAA 改不了,明确记成跳过、状态为部分完成;订阅主机名不在名单里不说已改好;
// 让出主机时把本任期改过、当前仍指向本机的记录改回原值。
func TestDNSPartialAndRevert(t *testing.T) {
	cf := newFakeCF(t)
	s := testService(t, false, map[string]string{KeyEpoch: "2", KeyDnsToken: "tok", KeyDnsNames: "sub.example.com", "publicIp": "203.0.113.7"})
	s.SetDNSAPI(cf.srv.URL)
	if s.DNSAPIOverride() != cf.srv.URL {
		t.Fatal("测试地址要能看到")
	}
	b := Became{How: HowTakeover, Epoch: 2, FormerIP: "198.51.100.1", FormerIP6: "2001:db8::1", Identity: identityResult{SubURLKept: true, SubHost: "sub.example.com"}}
	out := s.becameDNS(&b, "203.0.113.7")
	if out.State != "partial" || out.SubDone || len(out.Changes) != 1 || len(out.Skipped) != 1 || out.Skipped[0].Type != "AAAA" || out.Skipped[0].Skip != "noV6" {
		t.Fatalf("A 改了、AAAA 跳过:部分完成,订阅主机名没全改好: %+v", out)
	}
	txt := s.dnsText(b, "203.0.113.7")
	if !strings.Contains(txt, "198.51.100.1 → 203.0.113.7") || !strings.Contains(txt, "2001:db8::1") || !strings.Contains(txt, "手动") {
		t.Fatalf("通知里写原值 → 新值,跳过的要手动处理: %q", txt)
	}
	// 让出:改回
	got, err := s.revertDNS(b)
	if err != nil || len(got) != 1 || got[0].From != "203.0.113.7" || got[0].To != "198.51.100.1" {
		t.Fatalf("改回原值: %+v %v", got, err)
	}
	cf.mu.Lock()
	content := cf.records["r1"]["content"]
	cf.mu.Unlock()
	if content != "198.51.100.1" {
		t.Fatalf("记录改回去了: %v", content)
	}
	// 订阅主机名不在名单里:别的名字改好了也不说订阅已改好
	s.set(map[string]string{"publicIp6": "2001:db8::7"})
	b2 := Became{FormerIP: "198.51.100.1", FormerIP6: "2001:db8::1", Identity: identityResult{SubURLKept: true, SubHost: "other.example.com"}}
	out = s.becameDNS(&b2, "203.0.113.7")
	if out.State != "partial" || out.SubDone {
		t.Fatalf("订阅主机名不在名单里:不是全部完成: %+v", out)
	}
	// 没配自动改:要管理员自己改
	s.set(map[string]string{KeyDnsToken: ""})
	if out := s.becameDNS(&b2, "203.0.113.7"); out.State != "todo" {
		t.Fatalf("没配自动改:todo: %+v", out)
	}
}

// Cloudflare 回网页而不是 JSON:错误里只留状态码与去掉标签的前一小段,不把整页带进通知。
func TestCloudflareHTMLErrorTrimmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html><head><title>502 Bad Gateway</title></head><body><h1>Bad gateway</h1>" + strings.Repeat("<p>cloudflare error page</p>", 500) + "</body></html>"))
	}))
	t.Cleanup(srv.Close)
	s := testService(t, false, map[string]string{KeyDnsToken: "tok", KeyDnsNames: "sub.example.com", "publicIp": "203.0.113.7"})
	s.SetDNSAPI(srv.URL)
	b := Became{FormerIP: "198.51.100.1", Identity: identityResult{SubURLKept: true, SubHost: "sub.example.com"}}
	out := s.becameDNS(&b, "203.0.113.7")
	if out.State != "failed" || strings.Contains(out.Err, "<") || len([]rune(out.Err)) > 320 || !strings.Contains(out.Err, "502") {
		t.Fatalf("错误要短、不带标签: %d %q", len(out.Err), out.Err)
	}
}

// 启动时迁来的订阅端口绑不上:改回本机原来的端口,记进成为主机的告警(只在刚换上新库、还没发过通知时)。
func TestStartupSubPortFallback(t *testing.T) {
	s := testService(t, false, map[string]string{"subPort": "2096"})
	if _, ok := s.SubPortFallback(); ok {
		t.Fatal("不是刚成为主机:不兜底")
	}
	s.set(map[string]string{KeyBecame: mustJSON(Became{How: HowSetMaster, Epoch: 2, Identity: identityResult{SubURLKept: true, SubHost: "sub.example.com", PrevSubPort: 2056}})})
	port, ok := s.SubPortFallback()
	if !ok || port != 2056 {
		t.Fatalf("改回本机原来的订阅端口: %d %v", port, ok)
	}
	if err := s.NoteSubPortFallback(2096, 2056, errors.New("address already in use")); err != nil {
		t.Fatal(err)
	}
	var b Became
	s.getJSON(KeyBecame, &b)
	if s.setting("subPort") != "2056" || b.Identity.SubURLKept || len(b.Identity.Warnings) != 1 || b.Identity.Warnings[0].Code != idwSubPortBind {
		t.Fatalf("端口改回、告警记下: port=%s %+v", s.setting("subPort"), b.Identity)
	}
	if _, ok := s.SubPortFallback(); ok {
		t.Fatal("已经是本机原来的端口:不再兜底")
	}
}
