package sub

import (
	"net/http/httptest"
	"testing"
)

// 订阅日志的来源 IP:公网对端直接用对端地址,伪造的 X-Forwarded-For 不算;同机反代转过来时取最后一段。
func TestSubLogClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/sub/x", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	if ip := clientIP(r); ip != "203.0.113.9" {
		t.Fatalf("公网对端伪造的 X-Forwarded-For 不能采信: %s", ip)
	}
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	if ip := clientIP(r); ip != "198.51.100.7" {
		t.Fatalf("同机反代应取最后一段: %s", ip)
	}
	r.Header.Del("X-Forwarded-For")
	if ip := clientIP(r); ip != "127.0.0.1" {
		t.Fatalf("没有转发头就是对端: %s", ip)
	}
}
