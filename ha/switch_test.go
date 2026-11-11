package ha

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
)

// masterPanel 给主机一个连得上的面板地址(目标在 prepare 时会试着连一下原主机的面板)。
func masterPanel(t *testing.T, m *Service) {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	m.d.PanelURL = func() string { return srv.URL + "/app/" }
}

func waitSwitchDone(t *testing.T, s *Service) SwitchState {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		st := s.Switch()
		if (!st.Active() || st.Stage == StageUnknown) && !s.Switching() {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("设为主机一直没结束: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 被设为主机的一边:一次性公钥只认这一次;编号对不上、重放、过期都拒;没提交前丢掉暂存不留痕。
func TestIncomingGuards(t *testing.T) {
	cl := newCluster(t, 1)
	m, s := cl.m, cl.cands[0]
	cl.h.RunOnce()
	if _, err := s.Prepare(PrepareRequest{Version: "9.9.9"}); err == nil || !strings.Contains(err.Error(), "升级") {
		t.Fatalf("主机比本机新:要先升级本机: %v", err)
	}
	rep, err := s.Prepare(PrepareRequest{Version: m.d.Version})
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := ParsePub(rep.Pub)
	dir := t.TempDir()
	env, _, err := m.sealBundle(dir, pub, rep.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(env)
	// 封给别的一次性公钥的:拒
	other, _ := NewBoxKey()
	envOther, _, _ := m.sealBundle(t.TempDir(), other.Pub, rep.Nonce)
	rawOther, _ := os.ReadFile(envOther)
	if _, err := s.ReceiveBundle(bytes.NewReader(rawOther)); err == nil {
		t.Fatal("不是封给本机这一次的数据应拒")
	}
	// 上面失败不影响这一次:正确的收下
	if _, err := s.Prepare(PrepareRequest{Version: m.d.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReceiveBundle(bytes.NewReader(raw)); err == nil {
		t.Fatal("重新要过一次公钥后,旧的数据应拒(一次性)")
	}
	rep2, _ := s.Prepare(PrepareRequest{Version: m.d.Version})
	pub2, _ := ParsePub(rep2.Pub)
	env2, _, _ := m.sealBundle(t.TempDir(), pub2, rep2.Nonce)
	raw2, _ := os.ReadFile(env2)
	if st, err := s.ReceiveBundle(bytes.NewReader(raw2)); err != nil || st.Stage != incStaged || st.Users != 1 {
		t.Fatalf("应收下并组好新库: %+v %v", st, err)
	}
	if _, err := s.ReceiveBundle(bytes.NewReader(raw2)); err == nil {
		t.Fatal("同一份数据重放应拒")
	}
	if err := s.Commit("wrong"); err == nil {
		t.Fatal("编号不对不能提交")
	}
	if _, err := os.Stat(s.stagedFile()); err != nil {
		t.Fatal("暂存的新库要在")
	}
	// 过期:丢掉暂存
	clk := &testClock{t: time.Now().Add(incomingTTL + time.Minute)}
	s.SetNow(clk.now)
	s.inc.tick()
	if _, err := os.Stat(s.stagedFile()); err == nil || s.IncomingStage() != "" {
		t.Fatal("过期后暂存要丢掉")
	}
	if err := s.Commit(rep2.Nonce); err == nil {
		t.Fatal("过期后不能再提交")
	}
}

// 设为主机:提交请求没回话时去问目标 —— 目标说还没提交(暂存着)并确认丢掉了就取消、解冻;问不到就是结果不明:
// 保持冻结,到冻结截止以更高纪元隔离目标后解冻,本机继续当主机。
func TestSetMasterCommitUnknown(t *testing.T) {
	w := switchCommitWait
	switchCommitWait = 2 * time.Second
	t.Cleanup(func() { switchCommitWait = w })
	cl := newCluster(t, 1)
	m, s, a := cl.m, cl.cands[0], cl.agents[0]
	masterPanel(t, m) // 目标在 prepare 时要连得上原主机的面板
	cl.h.RunOnce()
	cl.h.RunOnce()
	id := cl.ids[0]
	// 1. 提交请求没处理也没回话,目标还在暂存 → 取消
	a.mu.Lock()
	a.dropCommit = true
	a.mu.Unlock()
	if _, err := m.SetMaster(id, "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitchDone(t, m)
	if st.Stage != StageFailed || m.Frozen() || m.IsNode() {
		t.Fatalf("目标还没提交:应取消、解冻,本机照旧是主机: %+v", st)
	}
	if s.IncomingStage() != "" {
		t.Fatal("目标的暂存要丢掉")
	}
	// 2. 推完数据后目标彻底失联 → 结果不明:保持冻结,不变成副机;冻结超时后解冻
	a.mu.Lock()
	a.dropCommit, a.dieAfterHandover = false, true
	a.mu.Unlock()
	if _, err := m.SetMaster(id, "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st = waitSwitchDone(t, m)
	if st.Stage != StageUnknown || m.IsNode() || !m.Frozen() {
		t.Fatalf("不确定目标接手没有时不能变成副机,也不能解冻: %+v", st)
	}
	L := m.Epoch()
	clk := &testClock{t: time.Unix(st.FreezeUntil, 0).Add(time.Second)}
	m.SetNow(clk.now)
	m.sw.tick()
	if m.Frozen() || m.Switch().Stage != StageFenced || m.Epoch() != L+2 {
		t.Fatalf("冻结截止仍不明:以纪元 L+2 隔离目标后解冻: %+v epoch=%d", m.Switch(), m.Epoch())
	}
}

// 误接任的自愈:多数副机一直还跟着另一台(纪元更低、还活着的)主机、本机又凑不够多数 → 让出主机;
// 有多数副机跟着的主机看到"跟过已经不在的主机"的副机 → 以更高纪元把它们收回来。
func TestYieldAndReclaim(t *testing.T) {
	// 让出:A(纪元 2)只有 1 台副机认它,2 台还跟着原主机
	a := testService(t, false, map[string]string{KeyEpoch: "2", KeyMode: ModeConfirm})
	restarted := 0
	a.d.Restart = func(time.Duration) { restarted++ }
	clk := &testClock{t: time.Now()}
	a.SetNow(clk.now)
	a.d.DB.Create(&model.Node{Name: "A", IsLocal: true, Enabled: true})
	var ids []uint
	for _, n := range []string{"B", "C", "D"} {
		row := model.Node{Name: n, ApiUrl: "https://" + n + "/app/", Token: "t", Enabled: true}
		a.d.DB.Create(&row)
		ids = append(ids, row.Id)
	}
	following := hub.EpochReply{Aware: true, Code: CodeFollowing, Epoch: 1, MasterId: 9}
	round := func() {
		a.NoteReply(model.Node{Id: ids[0]}, hub.EpochReply{Aware: true, NodeEpoch: 2})
		a.NoteReply(model.Node{Id: ids[1]}, following)
		a.NoteReply(model.Node{Id: ids[2]}, following)
	}
	round()
	for i := 0; i < 4; i++ { // 刚接任的两分钟里其它副机租约还没过期,回 still_following 很正常:不能马上让出
		clk.add(30 * time.Second)
		round()
	}
	if a.IsNode() {
		t.Fatal("还没过租约期再加一分钟,不能让出")
	}
	for i := 0; i < 4; i++ {
		clk.add(30 * time.Second)
		round()
	}
	if !a.IsNode() || restarted != 1 || a.setting(KeyRole) != RoleDemoted {
		t.Fatal("多数副机一直跟着另一台活着的主机:本机要让出")
	}
	var d Demoted
	if !a.getJSON(KeyDemoted, &d) || !d.Yield {
		t.Fatalf("记下是让出: %+v", d)
	}

	// 收回:M(纪元 1)有 2 台副机认它,1 台跟过纪元 2 的主机、那台主机早已不在(租约过期 5 分钟)
	cl := newCluster(t, 3)
	m := cl.m
	cl.h.RunOnce()
	if m.Epoch() != 1 {
		t.Fatal("前提:M 在纪元 1")
	}
	orphan := cl.ids[2]
	cl.cands[2].set(map[string]string{KeyEpoch: "2", KeyMasterId: "77"})
	cl.cands[2].SetNow(func() time.Time { return time.Now().Add(5 * time.Minute) })
	cl.h.RunOnce() // 两台回 ok,第三台回 stale(纪元 2,租约过期)→ 收回
	if m.Epoch() != 3 {
		t.Fatalf("应以纪元 3 收回: %d", m.Epoch())
	}
	cl.h.RunOnce()
	for i, c := range cl.cands {
		if c.Epoch() != 3 {
			t.Fatalf("副机 %d 要跟着 M 升到纪元 3: %d", i, c.Epoch())
		}
	}
	_ = orphan
}

// 设为主机前的检查:默认端口不算冲突;迁来的订阅端口、代理面板端口被目标上的线路占着要提前说;离线、版本旧的不能设。
func TestSetMasterPlanChecks(t *testing.T) {
	cl := newCluster(t, 1)
	m := cl.m
	masterPanel(t, m)
	cl.h.RunOnce()
	cl.h.RunOnce()
	id := cl.ids[0]
	m.d.DB.Model(&model.Node{}).Where("id = ?", id).Update("service_ports", []byte(`[{"label":"面板","port":2053},{"label":"订阅","port":2056}]`))
	p, err := m.SetMasterPlan(id)
	if err != nil || !p.Ok {
		t.Fatalf("应能设为主机: %+v %v", p, err)
	}
	for _, w := range p.Warnings {
		if w.Code == "subPort" || w.Code == "resellerPort" {
			t.Fatalf("默认端口不算冲突: %+v", p.Warnings)
		}
	}
	m.set(map[string]string{"subPort": "2096"}) // 本机的订阅端口迁过去会落在目标的线路上
	m.d.DB.Create(&model.Line{Name: "on-2096", Protocol: "vless", Port: 2096, Enabled: true, NodeIds: []byte(`[` + strconv.Itoa(int(id)) + `]`)})
	m.d.DB.Create(&model.Line{Name: "on-2054", Protocol: "trojan", Port: 2054, Enabled: true, NodeIds: []byte(`[` + strconv.Itoa(int(id)) + `]`)})
	p, _ = m.SetMasterPlan(id)
	codes := map[string]bool{}
	for _, w := range p.Warnings {
		codes[w.Code] = true
	}
	if !codes["subPort"] || !codes["resellerPort"] || p.SubKept {
		t.Fatalf("线路占着订阅端口、代理面板端口要提前说: %+v", p.Warnings)
	}
	cl.agents[0].setDown(true)
	cl.h.RunOnce()
	if p, _ := m.SetMasterPlan(id); p.Ok || len(p.Blockers) == 0 || p.Blockers[0].Code != "offline" {
		t.Fatalf("离线的不能设为主机: %+v", p.Blockers)
	}
}
