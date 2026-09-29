package web

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/runner"
)

// 登录限流:IPv6 按 /64 计数(换源地址绕不过去);不存在的用户名同样计入失败;没失败过的来源不建条目。
func TestLoginLimitPerPrefixAndUnknownUser(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s := NewServer(run)
	login := func(addr, user string) int {
		r := httptest.NewRequest("POST", "http://x/app/api/login", strings.NewReader(`{"username":"`+user+`","password":"x"}`))
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		s.handleLogin(w, r)
		return w.Code
	}
	for i := 1; i <= 10; i++ {
		if c := login(fmt.Sprintf("[2001:db8:1:2::%x]:5000", i), "nobody"); c != 401 {
			t.Fatalf("第 %d 次应是 401,得 %d", i, c)
		}
	}
	if c := login("[2001:db8:1:2:ffff::1]:5000", "admin"); c != 429 {
		t.Fatalf("同一 /64 里换地址、用不存在的用户名失败 10 次后应被挡,得 %d", c)
	}
	if c := login("[2001:db8:1:3::1]:5000", "nobody"); c != 401 {
		t.Fatalf("别的 /64 不受影响,得 %d", c)
	}
	s.loginBlocked("198.51.100.9") // 没失败过的来源
	s.mu.Lock()
	n := len(s.loginFails)
	s.mu.Unlock()
	if n != 2 {
		t.Fatalf("只有失败过的来源(两个 /64)才有条目,实际 %d", n)
	}
}

// 来源 IP 只在请求来自本机反代时才采信 X-Forwarded-For(取最后一段),直连时谁写都不认。
func TestClientIPTrustsForwardedOnlyFromLoopback(t *testing.T) {
	r := httptest.NewRequest("GET", "http://x/", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 198.51.100.2")
	r.RemoteAddr = "192.0.2.10:4000"
	if got := clientIP(r); got != "192.0.2.10" {
		t.Fatalf("直连时应取对端地址,得 %s", got)
	}
	r.RemoteAddr = "127.0.0.1:4000"
	if got := clientIP(r); got != "198.51.100.2" {
		t.Fatalf("本机反代时取最后一段,得 %s", got)
	}
}
