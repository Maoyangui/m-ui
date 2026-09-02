package web

import (
	"net/http/httptest"
	"testing"
)

func TestSameOriginGuard(t *testing.T) {
	mk := func(method string, headers map[string]string) bool {
		r := httptest.NewRequest(method, "http://panel.example.com:2053/app/api/users", nil)
		r.Host = "panel.example.com:2053"
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return sameOrigin(r)
	}
	if !mk("GET", map[string]string{"Sec-Fetch-Site": "cross-site"}) {
		t.Fatal("GET 不受限")
	}
	if !mk("POST", nil) {
		t.Fatal("无头(脚本/老浏览器/同源)应放行")
	}
	if !mk("POST", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://panel.example.com:2053"}) {
		t.Fatal("同源应放行")
	}
	if mk("POST", map[string]string{"Sec-Fetch-Site": "cross-site"}) {
		t.Fatal("跨站 POST 应拒绝")
	}
	if mk("POST", map[string]string{"Origin": "https://evil.example"}) {
		t.Fatal("Origin 不匹配应拒绝")
	}
	if mk("DELETE", map[string]string{"Sec-Fetch-Site": "same-site"}) {
		t.Fatal("same-site(子域)也应拒绝")
	}
	if !mk("POST", map[string]string{"Sec-Fetch-Site": "none"}) {
		t.Fatal("用户直接发起(none)应放行")
	}
}
