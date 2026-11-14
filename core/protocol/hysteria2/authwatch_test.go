package hysteria2

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagernet/sing-box/adapter/inbound"
	C "github.com/sagernet/sing-box/constant"
)

type rec struct{ got []string }

func (r *rec) NoteFailure(ip, in, proto, reason, detail string) {
	r.got = append(r.got, ip+"|"+in+"|"+proto+"|"+reason)
}

// 密码不对的认证请求落到伪装站点:记一笔"认证没过",回给对方的内容不变;访问伪装站点的普通请求不记;
// ignore_client_bandwidth + 限了下行时,没报下行带宽的认证请求可能密码是对的(sing-quic 也交给伪装站点),不记。
func TestAuthFailWatch(t *testing.T) {
	r := &rec{}
	in := &Inbound{Adapter: inbound.NewAdapter(C.TypeHysteria2, "hy"), arrivals: r}
	masq := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("masq"))
	})
	serve := func(a *authFailWatch, method, url string, rx string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, url, nil)
		req.RemoteAddr = "[2001:db8::9]:51000"
		if rx != "" {
			req.Header.Set("Hysteria-CC-RX", rx)
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, req)
		return w
	}
	a := &authFailWatch{next: masq, in: in}
	if w := serve(a, http.MethodPost, "https://hysteria/auth", "1000"); w.Code != http.StatusTeapot || w.Body.String() != "masq" {
		t.Fatalf("要原样交给伪装站点: %d %q", w.Code, w.Body.String())
	}
	serve(a, http.MethodGet, "https://hysteria/auth", "")        // 不是认证请求
	serve(a, http.MethodPost, "https://example.com/auth", "")    // 访问伪装站点
	serve(a, http.MethodPost, "https://hysteria/index.html", "") // 同上
	if len(r.got) != 1 || r.got[0] != "2001:db8::9|hy|hysteria2|auth" {
		t.Fatalf("记录 = %v", r.got)
	}
	amb := &authFailWatch{next: masq, in: in, rxAmbiguous: true}
	serve(amb, http.MethodPost, "https://hysteria/auth", "")
	serve(amb, http.MethodPost, "https://hysteria/auth", "0")
	if len(r.got) != 1 {
		t.Fatalf("分不清的不该记: %v", r.got)
	}
	serve(amb, http.MethodPost, "https://hysteria/auth", "5000000")
	if len(r.got) != 2 {
		t.Fatalf("报了带宽还落到伪装站点就是密码不对: %v", r.got)
	}
}
