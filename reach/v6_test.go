package reach

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 测试里不请求真实 Globalping:全部打到 reach_test.go 的假接口(目标是 IPv6 地址时,选点条件里有大陆的派大陆测点,否则派欧洲、北美的)。

// setV6 假接口这一轮 IPv6 入站怎么答:大陆派几个测点、通不通;境外派几个、通不通。
func (h *harness) setV6(cnProbes int, cnOK bool, probes int, ok bool) {
	h.api.mu.Lock()
	h.api.v6CNProbes, h.api.v6CNOK, h.api.v6Probes, h.api.v6OK = cnProbes, cnOK, probes, ok
	h.api.mu.Unlock()
}

func (f *fakeAPI) reqs() []measureRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]measureRequest(nil), f.requests...)
}

func v6Reqs(f *fakeAPI) []measureRequest {
	var out []measureRequest
	for _, q := range f.reqs() {
		if strings.Contains(q.Target, ":") {
			out = append(out, q)
		}
	}
	return out
}

// 订阅里有走 IPv6 的线路:同一轮连 IPv6 地址的那条 TCP 线路端口,先用大陆的家宽测点(不传 ipVersion);
// 大陆测点连上了就不再问境外测点。结论记进结果、存库,重启后还在。
func TestV6CheckRequestShapeAndOK(t *testing.T) {
	h := newHarness(t)
	h.targets[0].V6 = &V6Target{Host: "2001:db8::7", Port: 8443, PortLine: "tj6", UDPLines: []string{"hy6"}}
	h.runManual(t)
	r := latest(t, h, 1)
	if r.Verdict != VerdictOK {
		t.Fatalf("大陆那项不受影响: %+v", r)
	}
	v := r.V6
	if v == nil || v.State != V6OK || v.Scope != V6ScopeCN || v.OK != 4 || v.Total != 4 || v.CnOK != 4 || v.CnTotal != 4 || v.Port != 8443 || v.PortLine != "tj6" || len(v.UDPLines) != 1 || v.Measurement == "" {
		t.Fatalf("IPv6 入站结果 = %+v", v)
	}
	if len(v.Probes) != 4 || v.Probes[0].Country != "CN" || v.Probes[0].Group != GroupMobile || v.Probes[1].Group != GroupTelecom || v.Probes[2].Group != GroupUnicom {
		t.Fatalf("测点明细要带国家,大陆测点还要认出运营商: %+v", v.Probes)
	}
	qs := v6Reqs(h.api)
	if len(qs) != 1 || len(h.api.reqs()) != 2 {
		t.Fatalf("大陆测点连上了:应当正好多一次 IPv6 请求,不再问境外测点: %+v", h.api.reqs())
	}
	q := qs[0]
	if q.Type != "ping" || q.Target != "2001:db8::7" || q.Options.Protocol != "TCP" || q.Options.Port != 8443 {
		t.Fatalf("请求 = %+v", q)
	}
	b, _ := json.Marshal(q)
	if !strings.Contains(string(b), `"country":"CN"`) || !strings.Contains(string(b), eyeballTag) || strings.Contains(string(b), "ipVersion") || strings.Contains(string(b), "continent") {
		t.Fatalf("选点不对(先用大陆的家宽测点、不传 ipVersion): %s", b)
	}
	// 存进库里,重启后能看到
	var row model.ReachCheck
	h.svc.d.DB.Order("id desc").First(&row)
	if !strings.Contains(row.Detail, `"v6"`) {
		t.Fatalf("库里没存 IPv6 入站结果: %s", row.Detail)
	}
	s2 := New(Deps{DB: h.svc.d.DB, IsNode: func() bool { return false }, Setting: func(string) string { return "" },
		Targets: func(context.Context) []Target { return nil }, API: h.api.srv.URL, FirstDelay: time.Hour})
	defer s2.Stop()
	if v := s2.View().Results; len(v) != 1 || v[0].V6 == nil || v[0].V6.State != V6OK || v[0].V6.Scope != V6ScopeCN {
		t.Fatalf("重启后没恢复 IPv6 入站结果: %+v", v)
	}
}

// 先大陆、后境外:
//   - 大陆测点连上 → 通(不问境外);
//   - 大陆测点都连不上 → 问境外:境外通 = 大陆方向不通(cndown),境外也不通 = 入站没放行(down);
//   - 没有大陆的 IPv6 测点 → 只靠境外测点下结论,结论注明是境外测点(scope abroad);
//   - 测到的太少 → 下不了结论;一个测点都没有 → none。
func TestV6CheckVerdicts(t *testing.T) {
	cases := []struct {
		name            string
		cnProbes        int
		cnOK            bool
		probes          int
		ok              bool
		want, wantScope string
		wantReqs        int // 发了几次 IPv6 请求(被拒收的那次「没有大陆测点」也算一次)
	}{
		{"大陆通", 4, true, 4, true, V6OK, V6ScopeCN, 1},
		{"大陆一个测点通就算通", 1, true, 4, false, V6OK, V6ScopeCN, 1},
		{"大陆不通、境外通", 4, false, 4, true, V6CNDown, V6ScopeBoth, 2},
		{"大陆两个测点不通、境外一个通", 2, false, 1, true, V6CNDown, V6ScopeBoth, 2},
		{"大陆只有一个测点不通、境外通:说不准", 1, false, 4, true, V6Unsure, V6ScopeBoth, 2},
		{"大陆、境外都不通", 4, false, 4, false, V6Down, V6ScopeBoth, 2},
		{"大陆一个不通、境外一个不通:合起来两个", 1, false, 1, false, V6Down, V6ScopeBoth, 2},
		{"大陆两个不通、境外没有测点", 2, false, 0, false, V6Down, V6ScopeBoth, 2},
		{"大陆一个不通、境外没有测点", 1, false, 0, false, V6Unsure, V6ScopeBoth, 2},
		{"没有大陆测点、境外通", 0, false, 4, true, V6OK, V6ScopeAbroad, 2},
		{"没有大陆测点、境外一个通", 0, false, 1, true, V6OK, V6ScopeAbroad, 2},
		{"没有大陆测点、境外全不通", 0, false, 4, false, V6Down, V6ScopeAbroad, 2},
		{"没有大陆测点、境外两个不通也够下结论", 0, false, 2, false, V6Down, V6ScopeAbroad, 2},
		{"没有大陆测点、境外只有一个不通", 0, false, 1, false, V6Unsure, V6ScopeAbroad, 2},
		{"哪里都没有测点", 0, false, 0, false, V6None, V6ScopeAbroad, 2},
	}
	for _, c := range cases {
		h := newHarness(t)
		h.setV6(c.cnProbes, c.cnOK, c.probes, c.ok)
		h.targets[0].V6 = &V6Target{Host: "2001:db8::7", Port: 443, PortLine: "tj6"}
		h.runManual(t)
		v := latest(t, h, 1).V6
		if v == nil || v.State != c.want || v.Scope != c.wantScope {
			t.Errorf("%s: 结论 = %+v,应为 %s / %s", c.name, v, c.want, c.wantScope)
			continue
		}
		if n := len(v6Reqs(h.api)); n != c.wantReqs {
			t.Errorf("%s: 发了 %d 次 IPv6 请求,应为 %d", c.name, n, c.wantReqs)
		}
		if v.CnTotal != c.cnProbes || (c.cnOK && v.CnOK != c.cnProbes) {
			t.Errorf("%s: 大陆那一步的个数不对: %+v", c.name, v)
		}
	}
	// 境外那一步和真接口一样直接拒收(没有测点)时,结论一样
	h := newHarness(t)
	h.setV6(0, false, 0, false)
	h.api.mu.Lock()
	h.api.v6NoProbes422 = true
	h.api.mu.Unlock()
	h.targets[0].V6 = &V6Target{Host: "2001:db8::7", Port: 443, PortLine: "tj6"}
	h.runManual(t)
	if v := latest(t, h, 1).V6; v == nil || v.State != V6None || v.Error != "" {
		t.Errorf("两步都被拒收(没有测点): %+v", v)
	}
}

// 大陆测点都连不上、该问境外测点时额度不够了:不发第二次请求,结论仍是不通,但注明没能问境外测点
// (分不清是入站没放行还是大陆方向不通)。
func TestV6AbroadSkippedForQuota(t *testing.T) {
	h := newHarness(t)
	h.setV6(4, false, 4, true)
	h.targets = []Target{{NodeId: 1, Name: "纯v6", Err: "订阅里这台用的是 IPv6 地址,大陆连通检测暂只支持 IPv4",
		V6: &V6Target{Host: "2001:db8::9", Port: 443, PortLine: "anytls"}}}
	h.svc.c.mu.Lock()
	h.svc.c.quota = Quota{Limit: 250, Remaining: v6Cost() + v6AbroadCost() - 1, At: h.now.Unix(), ResetAt: h.now.Unix() + 1800}
	h.svc.c.mu.Unlock()
	h.api.mu.Lock()
	h.api.remaining = v6Cost() + v6AbroadCost() - 1
	h.api.mu.Unlock()
	h.runManual(t)
	v := latest(t, h, 1).V6
	if v == nil || v.State != V6Down || v.Scope != V6ScopeBoth || !strings.Contains(v.AbroadSkip, "额度") || v.CnTotal != 4 || v.Total != 4 {
		t.Fatalf("境外那一步没额度:应是不通并注明没问成境外测点: %+v", v)
	}
	if n := len(v6Reqs(h.api)); n != 1 {
		t.Fatalf("额度不够时不该发境外那一次: %d", n)
	}
}

// 走 IPv6 的只有 UDP 线路(hy2 / tuic)、地址不能测:不发请求,如实标出。只有 UDP 线路而地址空着(认不出这台的 IPv6)
// 也是"只有 UDP 线路",不报"没有可测的地址"(本来就不发请求,也不是没测成)。
func TestV6NoRequestWhenUntestable(t *testing.T) {
	h := newHarness(t)
	h.targets[0].V6 = &V6Target{Host: "2001:db8::7", UDPLines: []string{"hy6", "tuic6"}}
	h.targets = append(h.targets, Target{NodeId: 2, Name: "内网v6", Host: "198.51.100.2", Port: 443, PortFrom: "line",
		V6: &V6Target{Host: "fd00::5", Port: 443, PortLine: "tj6", Err: "这台订阅里的 IPv6 地址不是公网地址(内网或保留段),测点连不到"}},
		Target{NodeId: 3, Name: "UDP没地址", Host: "198.51.100.3", Port: 443, PortFrom: "line", V6: &V6Target{UDPLines: []string{"hy6"}}})
	h.runManual(t)
	if v := latest(t, h, 1).V6; v == nil || v.State != V6UDP || len(v.UDPLines) != 2 || v.Port != 0 {
		t.Fatalf("只有 UDP 线路: %+v", v)
	}
	if v := latest(t, h, 2).V6; v == nil || v.State != V6Error || !strings.Contains(v.Error, "不是公网地址") {
		t.Fatalf("内网地址: %+v", v)
	}
	if v := latest(t, h, 3).V6; v == nil || v.State != V6UDP || v.Error != "" {
		t.Fatalf("只有 UDP 线路、地址空着: %+v", v)
	}
	if qs := v6Reqs(h.api); len(qs) != 0 {
		t.Fatalf("测不了的不该发请求: %+v", qs)
	}
}

// 订阅里这台默认给 IPv6(大陆检测不测):IPv6 入站照测,大陆那项照旧标"暂只支持 IPv4"、不发大陆请求。
func TestV6CheckedWhenMainlandSkipped(t *testing.T) {
	h := newHarness(t)
	h.targets = []Target{{NodeId: 1, Name: "纯v6", Err: "订阅里这台用的是 IPv6 地址,大陆连通检测暂只支持 IPv4",
		V6: &V6Target{Host: "2001:db8::9", Port: 443, PortLine: "anytls"}}}
	h.runManual(t)
	r := latest(t, h, 1)
	if r.Verdict != VerdictError || r.V6 == nil || r.V6.State != V6OK {
		t.Fatalf("结果 = %+v / %+v", r, r.V6)
	}
	if all := h.api.reqs(); len(all) != 1 || all[0].Target != "2001:db8::9" {
		t.Fatalf("应当只有一次 IPv6 请求: %+v", all)
	}
}

// 额度:大陆那项不够、IPv6 那项够时只测 IPv6;都不够时都不发请求,面板上留着上一次测成的 IPv6 结论并注明原因。
func TestV6QuotaAndKeepLastMeasured(t *testing.T) {
	h := newHarness(t)
	h.targets[0].V6 = &V6Target{Host: "2001:db8::7", Port: 8443, PortLine: "tj6"}
	h.runManual(t) // 都测成
	first := latest(t, h, 1).V6
	if first == nil || first.State != V6OK {
		t.Fatalf("第一次: %+v", first)
	}
	setQuota := func(remaining int) {
		h.svc.c.mu.Lock()
		h.svc.c.quota = Quota{Limit: 250, Remaining: remaining, At: h.now.Unix(), ResetAt: h.now.Unix() + 1800}
		h.svc.c.mu.Unlock()
	}
	// 大陆一次要 8 个测点次(假接口:移动 2、电信 2、联通 1、香港 3),IPv6 要 4 个
	h.advance(time.Minute)
	setQuota(6)
	n := len(h.api.reqs())
	h.runManual(t)
	r := latest(t, h, 1)
	if r.Attempt == nil || r.Attempt.Verdict != VerdictError || r.V6 == nil || r.V6.State != V6OK || r.V6.At == first.At || r.V6.Attempt != nil {
		t.Fatalf("大陆额度不够、IPv6 够:应显示新的 IPv6 结论 %+v / %+v", r, r.V6)
	}
	if got := h.api.reqs()[n:]; len(got) != 1 || got[0].Target != "2001:db8::7" {
		t.Fatalf("只该发 IPv6 那一次: %+v", got)
	}
	second := r.V6
	h.advance(time.Minute)
	setQuota(2)
	n = len(h.api.reqs())
	h.runManual(t)
	r = latest(t, h, 1)
	if len(h.api.reqs()) != n {
		t.Fatal("额度不够还在发请求")
	}
	if r.V6 == nil || r.V6.State != V6OK || r.V6.At != second.At || r.V6.Attempt == nil || !strings.Contains(r.V6.Attempt.Error, "额度") {
		t.Fatalf("没测成时应留着上一次测成的 IPv6 结论并注明: %+v", r.V6)
	}
	// 这台不再有走 IPv6 的线路:不再显示
	h.targets[0].V6 = nil
	h.svc.c.mu.Lock()
	h.svc.c.quota = Quota{}
	h.svc.c.mu.Unlock()
	h.advance(time.Minute)
	h.runManual(t)
	if r := latest(t, h, 1); r.V6 != nil {
		t.Fatalf("没有 IPv6 线路了还显示: %+v", r.V6)
	}
}

// 外部节点检测给巡检预留额度时,把上一轮测过 IPv6 入站的那几台也算上。
func TestCanCheckExtReservesV6(t *testing.T) {
	h := newHarness(t)
	h.targets[0].V6 = &V6Target{Host: "2001:db8::7", Port: 8443, PortLine: "tj6"}
	h.runManual(t)
	h.svc.mu.Lock()
	cost := h.svc.perCheck
	h.svc.mu.Unlock()
	h.svc.c.mu.Lock()
	h.svc.c.quota = Quota{Limit: 250, Remaining: cost*2 + v6Cost() - 1, At: h.now.Unix(), ResetAt: h.now.Unix() + 1800}
	h.svc.c.mu.Unlock()
	if err := h.svc.CanCheckExt(1); err == nil {
		t.Fatal("预留没算 IPv6 入站那几个测点次")
	}
	h.svc.c.mu.Lock()
	h.svc.c.quota.Remaining = cost*2 + v6Cost()
	h.svc.c.mu.Unlock()
	if err := h.svc.CanCheckExt(1); err != nil {
		t.Fatalf("额度刚好够: %v", err)
	}
}

// 这次没有可用的 IPv6 测点(大陆、境外都没有:测点中途掉线、被判超时,结果里一个都没测到):和没测成一样,不盖掉上一次的结论,
// 面板上照旧显示上一次的"不通"并注明最近这次的情况;概况的 v6Bad 也照旧算它。
func TestV6NoneKeepsLastMeasured(t *testing.T) {
	h := newHarness(t)
	h.targets[0].V6 = &V6Target{Host: "2001:db8::7", Port: 8443, PortLine: "tj6"}
	h.setV6(4, false, 4, false)
	h.runManual(t)
	first := latest(t, h, 1).V6
	if first == nil || first.State != V6Down {
		t.Fatalf("第一次应测出不通: %+v", first)
	}
	h.advance(time.Minute)
	h.setV6(0, false, 0, false)
	h.runManual(t)
	v := latest(t, h, 1).V6
	if v == nil || v.State != V6Down || v.At != first.At || v.Attempt == nil || v.Attempt.Verdict != V6None {
		t.Fatalf("没有测点时应留着上一次的不通并注明: %+v", v)
	}
	if bad := h.svc.V6Bad(); len(bad) != 1 || bad[0].NodeId != 1 {
		t.Fatalf("v6Bad 照旧算上一次的不通: %+v", bad)
	}
	// 从来没测成过、这次又没有测点:就显示"无可用测点"
	h2 := newHarness(t)
	h2.targets[0].V6 = &V6Target{Host: "2001:db8::7", Port: 8443, PortLine: "tj6"}
	h2.setV6(0, false, 0, false)
	h2.runManual(t)
	if v := latest(t, h2, 1).V6; v == nil || v.State != V6None || v.Attempt != nil {
		t.Fatalf("没有可比的上一次时如实显示: %+v", v)
	}
}

// 概况"需要处理"用的 v6Bad:算 down(入站没放行)和 cndown(境外通、大陆不通),unsure / udp / none / error 不算,
// 和服务器页显示的是同一份;这台不再有走 IPv6 的线路就不算了;没有时是空数组。
func TestV6Bad(t *testing.T) {
	h := newHarness(t)
	if b, _ := json.Marshal(h.svc.V6Bad()); string(b) != "[]" {
		t.Fatalf("没有时要是空数组: %s", b)
	}
	h.targets = []Target{
		{NodeId: 1, Name: "不通", Host: "203.0.113.7", Port: 443, PortFrom: "line", V6: &V6Target{Host: "2001:db8::1", Port: 8443, PortLine: "tj6"}},
		{NodeId: 2, Name: "只有UDP", Host: "203.0.113.8", Port: 443, PortFrom: "line", V6: &V6Target{Host: "2001:db8::2", UDPLines: []string{"hy6"}}},
		{NodeId: 3, Name: "内网", Host: "203.0.113.9", Port: 443, PortFrom: "line", V6: &V6Target{Host: "fd00::3", Port: 443, Err: "不是公网地址"}},
	}
	h.setV6(4, false, 4, false)
	h.runManual(t)
	bad := h.svc.V6Bad()
	if len(bad) != 1 || bad[0].NodeId != 1 || bad[0].Name != "不通" || bad[0].Target != "2001:db8::1" || bad[0].Port != 8443 || bad[0].At == 0 || bad[0].State != V6Down {
		t.Fatalf("只算 down 的那台: %+v", bad)
	}
	b, _ := json.Marshal(bad[0])
	for _, k := range []string{`"nodeId":1`, `"name":"不通"`, `"target":"2001:db8::1"`, `"port":8443`, `"at":`, `"state":"down"`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("字段 %s: %s", k, b)
		}
	}
	if len(h.svc.Bad()) != 0 {
		t.Fatalf("不并进 reachBad: %+v", h.svc.Bad())
	}
	// 境外通、大陆不通:也算,状态写明是 cndown(概况上和「入站没放行」分开说)
	h.setV6(4, false, 4, true)
	h.advance(time.Minute)
	h.runManual(t)
	if bad := h.svc.V6Bad(); len(bad) != 1 || bad[0].State != V6CNDown {
		t.Fatalf("大陆方向不通也要算: %+v", bad)
	}
	// 线路改回 IPv4:这台不再有 IPv6 入站,概况上也不再算
	h.targets[0].V6 = nil
	h.advance(time.Minute)
	h.runManual(t)
	if bad := h.svc.V6Bad(); len(bad) != 0 {
		t.Fatalf("没有走 IPv6 的线路了还算不通: %+v", bad)
	}
}

// 额度先保大陆检测:IPv6 入站只用大陆检测剩下的。5 台服务器都有走 IPv6 的线路,额度只够 5 台大陆检测再加一次 IPv6 入站:
// 以前按服务器顺序每台"大陆 + IPv6"交替花,排在后面的服务器大陆检测被挤成"额度已用完"(被墙也不告警);
// 现在 5 台大陆检测都测成,IPv6 入站测成一次,其余的注明额度不够、留着上一次的结论。
func TestMainlandQuotaBeforeV6(t *testing.T) {
	h := newHarness(t)
	h.targets = nil
	for i := 1; i <= 5; i++ {
		h.targets = append(h.targets, Target{NodeId: uint(i), Name: fmt.Sprintf("s%d", i), Host: fmt.Sprintf("203.0.113.%d", i), Port: 443, PortFrom: "line",
			V6: &V6Target{Host: fmt.Sprintf("2001:db8::%d", i), Port: 8443, PortLine: "tj6"}})
	}
	// 大陆一次 8 个测点次(假接口:移动 2、电信 2、联通 1、香港 3),IPv6 一次 4 个
	h.api.mu.Lock()
	h.api.remaining = 5*8 + 4 + 2
	h.api.mu.Unlock()
	h.runManual(t)
	v6ok := 0
	for i := uint(1); i <= 5; i++ {
		r := latest(t, h, i)
		if r.Verdict != VerdictOK {
			t.Fatalf("s%d 的大陆检测被 IPv6 入站挤掉了: %+v", i, r)
		}
		switch {
		case r.V6 == nil:
			t.Fatalf("s%d 没有 IPv6 入站结果", i)
		case r.V6.State == V6OK:
			v6ok++
		case r.V6.State != V6Error || !strings.Contains(r.V6.Error, "额度"):
			t.Fatalf("s%d 的 IPv6 入站应注明额度不够: %+v", i, r.V6)
		}
	}
	if v6ok != 1 {
		t.Fatalf("剩下的额度正好够一次 IPv6 入站,实际测成 %d 次", v6ok)
	}
}

// 额度缓存:几个请求并发时响应处理的先后不定,同一个窗口里读到更大的剩余多半是早先那个请求的,不能把额度"加回去";
// 过了恢复时间、上限变了(填了令牌)才整个换。
func TestQuotaCacheNeverGoesBackUpWithinWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := &client{now: func() time.Time { return now }}
	hdr := func(limit, remaining, reset int) http.Header {
		h := http.Header{}
		h.Set("X-RateLimit-Limit", fmt.Sprint(limit))
		h.Set("X-RateLimit-Remaining", fmt.Sprint(remaining))
		h.Set("X-RateLimit-Reset", fmt.Sprint(reset))
		return h
	}
	c.noteQuota(hdr(250, 30, 1800))
	c.noteQuota(hdr(250, 46, 1801)) // 早先那个请求的响应后到
	if q := c.Quota(); q.Remaining != 30 {
		t.Fatalf("同一窗口里不该变多: %+v", q)
	}
	c.noteQuota(hdr(250, 22, 1799))
	if q := c.Quota(); q.Remaining != 22 {
		t.Fatalf("变少照记: %+v", q)
	}
	c.noteQuota(hdr(500, 480, 1799)) // 填了令牌,上限变了
	if q := c.Quota(); q.Remaining != 480 || q.Limit != 500 {
		t.Fatalf("上限变了整个换: %+v", q)
	}
	now = now.Add(31 * time.Minute) // 过了恢复时间:新窗口
	c.noteQuota(hdr(500, 496, 3600))
	if q := c.Quota(); q.Remaining != 496 {
		t.Fatalf("新窗口整个换: %+v", q)
	}
}
