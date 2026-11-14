package reach

import (
	"strings"
	"testing"
	"time"
)

// 这台的线路在订阅里都不走 IPv4(NoPort):大陆那项不发请求,结论记为 noPort(灰色、不告警);
// 它算结论,会盖掉以前测出的"被墙"—— 改用 IPv6 之后,顶部提示和概况里不会一直挂着以前那条。
func TestNoPortReplacesOldVerdictWithoutRequests(t *testing.T) {
	h := newHarness(t)
	down := map[string]bool{"cm": false, "ct": false, "cu": false, "hk": true}
	h.api.set(down, down)
	h.runAuto(t)
	if r := latest(t, h, 1); !IsBlocked(r.Verdict) || len(h.svc.Bad()) != 1 {
		t.Fatalf("先测出被墙: %+v / %+v", r, h.svc.Bad())
	}
	alerts := h.alertCount()
	n := len(h.api.reqs())
	h.targets[0] = Target{NodeId: 1, Name: "香港1", NoPort: "这台的线路在订阅里都走 IPv6,大陆连通检测暂只支持 IPv4"}
	h.advance(time.Hour)
	h.runAuto(t)
	r := latest(t, h, 1)
	if r.Verdict != VerdictNoPort || !strings.Contains(r.Error, "都走 IPv6") || r.Attempt != nil {
		t.Fatalf("应记为无可测端口(算结论,不带 attempt): %+v", r)
	}
	if got := len(h.api.reqs()); got != n {
		t.Fatalf("不该发请求: %d → %d", n, got)
	}
	if len(h.svc.Bad()) != 0 {
		t.Fatalf("以前的被墙不该一直挂着: %+v", h.svc.Bad())
	}
	if h.alertCount() != alerts {
		t.Fatalf("无可测端口不告警: %v", h.alerts)
	}
}

// 借来的面板 API 端口(走 IPv4 的线路没有 TCP 的副机)连香港也连不上:多半是这个端口只对主机放行(FAQ 的建议),
// 记"无可测端口"、不报红色的"境外也不通";线路自己的端口香港连不上,照旧是"境外也不通"。
func TestAPIPortUnreachableIsNoPort(t *testing.T) {
	h := newHarness(t)
	allDown := map[string]bool{"cm": false, "ct": false, "cu": false, "hk": false}
	h.api.set(allDown, allDown)
	h.targets[0] = Target{NodeId: 1, Name: "副机", Host: "203.0.113.7", Port: 2053, PortFrom: "api", TCPv6Only: true}
	h.runAuto(t)
	r := latest(t, h, 1)
	if r.Verdict != VerdictNoPort || !strings.Contains(r.Error, "API 端口") || len(r.Measurements) == 0 || !r.TCPv6Only {
		t.Fatalf("API 端口香港也不通应记无可测端口: %+v", r)
	}
	if h.alertCount() != 0 || len(h.svc.Bad()) != 0 {
		t.Fatalf("不告警、不进 reachBad: %v %+v", h.alerts, h.svc.Bad())
	}
	h.targets[0] = Target{NodeId: 1, Name: "副机", Host: "203.0.113.7", Port: 443, PortFrom: "line", PortLine: "vl4"}
	h.advance(time.Hour)
	h.runAuto(t)
	if r := latest(t, h, 1); r.Verdict != VerdictAbroadDown {
		t.Fatalf("线路端口香港也不通照旧是境外也不通: %+v", r)
	}
	// API 端口香港连得上:照常下结论(这时能看出 IP 有没有被墙)
	ok := map[string]bool{"cm": true, "ct": true, "cu": true, "hk": true}
	h.api.set(ok, ok)
	h.targets[0] = Target{NodeId: 1, Name: "副机", Host: "203.0.113.7", Port: 2053, PortFrom: "api", TCPv6Only: true}
	h.advance(time.Hour)
	h.runAuto(t)
	if r := latest(t, h, 1); r.Verdict != VerdictOK {
		t.Fatalf("API 端口连得上时照常下结论: %+v", r)
	}
}
