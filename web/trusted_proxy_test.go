package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// 面板挂在同机反代后面时,所有请求的 TCP 对端都是 127.0.0.1:限流按对端计数,一个配错令牌 / 猜密码的来源
// 就把所有人一起挡住。设了「受信任的反向代理」后,只有对端在名单里时才按 X-Forwarded-For(从右往左第一个
// 不受信任的地址)/ X-Real-IP 认来源;没设时和以前一样只看对端。

type proxyReq struct {
	peer, xff, realIP string
}

func newTrustedProxyServer(t *testing.T, trusted string) *Server {
	t.Helper()
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(run.DB()) })
	s := NewServer(run)
	db := run.DB()
	db.Create(&model.Setting{Key: "apiEnabled", Value: "true"})
	db.Create(&model.Setting{Key: "apiToken", Value: "good-token-0123456789"})
	db.Create(&model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-token-0123456789"})
	if trusted != "" {
		db.Create(&model.Setting{Key: "trustedProxies", Value: trusted})
	}
	return s
}

func (p proxyReq) apply(r *http.Request) {
	r.RemoteAddr = p.peer
	if p.xff != "" {
		r.Header.Set("X-Forwarded-For", p.xff)
	}
	if p.realIP != "" {
		r.Header.Set("X-Real-IP", p.realIP)
	}
}

func apiCall(s *Server, reseller bool, p proxyReq, token string) int {
	path := "http://x/app/api/v1/users"
	if reseller {
		path = "http://x/dl/api/v1/users"
	}
	r := httptest.NewRequest("GET", path, nil)
	p.apply(r)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	if reseller {
		s.handleResellerPublicAPI(w, r)
	} else {
		s.handlePublicAPI(w, r)
	}
	return w.Code
}

func TestTrustedProxyAPILimitPerClient(t *testing.T) {
	s := newTrustedProxyServer(t, "127.0.0.1, ::1")
	bad := proxyReq{peer: "127.0.0.1:50000", xff: "203.0.113.7"}
	for i := 0; i < 10; i++ {
		if c := apiCall(s, i%2 == 1, bad, "guess"); c != 401 {
			t.Fatalf("第 %d 次错令牌应 401: %d", i+1, c)
		}
	}
	if c := apiCall(s, false, bad, "good-token-0123456789"); c != http.StatusTooManyRequests {
		t.Fatalf("经反代来的这个来源失败 10 次后应被挡: %d", c)
	}
	// 同一个反代后面的另一个调用方不受牵连
	if c := apiCall(s, false, proxyReq{peer: "127.0.0.1:50001", xff: "198.51.100.9"}, "good-token-0123456789"); c != 200 {
		t.Fatalf("反代后面别的来源不该被一起挡: %d", c)
	}
	if c := apiCall(s, true, proxyReq{peer: "[::1]:50002", xff: "198.51.100.9"}, "rs-token-0123456789"); c != 200 {
		t.Fatalf("代理入口别的来源不该被一起挡: %d", c)
	}
	// 被挡的来源自己往 X-Forwarded-For 前面塞别人的地址:反代追加真实对端,取最右边不受信任的一段,照样挡住
	if c := apiCall(s, false, proxyReq{peer: "127.0.0.1:50003", xff: "198.51.100.9, 203.0.113.7"}, "good-token-0123456789"); c != http.StatusTooManyRequests {
		t.Fatalf("伪造的前段不能绕过限流: %d", c)
	}
	// 没有 X-Forwarded-For 时用 X-Real-IP
	if c := apiCall(s, false, proxyReq{peer: "127.0.0.1:50004", realIP: "203.0.113.7"}, "good-token-0123456789"); c != http.StatusTooManyRequests {
		t.Fatalf("X-Real-IP 指出的被挡来源应被挡: %d", c)
	}
	if c := apiCall(s, false, proxyReq{peer: "127.0.0.1:50005", realIP: "198.51.100.10"}, "good-token-0123456789"); c != 200 {
		t.Fatalf("X-Real-IP 指出的别的来源不受影响: %d", c)
	}
	// 不在名单里的对端写的转发头一概不认:被挡的来源直连时声称自己是别人也没用
	if c := apiCall(s, false, proxyReq{peer: "203.0.113.7:40000", xff: "198.51.100.11", realIP: "198.51.100.11"}, "good-token-0123456789"); c != http.StatusTooManyRequests {
		t.Fatalf("不受信任的对端写的转发头不能采信: %d", c)
	}
}

// 默认(没设受信任的反代):行为和以前一样,只看 TCP 对端,转发头不参与限流。
func TestTrustedProxyDefaultUsesPeer(t *testing.T) {
	s := newTrustedProxyServer(t, "")
	for i := 0; i < 10; i++ {
		apiCall(s, false, proxyReq{peer: "127.0.0.1:50000", xff: "203.0.113.7"}, "guess")
	}
	if c := apiCall(s, false, proxyReq{peer: "127.0.0.1:50001", xff: "198.51.100.9"}, "good-token-0123456789"); c != http.StatusTooManyRequests {
		t.Fatalf("没设受信任的反代时仍按对端计数: %d", c)
	}
	if c := apiCall(s, false, proxyReq{peer: "192.0.2.1:50001", xff: "203.0.113.7"}, "good-token-0123456789"); c != 200 {
		t.Fatalf("别的对端不受影响: %d", c)
	}
}

func TestTrustedProxyLoginLimitPerClient(t *testing.T) {
	s := newTrustedProxyServer(t, "10.0.0.0/8\n127.0.0.1")
	login := func(reseller bool, p proxyReq) int {
		path := "http://x/app/api/login"
		if reseller {
			path = "http://x/dl/api/login"
		}
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"username":"nobody","password":"x"}`))
		p.apply(r)
		w := httptest.NewRecorder()
		if reseller {
			s.handleResellerLogin(w, r)
		} else {
			s.handleLogin(w, r)
		}
		return w.Code
	}
	// 两层反代:外层 10.1.2.3 转给本机 127.0.0.1;都在名单里,来源是最右边不受信任的 203.0.113.7
	bad := proxyReq{peer: "127.0.0.1:50000", xff: "203.0.113.7, 10.1.2.3"}
	for i := 0; i < 10; i++ {
		if c := login(i%2 == 1, bad); c != 401 {
			t.Fatalf("第 %d 次应 401: %d", i+1, c)
		}
	}
	if c := login(false, bad); c != http.StatusTooManyRequests {
		t.Fatalf("主面板:这个来源应被挡: %d", c)
	}
	if c := login(true, bad); c != http.StatusTooManyRequests {
		t.Fatalf("代理面板:这个来源应被挡: %d", c)
	}
	other := proxyReq{peer: "127.0.0.1:50001", xff: "198.51.100.9, 10.1.2.3"}
	if c := login(false, other); c != 401 {
		t.Fatalf("主面板:同一反代后面别的来源不该被挡: %d", c)
	}
	if c := login(true, other); c != 401 {
		t.Fatalf("代理面板:同一反代后面别的来源不该被挡: %d", c)
	}
}

func TestTrustedProxySourceIP(t *testing.T) {
	s := newTrustedProxyServer(t, "127.0.0.1, 10.0.0.0/8, ::1")
	cases := []struct {
		p    proxyReq
		want string
	}{
		{proxyReq{peer: "203.0.113.5:1", xff: "198.51.100.1"}, "203.0.113.5"},               // 对端不受信任:转发头不认
		{proxyReq{peer: "127.0.0.1:1"}, "127.0.0.1"},                                        // 受信任但没带头:对端
		{proxyReq{peer: "127.0.0.1:1", xff: "198.51.100.1"}, "198.51.100.1"},                // 单层
		{proxyReq{peer: "[::1]:1", xff: "1.1.1.1, 198.51.100.1, 10.2.2.2"}, "198.51.100.1"}, // 跳过受信任的一层,不认最左边伪造的
		{proxyReq{peer: "127.0.0.1:1", xff: "10.3.3.3, 10.2.2.2"}, "10.3.3.3"},              // 全是受信任的:取最左边
		{proxyReq{peer: "127.0.0.1:1", xff: "[2001:db8::1]:443"}, "2001:db8::1"},            // 带端口的写法
		{proxyReq{peer: "127.0.0.1:1", xff: "198.51.100.1:5555"}, "198.51.100.1"},
		{proxyReq{peer: "127.0.0.1:1", xff: "garbage", realIP: "198.51.100.2"}, "198.51.100.2"},
		{proxyReq{peer: "127.0.0.1:1", realIP: "nope"}, "127.0.0.1"},
	}
	for i, c := range cases {
		r := httptest.NewRequest("GET", "http://x/", nil)
		c.p.apply(r)
		if got := s.sourceIP(r); got != c.want {
			t.Errorf("#%d %+v: 得到 %s,应为 %s", i, c.p, got, c.want)
		}
	}
	// 多个 X-Forwarded-For 头按顺序拼起来
	r := httptest.NewRequest("GET", "http://x/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Add("X-Forwarded-For", "198.51.100.1")
	r.Header.Add("X-Forwarded-For", "198.51.100.3")
	if got := s.sourceIP(r); got != "198.51.100.3" {
		t.Errorf("多个头: %s", got)
	}
}

func TestTrustedProxySettingValidated(t *testing.T) {
	s := newTrustedProxyServer(t, "")
	post := func(v string) int {
		r := httptest.NewRequest("POST", "http://x/app/api/settings", strings.NewReader(`{"trustedProxies":`+strconvQuote(v)+`}`))
		w := httptest.NewRecorder()
		s.handleSettings(w, r)
		return w.Code
	}
	for _, bad := range []string{"not-an-ip", "10.0.0.0/33", "0.0.0.0/0", "::/0", "127.0.0.1, x"} {
		if c := post(bad); c != http.StatusBadRequest {
			t.Errorf("%q 应被拒: %d", bad, c)
		}
	}
	if c := post(" 127.0.0.1\n::1 ;10.1.2.3/8 "); c != 200 {
		t.Fatalf("合法名单应能保存: %d", c)
	}
	if v := s.setting("trustedProxies"); v != "127.0.0.1, ::1, 10.0.0.0/8" {
		t.Fatalf("应存规范化后的写法: %q", v)
	}
	if c := post(""); c != 200 || s.setting("trustedProxies") != "" {
		t.Fatalf("清空应能保存: %d %q", c, s.setting("trustedProxies"))
	}
}

func strconvQuote(v string) string { b, _ := json.Marshal(v); return string(b) }
