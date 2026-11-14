//go:build !race

// 修复者 B 的多实例进程内端到端测试(设为主机流程、身份迁移、DNS)。和 ha_e2e_test.go 一样只在 -race 那一步跳过。

package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/runner"
)

// haProxy 主机经它连一台实例:before 在转给那台之前调;lose 为真的动作照常处理、但回话丢了(连接直接断),处理完调 after。
type haProxy struct {
	srv    *httptest.Server
	mu     sync.Mutex
	before func(action string)
	lose   func(action string) bool
	after  func(action string)
}

func (p *haProxy) set(before func(string), lose func(string) bool, after func(string)) {
	p.mu.Lock()
	p.before, p.lose, p.after = before, lose, after
	p.mu.Unlock()
}

func newHAProxy(t *testing.T, in *haInst) *haProxy {
	t.Helper()
	p := &haProxy{}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Path
		if k := strings.Index(action, "/agent/"); k >= 0 {
			action = action[k+len("/agent/"):]
		}
		p.mu.Lock()
		before, lose, after := p.before, p.lose, p.after
		p.mu.Unlock()
		if before != nil {
			before(action)
		}
		if lose != nil && lose(action) {
			in.serve(httptest.NewRecorder(), r)
			if after != nil {
				after(action)
			}
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
					return
				}
			}
			return
		}
		in.serve(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// addNodeVia 主机的服务器表里加一台副机,经 proxy 连它。
func addNodeVia(t *testing.T, m, n *haInst, p *haProxy, sort int) uint {
	t.Helper()
	row := model.Node{Name: n.name, ApiUrl: p.srv.URL + "/app/", Token: n.token, Insecure: true, Enabled: true, Sort: sort}
	if err := m.DB().Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.Id
}

// waitRunDone 等设为主机的后台步骤跑完(结果不明时进度还"进行中",不能用 waitSwitch)。
func waitRunDone(t *testing.T, m *haInst) ha.SwitchState {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		st := m.HA().Switch()
		if st.Stage != ha.StagePrepare && !m.HA().Switching() {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("设为主机一直没跑完: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// adminWrite 以管理员会话发一个写请求(建用户),返回状态码。
func adminWrite(t *testing.T, in *haInst, name string) int {
	t.Helper()
	tok := in.srv.newSession("boss")
	req := httptest.NewRequest(http.MethodPost, "/app/api/users", strings.NewReader(`{"name":"`+name+`"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	w := httptest.NewRecorder()
	in.srv.auth(in.srv.masterOnly(in.srv.handleUsers))(w, req)
	return w.Code
}

func setKV(t *testing.T, in *haInst, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		if err := in.DB().Exec("INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", k, v).Error; err != nil {
			t.Fatal(err)
		}
	}
}

// E10:提交的回话丢了,目标随后也联系不上(还没起来)→ 结果不明:原主机保持冻结;冻结截止仍不明 → 以 E+2 隔离目标、解冻,
// 别的副机跟着到 E+2。目标在那之后才起来(以 E+1 当主机)→ 副机都回 stale,它自动变为副机;原主机数据完整、写操作恢复。
func TestHASetMasterCommitLostE2E(t *testing.T) {
	defer ha.SetSwitchCommitWait(2 * time.Second)()
	hkClk := &haClock{}
	hk := newHAInst(t, "HK", false, hkClk)
	tw := newHAInst(t, "TW", true, &haClock{})
	jp := newHAInst(t, "JP", true, &haClock{})
	seedHAMaster(t, hk)
	px := newHAProxy(t, tw)
	twId := addNodeVia(t, hk, tw, px, 2)
	addNode(t, hk, jp, 3)
	hk.sync()
	hk.sync()
	isCommit := func(action string) bool { return action == "ha/commit" }
	px.set(nil, isCommit, func(string) { tw.setDown(true) }) // 提交做了,回话丢了,之后它也联系不上
	if _, err := hk.HA().SetMaster(twId, "admin", ha.SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitRunDone(t, hk)
	if st.Stage != ha.StageUnknown || hk.isNode() || !hk.HA().Frozen() {
		t.Fatalf("提交结果不明:保持冻结,不变成副机: %+v", st)
	}
	if code := adminWrite(t, hk, "carol"); code != http.StatusServiceUnavailable {
		t.Fatalf("结果不明期间写操作暂停: %d", code)
	}
	L := hk.HA().Epoch()
	hkClk.add(time.Duration(st.FreezeUntil-time.Now().Unix()+5) * time.Second) // 冻结截止仍不明
	hk.HA().Tick()
	if st := hk.HA().Switch(); st.Stage != ha.StageFenced || hk.HA().Epoch() != L+2 || hk.HA().Frozen() {
		t.Fatalf("截止后以 E+2 隔离目标、解冻: %+v epoch=%d", st, hk.HA().Epoch())
	}
	hk.sync()
	if jp.setting(ha.KeyEpoch) != strconv.FormatUint(L+2, 10) {
		t.Fatalf("JP 跟着原主机到 E+2: %s", jp.setting(ha.KeyEpoch))
	}
	// 目标这时才起来:换上提交时的新库,以 E+1 当主机 —— 副机都不认它,它自动变为副机
	tw.setDown(false)
	tw.restart()
	if tw.isNode() || tw.setting(ha.KeyEpoch) != strconv.FormatUint(L+1, 10) {
		t.Fatalf("前提:目标以 E+1 起来当主机: node=%v epoch=%s", tw.isNode(), tw.setting(ha.KeyEpoch))
	}
	tw.sync()
	if !tw.isNode() || tw.setting(ha.KeyRole) != ha.RoleDemoted {
		t.Fatalf("目标被副机拒绝,自动变为副机: node=%v role=%s", tw.isNode(), tw.setting(ha.KeyRole))
	}
	if hk.isNode() || hk.HA().Epoch() != L+2 {
		t.Fatal("原主机照旧是主机")
	}
	var users int64
	hk.DB().Model(&model.User{}).Count(&users)
	if users != 2 {
		t.Fatalf("原主机的数据完整: %d", users)
	}
	if code := adminWrite(t, hk, "dave"); code == http.StatusServiceUnavailable {
		t.Fatal("隔离后写操作恢复")
	}
}

// E11:推数据推得太久,越过冻结截止(交接、提交前各要留 2 分钟)→ 不发交接标记、不提交;目标丢掉暂存;原主机解冻、照旧是主机。
func TestHASetMasterFreezeExpiryE2E(t *testing.T) {
	hkClk := &haClock{}
	hk := newHAInst(t, "HK", false, hkClk)
	tw := newHAInst(t, "TW", true, &haClock{})
	jp := newHAInst(t, "JP", true, &haClock{})
	seedHAMaster(t, hk)
	px := newHAProxy(t, tw)
	twId := addNodeVia(t, hk, tw, px, 2)
	addNode(t, hk, jp, 3)
	hk.sync()
	hk.sync()
	var commits atomic.Int32
	px.set(func(action string) {
		switch action {
		case "ha/handover":
			hkClk.add(61 * time.Minute) // 发得慢:发完时冻结已经到期
		case "ha/commit":
			commits.Add(1)
		}
	}, nil, nil)
	if _, err := hk.HA().SetMaster(twId, "admin", ha.SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitch(t, hk)
	if st.Stage != ha.StageFailed || st.GrantSent || hk.isNode() || hk.HA().Frozen() || hk.restarted() {
		t.Fatalf("越过冻结截止:中止、解冻、照旧是主机: %+v", st)
	}
	if commits.Load() != 0 {
		t.Fatal("冻结到期后绝不能提交")
	}
	if jp.setting(ha.KeyGrant) != "" {
		t.Fatal("交接标记不能发出去")
	}
	if tw.HA().IncomingStage() != "" || tw.restarted() {
		t.Fatal("目标丢掉暂存、不重启")
	}
	if code := adminWrite(t, hk, "carol"); code == http.StatusServiceUnavailable {
		t.Fatal("写操作恢复")
	}
}

// E12:没开过容灾的集群设为主机:先进入纪元 1;提交的回话丢了,而目标已经以纪元 2 起来 —— 原主机问它时收到 409(主机就是它),
// 认出它已接手,照常变为副机(不是"失联被接任",也不解冻成"没完成")。
func TestHASetMasterEpochZeroE2E(t *testing.T) {
	defer ha.SetSwitchCommitWait(20 * time.Second)()
	hk := newHAInst(t, "HK", false, &haClock{})
	tw := newHAInst(t, "TW", true, &haClock{})
	jp := newHAInst(t, "JP", true, &haClock{})
	seedHAMaster(t, hk)
	px := newHAProxy(t, tw)
	twId := addNodeVia(t, hk, tw, px, 2)
	jpId := addNode(t, hk, jp, 3)
	hk.sync()
	hk.sync()
	if hk.HA().Epoch() != 0 {
		t.Fatal("前提:没开过容灾")
	}
	restarted := make(chan struct{})
	px.set(nil, func(action string) bool { return action == "ha/commit" }, func(string) {
		go func() { // 回话丢了;它照常重启,以新纪元当主机
			time.Sleep(200 * time.Millisecond)
			tw.restart()
			close(restarted)
		}()
	})
	if _, err := hk.HA().SetMaster(twId, "admin", ha.SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitch(t, hk)
	<-restarted
	if st.Stage != ha.StageDone || st.Epoch != 2 || !hk.isNode() {
		t.Fatalf("认出目标已接手,本机变为副机: %+v", st)
	}
	if tw.isNode() || tw.setting(ha.KeyEpoch) != "2" {
		t.Fatalf("目标以纪元 2 当主机: %s", tw.setting(ha.KeyEpoch))
	}
	n := hk.HA().Notices()
	if len(n) == 0 || n[len(n)-1].Kind != "switched" || hk.setting(ha.KeyDemoted) != "" {
		t.Fatalf("是换主机完成,不是失联被接任: %+v", n)
	}
	hk.restart()
	tw.sync()
	tw.sync()
	for _, x := range []struct {
		in *haInst
		id uint
	}{{hk, hk.localId()}, {jp, jpId}} {
		if x.in.setting(ha.KeyEpoch) != "2" || x.in.setting(ha.KeyMasterId) != strconv.Itoa(int(twId)) {
			t.Fatalf("%s 改听 TW(纪元 2): %s %s", x.in.name, x.in.setting(ha.KeyEpoch), x.in.setting(ha.KeyMasterId))
		}
	}
}

// E13:目标的面板端口 = 原主机的订阅端口:确认框如实说(订阅会用目标自己的订阅端口、订阅地址会变,不挡);
// 新主机照常起来,用自己的订阅端口,成为主机的提示里带着这条告警。
func TestHASetMasterPortConflictE2E(t *testing.T) {
	hk := newHAInst(t, "HK", false, &haClock{})
	tw := newHAInst(t, "TW", true, &haClock{})
	seedHAMaster(t, hk)
	twId := addNode(t, hk, tw, 2)
	twWeb := tw.setting("webPort")
	setKV(t, hk, map[string]string{"subPort": twWeb})
	hk.sync()
	hk.sync()
	p, err := hk.HA().SetMasterPlan(twId)
	if err != nil || !p.Ok || p.SubKept {
		t.Fatalf("端口撞了只提示不挡,订阅地址会变: %+v %v", p, err)
	}
	found := false
	for _, w := range p.Warnings {
		found = found || (w.Code == "subPortSvc" && w.Args["svc"] == "panel" && w.Args["port"] == twWeb && w.Args["own"] == "2056")
	}
	if !found {
		t.Fatalf("确认框说清:订阅端口和它的面板端口相同,它会用自己的 2056: %+v", p.Warnings)
	}
	if _, err := hk.HA().SetMaster(twId, "admin", ha.SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	if st := waitSwitch(t, hk); st.Stage != ha.StageDone || st.SubKept {
		t.Fatalf("设为主机完成,订阅地址会变: %+v", st)
	}
	tw.restart()
	if tw.isNode() || tw.setting("webPort") != twWeb || tw.setting("subPort") == twWeb {
		t.Fatalf("新主机照常起来,面板端口不变,订阅不占面板端口: web=%s sub=%s", tw.setting("webPort"), tw.setting("subPort"))
	}
	tw.HA().Tick()
	n := tw.HA().Notices()
	if len(n) == 0 || n[len(n)-1].Kind != "became" || n[len(n)-1].SubKept {
		t.Fatalf("成为主机的提示:订阅地址会变: %+v", n)
	}
	got := false
	for _, it := range n[len(n)-1].Items {
		got = got || (it.Code == "subPortSvc" && it.Args["svc"] == "panel")
	}
	if !got {
		t.Fatalf("提示里带端口告警(代码 + 参数): %+v", n[len(n)-1].Items)
	}
}

// E14:原主机的面板域名就是订阅域名(换主机后这个名字指向新主机):原主机变为副机后按它的公网 IP 连,新主机照样管得到它;
// 那一行的域名(随主机走的名字)清空,原主机线路的用户不会被指到新主机。
func TestHAFormerLinkAvoidsMovedDomainE2E(t *testing.T) {
	hk := newHAInst(t, "HK", false, &haClock{})
	tw := newHAInst(t, "TW", true, &haClock{})
	seedHAMaster(t, hk)
	setKV(t, hk, map[string]string{"webDomain": "sub.example.com"}) // 这个名字解析不到本机(测试里根本解析不到):用它就连不上
	hkId := hk.localId()
	hk.DB().Model(&model.Node{}).Where("id = ?", hkId).Update("domain", "sub.example.com")
	twId := addNode(t, hk, tw, 2)
	hk.sync()
	hk.sync()
	if !strings.Contains(hk.R().PanelURL(), "sub.example.com") {
		t.Fatalf("前提:面板地址用的是域名: %s", hk.R().PanelURL())
	}
	if _, err := hk.HA().SetMaster(twId, "admin", ha.SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	if st := waitSwitch(t, hk); st.Stage != ha.StageDone {
		t.Fatalf("设为主机应完成: %+v", st)
	}
	tw.restart()
	hk.restart()
	var row model.Node
	tw.DB().First(&row, hkId)
	if !strings.Contains(row.ApiUrl, "127.0.0.1") || strings.Contains(row.ApiUrl, "sub.example.com") || row.Domain != "" || row.CertFP == "" {
		t.Fatalf("原主机那一行按公网 IP 连、钉证书指纹,随主机走的域名清空: %+v", row)
	}
	tw.sync()
	tw.sync()
	if st, ok := tw.R().Hub().StatusOf(hkId); !ok || !st.OK || !st.Synced {
		t.Fatalf("新主机连得上原主机,那一行在线: %+v", st)
	}
}

// fakeCloudflare 假的 Cloudflare:example.com 一个区域;别的区域令牌看不到。
type fakeCloudflare struct {
	mu      sync.Mutex
	srv     *httptest.Server
	records map[string]map[string]interface{}
	patches []string
}

func newFakeCloudflare(t *testing.T) *fakeCloudflare {
	t.Helper()
	f := &fakeCloudflare{records: map[string]map[string]interface{}{
		"a1":  {"id": "a1", "type": "A", "name": "sub.example.com", "content": "198.51.100.1", "proxied": true, "ttl": 1},
		"aa1": {"id": "aa1", "type": "AAAA", "name": "sub.example.com", "content": "2001:db8::1", "proxied": true, "ttl": 1},
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ok := func(v interface{}) { json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "result": v}) }
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "example.com" {
				ok([]map[string]string{{"id": "z1", "name": "example.com"}})
				return
			}
			ok([]map[string]string{})
		case r.Method == http.MethodGet && r.URL.Path == "/zones/z1/dns_records":
			out := []map[string]interface{}{}
			for _, rec := range f.records {
				if rec["name"] == r.URL.Query().Get("name") && rec["type"] == r.URL.Query().Get("type") {
					out = append(out, rec)
				}
			}
			ok(out)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/zones/z1/dns_records/"):
			id := strings.TrimPrefix(r.URL.Path, "/zones/z1/dns_records/")
			b, _ := io.ReadAll(r.Body)
			var body map[string]interface{}
			json.Unmarshal(b, &body)
			f.patches = append(f.patches, id+" "+string(b))
			for k, v := range body {
				f.records[id][k] = v
			}
			ok(f.records[id])
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"success":false,"errors":[{"message":"not found"}]}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// E15:假 Cloudflare(经只供测试的环境变量 M_UI_TEST_CF_API 接上,审计里记一条):设为主机后新主机改 DNS ——
// 指向原主机 IPv4 的 A 记录改到本机;本机没有 IPv6,指向原主机的 AAAA 改不了、明确跳过;另一个区域令牌看不到 → 部分完成并列明。
func TestHADNSMockE2E(t *testing.T) {
	cf := newFakeCloudflare(t)
	t.Setenv(runner.TestCFAPIEnv, cf.srv.URL)
	hk := newHAInst(t, "HK", false, &haClock{})
	tw := newHAInst(t, "TW", true, &haClock{})
	seedHAMaster(t, hk)
	// 原主机的公网 IP 是 198.51.100.1 / 2001:db8::1(面板地址照样用 127.0.0.1 连);新主机只有 IPv4 203.0.113.7
	setKV(t, hk, map[string]string{"webDomain": "127.0.0.1", "publicIp": "198.51.100.1", "publicIp6": "2001:db8::1",
		ha.KeyDnsToken: "cf-test", ha.KeyDnsNames: "sub.example.com,api.other.com"})
	setKV(t, tw, map[string]string{"publicIp": "203.0.113.7"})
	twId := addNode(t, hk, tw, 2)
	hk.sync()
	hk.sync()
	if v := tw.HA().DNSAPIOverride(); v != cf.srv.URL {
		t.Fatalf("环境变量里的测试地址要接上: %q", v)
	}
	if !haAudited(tw, "cf-api-override") {
		t.Fatal("用测试地址要进操作审计")
	}
	p, _ := hk.HA().SetMasterPlan(twId)
	if !p.DnsAuto || p.TargetIP != "203.0.113.7" {
		t.Fatalf("订阅主机名在名单里:会自动改,改到目标探测到的公网 IP: %+v", p)
	}
	if _, err := hk.HA().SetMaster(twId, "admin", ha.SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	if st := waitSwitch(t, hk); st.Stage != ha.StageDone {
		t.Fatalf("设为主机应完成: %+v", st)
	}
	tw.restart()
	tw.HA().Tick() // 刚成为主机:改 DNS、发通知、留提示
	cf.mu.Lock()
	a, aaaa := cf.records["a1"]["content"], cf.records["aa1"]["content"]
	patches := append([]string(nil), cf.patches...)
	cf.mu.Unlock()
	if a != "203.0.113.7" || aaaa != "2001:db8::1" || len(patches) != 1 {
		t.Fatalf("只改 A(本机没有 IPv6,AAAA 不动): a=%v aaaa=%v %v", a, aaaa, patches)
	}
	n := tw.HA().Notices()
	if len(n) == 0 {
		t.Fatal("要有成为主机的提示")
	}
	last := n[len(n)-1]
	if last.Kind != "became" || last.DnsState != "partial" || last.Dns {
		t.Fatalf("部分完成,订阅主机名没全改好: %+v", last)
	}
	changed, skipped := false, false
	for _, c := range last.Changes {
		changed = changed || (c.Type == "A" && c.From == "198.51.100.1" && c.To == "203.0.113.7")
		skipped = skipped || (c.Type == "AAAA" && c.Skip == "noV6" && c.To == "")
	}
	if !changed || !skipped {
		t.Fatalf("列明改了的(原值 → 新值)与跳过的: %+v", last.Changes)
	}
	var b ha.Became
	raw := tw.setting(ha.KeyBecame)
	if json.Unmarshal([]byte(raw), &b) != nil || !strings.Contains(b.DnsErr, "api.other.com") {
		t.Fatalf("看不到的区域写进错误: %s", raw)
	}
}
