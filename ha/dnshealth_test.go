package ha

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 连订阅一起接管靠的 DNS 接口定期自检:凭据失效时就绪检查标红、Telegram 告一次(不重复),恢复了说一声;
// 订阅域名没有指向本机 IP 的记录时提醒(接任时改不了它);没开接管订阅不查。
func TestDNSHealthCheck(t *testing.T) {
	cf := newFakeCF(t)
	s := testService(t, false, map[string]string{KeyEpoch: "1", KeyMode: ModeConfirm, KeyTakeSub: "true", KeyDnsToken: "tok",
		KeyDnsNames: "sub.example.com", "subDomain": "sub.example.com", "publicIp": "198.51.100.99"})
	var reject atomic.Bool // 服务商拒绝一切(令牌被删、权限被收回)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject.Load() {
			rejectAll{}.ServeHTTP(w, r)
			return
		}
		cf.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	s.SetDNSAPI(srv.URL)
	clk := &testClock{t: time.Now()}
	s.SetNow(clk.now)
	var sent []string
	s.d.Notify = func(_, m string) { sent = append(sent, m) }
	ready := func() map[string]Check {
		out := map[string]Check{}
		for _, c := range s.readiness(s.Config(), nil) {
			out[c.Code] = c
		}
		return out
	}
	s.dnsHealthTick()
	if r := ready(); r["takeSubDns"].Level != "ok" || r["takeSubDnsErr"].Code != "" || r["takeSubDnsMatch"].Code != "" {
		t.Fatalf("凭据能用、订阅域名指向本机:正常: %+v", r)
	}
	// 订阅域名的记录都不指向本机 IP(比如开了 CDN):提醒
	s.set(map[string]string{"publicIp": "203.0.113.50"})
	clk.add(dnsHealthEvery + time.Minute)
	s.dnsHealthTick()
	if r := ready(); r["takeSubDnsMatch"].Level != "warn" {
		t.Fatalf("订阅域名没有指向本机 IP 的记录:要提醒: %+v", r)
	}
	// 凭据失效(服务商回 403):标红、告警一次
	reject.Store(true)
	clk.add(dnsHealthEvery + time.Minute)
	s.dnsHealthTick()
	if r := ready(); r["takeSubDnsErr"].Level != "bad" || r["takeSubDns"].Code != "" {
		t.Fatalf("自检失败:标红,不再显示正常: %+v", r)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "DNS 接口用不了了") {
		t.Fatalf("告警一次: %q", sent)
	}
	clk.add(dnsHealthEvery + time.Minute)
	s.dnsHealthTick()
	if len(sent) != 1 {
		t.Fatal("一直失败:不重复告警")
	}
	// 没到时间、凭据也没换:不重查
	s.dnsHealthTick()
	// 恢复
	reject.Store(false)
	clk.add(dnsHealthEvery + time.Minute)
	s.dnsHealthTick()
	if len(sent) != 2 || !strings.Contains(sent[1], "恢复") {
		t.Fatalf("恢复了说一声: %q", sent)
	}
	// 关掉接管订阅:不查、不显示
	s.set(map[string]string{KeyTakeSub: "false"})
	s.dnsHealthTick()
	if checked, _, _ := s.dnsHealthView(); checked {
		t.Fatal("没开接管订阅:不查")
	}
}

// rejectAll 服务商拒绝一切(令牌被删、权限被收回)。
type rejectAll struct{}

func (rejectAll) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`))
}
