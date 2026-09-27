package reach

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// ---- 纯逻辑 ----

func mkProbe(country, city string, asn int, network string, tags ...string) probe {
	var p probe
	p.Location.Country, p.Location.City, p.Location.ASN, p.Location.Network = country, city, asn, network
	p.Tags = tags
	return p
}

func TestCarrierOf(t *testing.T) {
	cases := []struct {
		asn  int
		net  string
		want string
	}{
		{9808, "", GroupMobile}, {56046, "whatever", GroupMobile}, {99999, "Henan Mobile Communications", GroupMobile},
		{4134, "", GroupTelecom}, {99998, "Chinanet Backbone", GroupTelecom}, {99997, "China Telecom Guangdong", GroupTelecom},
		{4837, "", GroupUnicom}, {99996, "CHINA UNICOM Industrial Internet Backbone", GroupUnicom},
		{45090, "Shenzhen Tencent Computer Systems Company", ""}, {37963, "Hangzhou Alibaba Advertising", ""},
	}
	for _, c := range cases {
		if got := carrierOf(c.asn, c.net); got != c.want {
			t.Errorf("carrierOf(%d, %q) = %q, want %q", c.asn, c.net, got, c.want)
		}
	}
}

// 机房测点(datacenter-network)一律不选;每家至多 4 个;先铺开不同城市。
func TestPickLocationsEyeballOnlyAndSpread(t *testing.T) {
	list := []probe{
		mkProbe("CN", "Beijing", 45090, "Shenzhen Tencent Computer Systems Company", "datacenter-network"),
		mkProbe("CN", "Nanjing", 4134, "Chinanet Backbone", "datacenter-network"), // 电信的机房测点也不要
		mkProbe("CN", "Guangzhou", 4134, "Chinanet Backbone", "eyeball-network"),
		mkProbe("CN", "Guangzhou", 4134, "Chinanet Backbone", "eyeball-network"),
		mkProbe("CN", "Guangzhou", 4134, "Chinanet Backbone", "eyeball-network"),
		mkProbe("CN", "Xi'an", 4134, "Chinanet Backbone", "eyeball-network"),
		mkProbe("CN", "Guilin", 4134, "China Telecom", "eyeball-network"),
		mkProbe("CN", "Shanghai", 9808, "China Mobile Communications Group", "eyeball-network"),
		mkProbe("CN", "Wuxi", 56046, "China Mobile Communications", "eyeball-network"),
		mkProbe("CN", "Changsha", 4837, "CHINA UNICOM China169 Backbone", "eyeball-network"),
		mkProbe("CN", "Shenzhen", 208414, "WEDOS Internet", "eyeball-network"), // 不是三家
		mkProbe("HK", "Hong Kong", 4760, "PCCW", "eyeball-network"),
	}
	locs, planned := pickLocations(list, 4)
	if planned[GroupTelecom] != 4 || planned[GroupMobile] != 2 || planned[GroupUnicom] != 1 {
		t.Fatalf("planned = %v", planned)
	}
	got := map[string]int{}
	for _, l := range locs {
		if l.Country != "CN" || len(l.Tags) != 1 || l.Tags[0] != eyeballTag {
			t.Fatalf("选点条件必须限定大陆家宽: %+v", l)
		}
		got[fmt.Sprintf("%d/%s", l.ASN, l.City)] += l.Limit
	}
	// 电信 4 个:三个城市各一个,多出来的那个给测点最多的广州
	if got["4134/Guangzhou"] != 2 || got["4134/Xi'an"] != 1 || got["4134/Guilin"] != 1 {
		t.Fatalf("电信选点没有先铺开城市: %v", got)
	}
	if _, bad := got["4134/Nanjing"]; bad {
		t.Fatal("选了机房测点")
	}
}

type fakeResult struct {
	country, city, network string
	asn                    int
	ok                     bool
	offline                bool
}

func fakeMeasurement(rs []fakeResult) *measurement {
	b, _ := json.Marshal(fakeBody("m1", rs))
	var m measurement
	_ = json.Unmarshal(b, &m)
	return &m
}

func fakeBody(id string, rs []fakeResult) map[string]interface{} {
	results := []interface{}{}
	for _, r := range rs {
		res := map[string]interface{}{"status": "finished"}
		switch {
		case r.offline:
			res = map[string]interface{}{"status": "offline"}
		case r.ok:
			res["stats"] = map[string]interface{}{"avg": 42.34, "loss": 0, "rcv": 3, "total": 3}
		default:
			res["stats"] = map[string]interface{}{"avg": nil, "loss": 100, "rcv": 0, "total": 3}
		}
		results = append(results, map[string]interface{}{
			"probe":  map[string]interface{}{"country": r.country, "city": r.city, "asn": r.asn, "network": r.network, "tags": []string{"eyeball-network"}},
			"result": res,
		})
	}
	return map[string]interface{}{"id": id, "status": "finished", "results": results}
}

func cn(asn int, ok bool) fakeResult {
	return fakeResult{country: "CN", city: "X", asn: asn, ok: ok}
}
func hk(ok bool) fakeResult { return fakeResult{country: "HK", city: "Hong Kong", asn: 4760, ok: ok} }

func TestVerdict(t *testing.T) {
	allDown := []fakeResult{cn(9808, false), cn(9808, false), cn(4134, false), cn(4837, false), hk(true)}
	cases := []struct {
		name     string
		rs       []fakeResult
		icmp     *ICMPCheck
		icmpOnly bool
		want     string
	}{
		{"三网都通", []fakeResult{cn(9808, true), cn(4134, true), cn(4837, true), hk(true)}, nil, false, VerdictOK},
		{"一个测点抽风不算不通", []fakeResult{cn(9808, true), cn(9808, false), cn(4134, true), cn(4837, true), hk(true)}, nil, false, VerdictOK},
		{"移动整家不通", []fakeResult{cn(9808, false), cn(9808, false), cn(4134, true), cn(4837, true), hk(true)}, nil, false, VerdictPartial},
		{"三网不通、还没 ping", allDown, nil, false, VerdictBlocked},
		{"三网不通、ping 也不通", allDown, &ICMPCheck{CNOK: 0, CNTotal: 4, HKOK: 3, HKTotal: 3}, false, VerdictIPBlocked},
		{"三网不通、ping 通", allDown, &ICMPCheck{CNOK: 3, CNTotal: 4, HKOK: 3, HKTotal: 3}, false, VerdictPortBlocked},
		{"ping 香港也不通:分不清", allDown, &ICMPCheck{CNOK: 0, CNTotal: 4, HKOK: 0, HKTotal: 3}, false, VerdictBlocked},
		{"只能 ping 的服务器三网不通", allDown, nil, true, VerdictBlocked},
		{"香港也不通", []fakeResult{cn(9808, false), cn(4134, false), cn(4837, false), hk(false)}, nil, false, VerdictAbroadDown},
		{"只有一家有测点且不通", []fakeResult{cn(9808, false), hk(true)}, nil, false, VerdictPartial},
		{"大陆测点全部离线", []fakeResult{{country: "CN", asn: 9808, offline: true}, hk(true)}, nil, false, VerdictNoProbes},
	}
	for _, c := range cases {
		gs := summarize(fakeMeasurement(c.rs))
		if got := verdict(gs, c.icmp, c.icmpOnly); got != c.want {
			t.Errorf("%s: verdict = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestSummarizeSkipsOfflineAndAverages(t *testing.T) {
	gs := summarize(fakeMeasurement([]fakeResult{cn(9808, true), {country: "CN", asn: 9808, offline: true}, cn(9808, false), hk(true)}))
	g := gs[GroupMobile]
	if g.Total != 2 || g.OK != 1 || g.State != StateOK || g.AvgMs != 42.3 || len(g.Probes) != 3 {
		t.Fatalf("移动组汇总不对: %+v", g)
	}
	if gs[GroupTelecom].State != StateNone {
		t.Fatalf("没有测点的组应当是 none: %+v", gs[GroupTelecom])
	}
}

func TestClampMinutes(t *testing.T) {
	for in, want := range map[string]int{"": 90, "abc": 90, "0": 90, "-5": 90, "10": 30, "30": 30, "90": 90, "2000": 1440, " 45 ": 45} {
		if got := ClampMinutes(in); got != want {
			t.Errorf("ClampMinutes(%q) = %d, want %d", in, got, want)
		}
	}
}

// ---- 假的 Globalping ----

type fakeAPI struct {
	mu        sync.Mutex
	srv       *httptest.Server
	seq       int
	bodies    map[string]map[string]interface{}
	requests  []measureRequest
	tcpOK     map[string]bool // group → 连端口通不通(键:cm ct cu hk)
	icmpOK    map[string]bool
	remaining int
	token     string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{bodies: map[string]map[string]interface{}{}, remaining: 250,
		tcpOK: map[string]bool{"cm": true, "ct": true, "cu": true, "hk": true}, icmpOK: map[string]bool{"cm": true, "ct": true, "cu": true, "hk": true}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) set(tcp, icmp map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tcpOK, f.icmpOK = tcp, icmp
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = r.Header.Get("Authorization")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/probes":
		list := []probe{
			mkProbe("CN", "Beijing", 9808, "China Mobile Communications Group", "eyeball-network"),
			mkProbe("CN", "Shanghai", 9808, "China Mobile Communications Group", "eyeball-network"),
			mkProbe("CN", "Guangzhou", 4134, "Chinanet Backbone", "eyeball-network"),
			mkProbe("CN", "Nanjing", 4134, "Chinanet Backbone", "eyeball-network"),
			mkProbe("CN", "Changsha", 4837, "CHINA UNICOM China169 Backbone", "eyeball-network"),
			mkProbe("CN", "Beijing", 45090, "Shenzhen Tencent Computer Systems Company", "datacenter-network"),
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodPost && r.URL.Path == "/measurements":
		var req measureRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.requests = append(f.requests, req)
		if f.remaining <= 0 {
			w.Header().Set("X-RateLimit-Reset", "1200")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
			return
		}
		f.remaining -= 8
		w.Header().Set("X-RateLimit-Limit", "250")
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(f.remaining))
		w.Header().Set("X-RateLimit-Reset", "1800")
		f.seq++
		id := fmt.Sprintf("m%d", f.seq)
		ok := f.tcpOK
		if req.Options.Protocol == "" {
			ok = f.icmpOK
		}
		f.bodies[id] = fakeBody(id, []fakeResult{
			{country: "CN", city: "Beijing", asn: 9808, network: "China Mobile Communications Group", ok: ok["cm"]},
			{country: "CN", city: "Shanghai", asn: 9808, network: "China Mobile Communications Group", ok: ok["cm"]},
			{country: "CN", city: "Guangzhou", asn: 4134, network: "Chinanet Backbone", ok: ok["ct"]},
			{country: "CN", city: "Nanjing", asn: 4134, network: "Chinanet Backbone", ok: ok["ct"]},
			{country: "CN", city: "Changsha", asn: 4837, network: "CHINA UNICOM China169 Backbone", ok: ok["cu"]},
			{country: "HK", city: "Hong Kong", asn: 4760, network: "PCCW", ok: ok["hk"]},
		})
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "probesCount": 6})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/measurements/"):
		b, ok := f.bodies[strings.TrimPrefix(r.URL.Path, "/measurements/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(b)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type harness struct {
	svc      *Service
	api      *fakeAPI
	settings map[string]string
	alerts   []string
	now      time.Time
	mu       sync.Mutex
	targets  []Target
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "m-ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	h := &harness{api: newFakeAPI(t), settings: map[string]string{}, now: time.Unix(1_800_000_000, 0),
		targets: []Target{{NodeId: 1, Name: "香港1", Host: "203.0.113.7", Port: 443, PortFrom: "line", PortLine: "anytls-443"}}}
	h.svc = New(Deps{
		DB: db, IsNode: func() bool { return false },
		Setting: func(k string) string { h.mu.Lock(); defer h.mu.Unlock(); return h.settings[k] },
		Targets: func(context.Context) []Target {
			h.mu.Lock()
			defer h.mu.Unlock()
			return append([]Target(nil), h.targets...)
		},
		Notify: func(toggle, text string) {
			if toggle != "tgOnReach" {
				t.Errorf("告警开关键 = %q", toggle)
			}
			h.mu.Lock()
			h.alerts = append(h.alerts, text)
			h.mu.Unlock()
		},
		API: h.api.srv.URL, Now: func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now },
		Poll: time.Millisecond, RecheckDelay: time.Millisecond, FirstDelay: time.Hour,
	})
	t.Cleanup(h.svc.Stop)
	return h
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *harness) alertCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.alerts)
}

func (h *harness) runAuto(t *testing.T) {
	t.Helper()
	if err := h.svc.begin(nil); err != nil {
		t.Fatal(err)
	}
	h.svc.runAll(context.Background(), nil, true)
}

func (h *harness) runManual(t *testing.T, ids ...uint) {
	t.Helper()
	if err := h.svc.Run(ids); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.svc.View().Running {
		if time.Now().After(deadline) {
			t.Fatal("手动检测没结束")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func latest(t *testing.T, h *harness, id uint) Result {
	t.Helper()
	for _, r := range h.svc.View().Results {
		if r.NodeId == id {
			return r
		}
	}
	t.Fatalf("没有服务器 %d 的结果", id)
	return Result{}
}

// 连端口:只用大陆家宽测点 + 香港对照;TCP、端口对;三网都通时不补测 ping。
func TestCheckRequestShapeAndOK(t *testing.T) {
	h := newHarness(t)
	h.settings["reachToken"] = "tok123"
	h.runManual(t)
	r := latest(t, h, 1)
	if r.Verdict != VerdictOK || r.Port != 443 || r.PortLine != "anytls-443" {
		t.Fatalf("结果 = %+v", r)
	}
	h.api.mu.Lock()
	reqs := append([]measureRequest(nil), h.api.requests...)
	tok := h.api.token
	h.api.mu.Unlock()
	if len(reqs) != 1 {
		t.Fatalf("三网都通不该补测 ping,请求数 = %d", len(reqs))
	}
	q := reqs[0]
	if q.Type != "ping" || q.Target != "203.0.113.7" || q.Options.Protocol != "TCP" || q.Options.Port != 443 {
		t.Fatalf("请求 = %+v", q)
	}
	b, _ := json.Marshal(q.Locs)
	if strings.Contains(string(b), "45090") || !strings.Contains(string(b), `"HK"`) || !strings.Contains(string(b), eyeballTag) {
		t.Fatalf("选点条件不对:%s", b)
	}
	if tok != "Bearer tok123" {
		t.Fatalf("令牌没带上: %q", tok)
	}
	if v := h.svc.View(); v.Quota == nil || v.Quota.Limit != 250 || v.PerCheck == 0 {
		t.Fatalf("额度没记下: %+v", v)
	}
}

// 三网不通、香港通:用同一批测点补测 ping;ping 也不通 → IP 被墙;定时巡检复测一次后只告警一次,
// 同样的结果再来不重复告,恢复时再告一次。手动检测不告警。
func TestBlockedAlertLifecycle(t *testing.T) {
	h := newHarness(t)
	down := map[string]bool{"cm": false, "ct": false, "cu": false, "hk": true}
	h.api.set(down, down)

	h.runAuto(t)
	r := latest(t, h, 1)
	if r.Verdict != VerdictIPBlocked || !r.Rechecked || r.ICMP == nil || r.ICMP.CNOK != 0 || r.ICMP.HKOK != 1 {
		t.Fatalf("结果 = %+v", r)
	}
	// 复测时第二次请求的 ping 要复用第一次测量的测点
	h.api.mu.Lock()
	var reuse bool
	for _, q := range h.api.requests {
		if s, ok := q.Locs.(string); ok && strings.HasPrefix(s, "m") && q.Options.Protocol == "" {
			reuse = true
		}
	}
	h.api.mu.Unlock()
	if !reuse {
		t.Fatal("补测 ping 没有复用同一批测点")
	}
	if h.alertCount() != 1 || !strings.Contains(h.alerts[0], "IP 疑似被墙") || !strings.Contains(h.alerts[0], "203.0.113.7:443") {
		t.Fatalf("告警 = %v", h.alerts)
	}

	h.advance(91 * time.Minute)
	h.runAuto(t)
	if h.alertCount() != 1 {
		t.Fatalf("同一件事又告了一次: %v", h.alerts)
	}

	up := map[string]bool{"cm": true, "ct": true, "cu": true, "hk": true}
	h.api.set(up, up)
	h.runManual(t) // 手动发现恢复:不告警,但基线跟着变
	if h.alertCount() != 1 {
		t.Fatalf("手动检测不该告警: %v", h.alerts)
	}
	h.api.set(down, map[string]bool{"cm": true, "ct": true, "cu": true, "hk": true})
	h.runAuto(t)
	if r := latest(t, h, 1); r.Verdict != VerdictPortBlocked {
		t.Fatalf("ping 通、端口不通应判端口被封: %+v", r)
	}
	if h.alertCount() != 2 || !strings.Contains(h.alerts[1], "端口疑似被封") {
		t.Fatalf("告警 = %v", h.alerts)
	}
	h.api.set(up, up)
	h.runAuto(t)
	if h.alertCount() != 3 || !strings.Contains(h.alerts[2], "已恢复") {
		t.Fatalf("恢复没告: %v", h.alerts)
	}
}

// 复测结果正常(测点抽风):不告警。
func TestRecheckSuppressesFlake(t *testing.T) {
	h := newHarness(t)
	h.api.set(map[string]bool{"cm": false, "ct": true, "cu": true, "hk": true}, map[string]bool{"cm": true, "ct": true, "cu": true, "hk": true})
	var once sync.Once
	h.svc.d.RecheckDelay = time.Millisecond
	go func() {
		// 第一轮测量发出后立刻把移动恢复,复测时就是正常的
		for {
			h.api.mu.Lock()
			n := len(h.api.requests)
			h.api.mu.Unlock()
			if n >= 1 {
				once.Do(func() {
					h.api.set(map[string]bool{"cm": true, "ct": true, "cu": true, "hk": true}, map[string]bool{"cm": true, "ct": true, "cu": true, "hk": true})
				})
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	h.svc.d.RecheckDelay = 50 * time.Millisecond
	h.runAuto(t)
	if r := latest(t, h, 1); r.Verdict != VerdictOK || !r.Rechecked {
		t.Fatalf("结果 = %+v", r)
	}
	if h.alertCount() != 0 {
		t.Fatalf("抽风不该告警: %v", h.alerts)
	}
}

// 额度用完:结果记为检测失败并说明原因;之后额度不够就不再发请求。
func TestQuotaExhausted(t *testing.T) {
	h := newHarness(t)
	h.api.mu.Lock()
	h.api.remaining = 0
	h.api.mu.Unlock()
	h.runManual(t)
	r := latest(t, h, 1)
	if r.Verdict != VerdictError || !strings.Contains(r.Error, "额度") {
		t.Fatalf("结果 = %+v", r)
	}
	h.api.mu.Lock()
	n := len(h.api.requests)
	h.api.mu.Unlock()
	h.runManual(t)
	h.api.mu.Lock()
	n2 := len(h.api.requests)
	h.api.mu.Unlock()
	if n2 != n {
		t.Fatalf("额度用完后还在发请求: %d → %d", n, n2)
	}
	if h.alertCount() != 0 {
		t.Fatal("检测失败不该告警")
	}
}

// 定时:到点才测;关掉自动就不测;副机不测;手动那轮在跑时定时的跳过。
func TestTickSchedule(t *testing.T) {
	h := newHarness(t)
	h.svc.tick()
	if len(h.svc.View().Results) != 1 {
		t.Fatal("第一次到点应当测")
	}
	h.advance(89 * time.Minute)
	h.svc.tick()
	h.api.mu.Lock()
	n := len(h.api.requests)
	h.api.mu.Unlock()
	if n != 1 {
		t.Fatalf("没到 90 分钟就测了: %d", n)
	}
	h.advance(2 * time.Minute)
	h.mu.Lock()
	h.settings["reachAuto"] = "false"
	h.mu.Unlock()
	h.svc.tick()
	h.api.mu.Lock()
	n = len(h.api.requests)
	h.api.mu.Unlock()
	if n != 1 {
		t.Fatal("关掉自动巡检还在测")
	}
	h.mu.Lock()
	h.settings["reachAuto"] = ""
	h.settings["reachMinutes"] = "30"
	h.mu.Unlock()
	h.svc.tick()
	h.api.mu.Lock()
	n = len(h.api.requests)
	h.api.mu.Unlock()
	if n != 2 {
		t.Fatalf("改成 30 分钟后到点没测: %d", n)
	}
	if v := h.svc.View(); v.Minutes != 30 || v.NextRun != v.LastRun+30*60 {
		t.Fatalf("下次时间不对: %+v", v)
	}
}

// 重启后:结果、历史和告警基线从库里恢复,不会把同一件事再告一次。
func TestPersistAcrossRestart(t *testing.T) {
	h := newHarness(t)
	down := map[string]bool{"cm": false, "ct": false, "cu": false, "hk": true}
	h.api.set(down, down)
	h.runAuto(t)
	if h.alertCount() != 1 {
		t.Fatalf("告警 = %v", h.alerts)
	}
	db := h.svc.d.DB
	var n int64
	db.Model(&model.ReachCheck{}).Count(&n)
	if n != 1 {
		t.Fatalf("库里应有 1 条,实际 %d", n)
	}
	h2 := &harness{api: h.api, settings: map[string]string{}, now: h.now.Add(2 * time.Hour), targets: h.targets}
	h2.svc = New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(k string) string { return "" },
		Targets: func(context.Context) []Target { return h2.targets },
		Notify:  func(_, text string) { h2.alerts = append(h2.alerts, text) },
		API:     h.api.srv.URL, Now: func() time.Time { return h2.now }, Poll: time.Millisecond, RecheckDelay: time.Millisecond, FirstDelay: time.Hour})
	defer h2.svc.Stop()
	v := h2.svc.View()
	if len(v.Results) != 1 || v.Results[0].Verdict != VerdictIPBlocked || len(v.History[1]) != 1 || v.LastRun == 0 {
		t.Fatalf("重启后没恢复: %+v", v)
	}
	h2.runAuto(t)
	if len(h2.alerts) != 0 {
		t.Fatalf("重启后把同一件事又告了一次: %v", h2.alerts)
	}
}

// 服务器删掉:结果与库里的记录都不留;没地址的服务器记为检测失败、不发请求。
func TestForgetAndMissingAddress(t *testing.T) {
	h := newHarness(t)
	h.targets = append(h.targets, Target{NodeId: 2, Name: "台湾", Err: "没有可测的地址"})
	h.runManual(t)
	if r := latest(t, h, 2); r.Verdict != VerdictError || r.Error == "" {
		t.Fatalf("没地址的服务器: %+v", r)
	}
	h.api.mu.Lock()
	n := len(h.api.requests)
	h.api.mu.Unlock()
	if n != 1 {
		t.Fatalf("没地址的服务器不该发请求: %d", n)
	}
	h.mu.Lock()
	h.targets = h.targets[:1]
	h.mu.Unlock()
	h.runManual(t)
	if len(h.svc.View().Results) != 1 {
		t.Fatal("删掉的服务器结果还在")
	}
	var cnt int64
	h.svc.d.DB.Model(&model.ReachCheck{}).Where("node_id = ?", 2).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("删掉的服务器库里还有 %d 条", cnt)
	}
	// 只测一台时不清别的服务器
	h.runManual(t, 1)
	if len(h.svc.View().Results) != 1 {
		t.Fatal("只测一台时不该动别的")
	}
}

// 正在跑时再点:返回忙,不排第二轮。
func TestBusy(t *testing.T) {
	h := newHarness(t)
	if err := h.svc.begin(nil); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Run(nil); err != ErrBusy {
		t.Fatalf("err = %v", err)
	}
	h.svc.finish(false)
	if err := h.svc.Run(nil); err != nil {
		t.Fatal(err)
	}
}

// 副机不做检测。
func TestNodeRefuses(t *testing.T) {
	s := New(Deps{Setting: func(string) string { return "" }, IsNode: func() bool { return true }, Targets: func(context.Context) []Target { return nil }})
	defer s.Stop()
	if err := s.Run(nil); err == nil {
		t.Fatal("副机上手动检测应当被拒")
	}
}

// 没测成的那次(额度用完)不能把"被墙"盖掉:界面、概览提示、告警基线都以最近一次测成的为准。
func TestFailedAttemptKeepsLastResult(t *testing.T) {
	h := newHarness(t)
	down := map[string]bool{"cm": false, "ct": false, "cu": false, "hk": true}
	h.api.set(down, down)
	h.runAuto(t)
	if h.alertCount() != 1 {
		t.Fatalf("告警 = %v", h.alerts)
	}
	h.api.mu.Lock()
	h.api.remaining = 0
	h.api.mu.Unlock()
	h.advance(time.Minute)
	h.runManual(t)
	r := latest(t, h, 1)
	if r.Verdict != VerdictIPBlocked || r.Attempt == nil || r.Attempt.Verdict != VerdictError || !strings.Contains(r.Attempt.Error, "额度") {
		t.Fatalf("没测成时应当仍显示上一次的结论并注明: %+v", r)
	}
	if bad := h.svc.Bad(); len(bad) != 1 || bad[0].Verdict != VerdictIPBlocked {
		t.Fatalf("概览提示被没测成的那次抹掉了: %+v", bad)
	}
	if n := len(h.svc.View().History[1]); n != 2 {
		t.Fatalf("历史里应当有两次(含没测成的): %d", n)
	}
	// 额度恢复,还是被墙:和上一次测成的相同,不复测、不再告警
	h.api.mu.Lock()
	h.api.remaining = 250
	h.api.requests = nil
	h.api.mu.Unlock()
	h.svc.c.mu.Lock()
	h.svc.c.quota = Quota{}
	h.svc.c.mu.Unlock()
	h.advance(91 * time.Minute)
	h.runAuto(t)
	h.api.mu.Lock()
	n := len(h.api.requests)
	h.api.mu.Unlock()
	if n != 2 { // 连端口一次 + 补测 ping 一次,没有复测
		t.Fatalf("和上次测成的结果相同却复测了: %d 次请求", n)
	}
	if h.alertCount() != 1 {
		t.Fatalf("同一件事又告了一次: %v", h.alerts)
	}
	if r := latest(t, h, 1); r.Attempt != nil {
		t.Fatalf("测成之后不该再挂着失败说明: %+v", r)
	}
}
