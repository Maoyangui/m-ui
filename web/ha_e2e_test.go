//go:build !race

// 这几条测试在一个进程里起好几台 m-ui:副机收到配置就拉起各自的内嵌数据面,而数据面(core 包)按"一个进程一个"设计,
// 用了进程级的全局变量,几台同时起在竞争检测(-race)下必然报竞争(真机上一个进程只有一个数据面,不会这样)。
// 和 agent_apply_e2e_test.go 一样:普通 go test 照跑,只在 -race 那一步跳过;容灾服务本身的并发由 ha 包的测试在 -race 下覆盖。

package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ha"
)

// 设为主机:对一台已有的副机执行 → 它以新纪元接手全部数据与订阅地址,原主机变为副机继续服务,另一台副机改听它;
// 再对一台刚装好、刚作为副机加进来的全新实例执行,同样接手。
func TestHASetMasterE2E(t *testing.T) {
	clk := &haClock{}
	hk := newHAInst(t, "HK", false, clk)
	tw := newHAInst(t, "TW", true, clk)
	jp := newHAInst(t, "JP", true, clk)
	seedHAMaster(t, hk)
	twId := addNode(t, hk, tw, 2)
	jpId := addNode(t, hk, jp, 3)
	hkId := hk.localId()
	hk.sync()
	hk.sync()
	if st, ok := hk.R().Hub().StatusOf(twId); !ok || !st.Synced || st.HA == nil {
		t.Fatalf("副机要同步上、报容灾信息: %+v", st)
	}
	// 没开过主机容灾(纪元 0)也能设为主机
	plan, err := hk.HA().SetMasterPlan(twId)
	if err != nil || !plan.Ok || plan.NewPanel != tw.url() || plan.SubHost != "sub.example.com" || !plan.SubKept {
		t.Fatalf("检查应通过并讲清新面板地址、订阅域名: %+v %v", plan, err)
	}
	if p, _ := hk.HA().SetMasterPlan(hkId); p.Ok || len(p.Blockers) == 0 || p.Blockers[0].Code != "self" {
		t.Fatalf("主机自己不能设为主机: %+v", p)
	}
	if _, err := hk.HA().SetMaster(twId, "admin", ha.SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitch(t, hk)
	if st.Stage != ha.StageDone || st.NewPanel != tw.url() {
		t.Fatalf("设为主机应完成并给出新面板地址: %+v", st)
	}
	if !tw.restarted() || !hk.restarted() || !hk.isNode() {
		t.Fatal("目标与原主机都要重启,原主机变为副机")
	}
	tw.restart()
	hk.restart()
	if tw.isNode() || tw.setting(ha.KeyEpoch) != "1" {
		t.Fatalf("TW 应以纪元 1 成为主机: node=%v epoch=%s", tw.isNode(), tw.setting(ha.KeyEpoch))
	}
	checkMasterData(t, tw, 1000)
	if !haAudited(tw, "became-master") || !haAudited(hk, "became-node") {
		t.Fatal("换主机要进两边的操作审计")
	}
	if tw.setting("subDomain") != "sub.example.com" || tw.setting("subPort") != "2096" || tw.setting("subPath") != "/s/" {
		t.Fatal("订阅地址相关设置要随主机身份迁移")
	}
	if tw.localId() != twId {
		t.Fatal("新主机的服务器表里本机是 TW")
	}
	var hkRow model.Node
	tw.DB().First(&hkRow, hkId)
	if hkRow.IsLocal || !hkRow.Enabled || hkRow.ApiUrl != hk.url() || hkRow.Token != hk.token || hkRow.CertFP == "" {
		t.Fatalf("原主机那一行要填上作为副机时怎么连: %+v", hkRow)
	}
	tw.sync()
	tw.sync()
	for _, n := range []struct {
		in *haInst
		id uint
	}{{hk, hkId}, {jp, jpId}} {
		if n.in.setting(ha.KeyEpoch) != "1" || n.in.setting(ha.KeyMasterId) != strconv.Itoa(int(twId)) {
			t.Fatalf("%s 要改听 TW(纪元 1): epoch=%s master=%s", n.in.name, n.in.setting(ha.KeyEpoch), n.in.setting(ha.KeyMasterId))
		}
		if st, ok := tw.R().Hub().StatusOf(n.id); !ok || !st.Synced {
			t.Fatalf("%s 要在新主机那里同步上: %+v", n.in.name, st)
		}
	}
	var hkUsers int64
	hk.DB().Model(&model.User{}).Count(&hkUsers)
	if hkUsers != 2 {
		t.Fatal("原主机作为副机照常收到用户表")
	}
	tw.HA().Tick()
	if n := tw.HA().Notices(); len(n) == 0 || n[len(n)-1].Kind != "became" {
		t.Fatalf("新主机面板顶部要有一次性提示: %+v", n)
	}
	if v := hk.HA().View(false); v.Node == nil || v.Node.Switch == nil || v.Node.Switch.NewPanel != tw.url() {
		t.Fatalf("原主机(已是副机)的面板上要看得到结果和新面板地址: %+v", v.Node)
	}

	// 全新实例:装好、作为副机加进来,再设为主机
	nw := newHAInst(t, "NEW", true, clk)
	nwId := addNode(t, tw, nw, 4)
	tw.sync()
	tw.sync()
	if _, err := tw.HA().SetMaster(nwId, "admin", ha.SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	if st := waitSwitch(t, tw); st.Stage != ha.StageDone {
		t.Fatalf("全新实例设为主机应完成: %+v", st)
	}
	nw.restart()
	tw.restart()
	if nw.isNode() || nw.setting(ha.KeyEpoch) != "2" {
		t.Fatal("全新实例应以纪元 2 成为主机")
	}
	checkMasterData(t, nw, 1000)
	nw.sync()
	nw.sync()
	for _, in := range []*haInst{hk, tw, jp} {
		if in.setting(ha.KeyEpoch) != "2" || !in.isNode() {
			t.Fatalf("%s 要改听全新的主机(纪元 2)", in.name)
		}
	}
}

// 设为主机中途失败(推数据那一步对方出错):原主机解冻、照旧是主机、数据不变;目标丢掉暂存。
func TestHASetMasterFailureRecovers(t *testing.T) {
	clk := &haClock{}
	hk := newHAInst(t, "HK", false, clk)
	tw := newHAInst(t, "TW", true, clk)
	seedHAMaster(t, hk)
	twId := addNode(t, hk, tw, 2)
	hk.sync()
	hk.sync()
	tw.setFail("ha/handover")
	if _, err := hk.HA().SetMaster(twId, "admin", ha.SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitch(t, hk)
	if st.Stage != ha.StageFailed || st.Err == "" {
		t.Fatalf("应失败并写明原因: %+v", st)
	}
	if hk.isNode() || hk.HA().Frozen() || hk.restarted() {
		t.Fatal("失败后原主机照旧是主机、已解冻、不重启")
	}
	if tw.HA().IncomingStage() != "" || tw.restarted() {
		t.Fatal("目标要丢掉暂存,不重启")
	}
	if _, err := os.Stat(tw.path + ".promote"); err == nil {
		t.Fatal("目标不能留下待换的新库")
	}
	// 冻结期间(设为主机进行中)写操作 503,读照常
	tw.setFail("")
	hk.DB().Exec("INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		ha.KeyFreezeUntil, strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
	tok := hk.srv.newSession("boss")
	for _, c := range []struct {
		method string
		want   int
	}{{http.MethodPost, http.StatusServiceUnavailable}, {http.MethodGet, http.StatusOK}} {
		req := httptest.NewRequest(c.method, "/app/api/users", strings.NewReader(`{"name":"carol"}`))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		w := httptest.NewRecorder()
		hk.srv.auth(hk.srv.masterOnly(hk.srv.handleUsers))(w, req)
		if w.Code != c.want {
			t.Fatalf("冻结期间 %s 应 %d,实际 %d %s", c.method, c.want, w.Code, w.Body.String())
		}
	}
	hk.DB().Exec("UPDATE settings SET value = '' WHERE key = ?", ha.KeyFreezeUntil)
	var users int64
	hk.DB().Model(&model.User{}).Count(&users)
	if users != 2 {
		t.Fatal("数据不变")
	}
	// 再来一次就成功
	if _, err := hk.HA().SetMaster(twId, "admin", ha.SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	if st := waitSwitch(t, hk); st.Stage != ha.StageDone {
		t.Fatalf("重试应成功: %+v", st)
	}
}

// 主机崩溃,备用接任:副本推送、解密、落盘;租约过期后备用 1 接任(纪元 2),另一台副机改听它、流量补回;
// 原主机回来后在多数副机回 stale 时自动变为副机,新主机把它那一行重新启用、照常管起来;
// 最后对崩溃后回来的原主机「设为主机」,它重新当主机。
func TestHATakeoverE2E(t *testing.T) {
	clk := &haClock{}
	hk := newHAInst(t, "HK", false, clk)
	tw := newHAInst(t, "TW", true, clk)
	jp := newHAInst(t, "JP", true, clk)
	seedHAMaster(t, hk)
	twId := addNode(t, hk, tw, 2)
	jpId := addNode(t, hk, jp, 3)
	hkId := hk.localId()
	hk.sync()
	if err := hk.HA().SaveConfig(ha.ConfigInput{Config: ha.Config{Mode: ha.ModeConfirm, Standbys: 1, ReplicaMinutes: 5, ConfirmMinutes: 3,
		AutoMinutes: 10, RankStepMinutes: 3}}); err != nil {
		t.Fatal(err)
	}
	hk.sync() // 副机切到纪元 1、报备用公钥
	hk.sync()
	if err := hk.HA().ReplicateNow(); err != nil {
		t.Fatalf("副本应发给备用 1(TW): %v", err)
	}
	if v := tw.HA().View(false); v.Node == nil || v.Node.Rank != 1 || v.Node.Replica == nil {
		t.Fatalf("TW 是备用 1、手里有副本: %+v", v.Node)
	}
	if v := jp.HA().View(false); v.Node == nil || v.Node.Rank != 0 || v.Node.Replica != nil {
		t.Fatal("JP 不是备用,不该有副本")
	}
	if err := tw.HA().SelfCheck(); err != nil {
		t.Fatalf("副本自检应通过: %v", err)
	}
	// JP 线路上产生了流量,主机还没来得及并账就崩了
	jp.DB().Create(&model.AgentCounter{UserName: "alice", Up: 50, Down: 0})
	// 主机还在联系:不能接任
	if err := tw.HA().Takeover(ha.TakeoverOpts{By: "test"}); err == nil {
		t.Fatal("主机刚联系过,不能接任")
	}
	hk.setDown(true)
	clk.add(4 * time.Minute)
	if v := tw.HA().View(false); !v.Node.Lost || !v.Node.CanTakeover {
		t.Fatalf("主机失联后备用面板上要能接任: %+v", v.Node)
	}
	if err := tw.HA().Takeover(ha.TakeoverOpts{By: "test"}); err != nil {
		t.Fatal(err)
	}
	if !tw.restarted() {
		t.Fatal("接任要重启")
	}
	tw.restart()
	if tw.isNode() || tw.setting(ha.KeyEpoch) != "2" {
		t.Fatal("TW 应以纪元 2 成为主机")
	}
	checkMasterData(t, tw, 1000)
	if !haAudited(tw, "takeover") {
		t.Fatal("接任要进新主机的操作审计")
	}
	var hkRow model.Node
	tw.DB().First(&hkRow, hkId)
	if hkRow.Enabled || hkRow.ApiUrl != hk.url() {
		t.Fatalf("失联的原主机那一行先停用(不进订阅),记着怎么连它: %+v", hkRow)
	}
	tw.sync()
	tw.sync()
	if jp.setting(ha.KeyEpoch) != "2" {
		t.Fatal("JP 租约已过期,应改听 TW")
	}
	var alice model.User
	tw.DB().Where("name = ?", "alice").First(&alice)
	if alice.Up != 1050 {
		t.Fatalf("JP 账本里的流量要按游标补回: up=%d", alice.Up)
	}
	if v := tw.HA().View(false); v.Former == nil || v.Former.Id != hkId || v.Became == nil || v.Became.How != ha.HowTakeover {
		t.Fatalf("新主机要记着原主机失联、自己怎么成为主机的: %+v %+v", v.Former, v.Became)
	}
	// 原主机回来:多数副机回 stale → 自动变为副机
	hk.setDown(false)
	hk.sync()
	if !hk.isNode() || !hk.restarted() || hk.setting(ha.KeyRole) != ha.RoleDemoted {
		t.Fatal("原主机回来应自动变为副机")
	}
	if !haAudited(hk, "demoted") {
		t.Fatal("原主机变为副机要进它自己的操作审计")
	}
	hk.restart()
	tw.HA().Tick() // 发现原主机回来了:它认了纪元 2,重新启用那一行
	tw.DB().First(&hkRow, hkId)
	if !hkRow.Enabled || tw.HA().View(false).Former != nil {
		t.Fatal("原主机回来后那一行要重新启用")
	}
	if hk.setting(ha.KeyEpoch) != "2" || hk.setting(ha.KeyRole) != "" {
		t.Fatal("原主机要认新主机的纪元,变成普通副机")
	}
	found := false
	for _, n := range tw.HA().Notices() {
		found = found || n.Kind == "formerBack"
	}
	if !found {
		t.Fatal("新主机面板顶部要提示原主机已恢复")
	}
	tw.sync()
	tw.sync()
	if st, ok := tw.R().Hub().StatusOf(hkId); !ok || !st.Synced {
		t.Fatalf("原主机作为副机要同步上: %+v", st)
	}
	_ = jpId
	_ = twId
	// 想让原主机重新当主机:对它「设为主机」
	if _, err := tw.HA().SetMaster(hkId, "admin", ha.SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	if st := waitSwitch(t, tw); st.Stage != ha.StageDone {
		t.Fatalf("对回来的原主机设为主机应完成: %+v", st)
	}
	hk.restart()
	tw.restart()
	if hk.isNode() || hk.setting(ha.KeyEpoch) != "3" {
		t.Fatal("原主机应以纪元 3 重新当主机")
	}
	checkMasterData(t, hk, 1050)
	hk.sync()
	hk.sync()
	for _, in := range []*haInst{tw, jp} {
		if in.setting(ha.KeyEpoch) != "3" {
			t.Fatalf("%s 要改听 HK(纪元 3)", in.name)
		}
	}
}

// 自动接任:只有一台备用连不上主机(另一台副机还连得上)时不接任 —— 副机之间经 HMAC 同伴接口互相确认;
// 主机真的不在了,两台副机都联系不上它,备用 1 拿到过半票(集群 3 台要 2 票)后接任,另一台副机认它。
func TestHAAutoTakeoverPartitionE2E(t *testing.T) {
	hkClk, twClk, jpClk := &haClock{}, &haClock{}, &haClock{}
	hk := newHAInst(t, "HK", false, hkClk)
	tw := newHAInst(t, "TW", true, twClk)
	jp := newHAInst(t, "JP", true, jpClk)
	seedHAMaster(t, hk)
	addNode(t, hk, tw, 2)
	jpId := addNode(t, hk, jp, 3)
	hk.sync()
	if err := hk.HA().SaveConfig(ha.ConfigInput{Config: ha.Config{Mode: ha.ModeAuto, Standbys: 1, ReplicaMinutes: 5, ConfirmMinutes: 3,
		AutoMinutes: 5, RankStepMinutes: 3}}); err != nil {
		t.Fatal(err)
	}
	hk.sync()
	hk.sync()
	if err := hk.HA().ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	hk.sync() // 集群密钥、备用名单随状态到副机
	// 只有 TW 和主机之间断了:JP 还联系得上主机
	twClk.add(6 * time.Minute)
	tw.HA().Tick()
	if tw.restarted() {
		t.Fatal("另一台副机还联系得上主机:不能接任")
	}
	if v := tw.HA().View(false); v.Node.Evidence == nil || v.Node.Evidence.Reason != "peerSeesMaster" || v.Node.Evidence.PeersOK != 1 {
		t.Fatalf("要经同伴接口问到 JP 还联系得上主机: %+v", v.Node.Evidence)
	}
	// 主机真的不在了
	hk.setDown(true)
	jpClk.add(6 * time.Minute)
	twClk.add(time.Minute)
	tw.HA().Tick()
	if !tw.restarted() {
		t.Fatalf("两台副机都联系不上主机、票过半:应接任: %+v", tw.HA().View(false).Node.Evidence)
	}
	tw.restart()
	tw.sync()
	if jp.setting(ha.KeyEpoch) != "2" {
		t.Fatal("投过票的 JP 要认接任的新主机")
	}
	if st, ok := tw.R().Hub().StatusOf(jpId); !ok || !st.Synced {
		t.Fatalf("JP 要在新主机那里同步上: %+v", st)
	}
}
