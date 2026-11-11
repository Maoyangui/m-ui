package sub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/util/slowreq"
)

// 订阅服务的慢请求日志:订阅路径记成 {sub},订阅键(用户名 / 订阅令牌 / 共享令牌)一律打码,只留 /qr、/logo 这样的固定后缀。
func TestSubRouteShapeMasksKey(t *testing.T) {
	for in, want := range map[string]string{
		"/s3cret/alice":                   "{sub}/:key",
		"/s3cret/Zx9Kq2Lm7Zp4Rt8Wv1Yb":    "{sub}/:key",
		"/s3cret/Zx9Kq2Lm7Zp4Rt8Wv1Yb/qr": "{sub}/:key/qr",
		"/s3cret/alice/logo":              "{sub}/:key/logo",
		"/s3cret/":                        "{sub}/",
		"/other/alice":                    "{other}",
	} {
		if got := subRouteShape(in, "/s3cret/"); got != want {
			t.Errorf("subRouteShape(%q) = %q,应为 %q", in, got, want)
		}
	}
	// 走一遍慢请求日志:日志里既没有订阅路径也没有订阅键,查询串也不带
	now := time.Unix(1_700_000_000, 0)
	var got []string
	l := &slowreq.Logger{Name: "订阅", Now: func() time.Time { return now },
		Shape: func(r *http.Request) string { return subRouteShape(r.URL.Path, "/s3cret/") },
		Log:   func(m string) { got = append(got, m) }}
	h := l.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { now = now.Add(4 * time.Second) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://x/s3cret/Zx9Kq2Lm7Zp4Rt8Wv1Yb?flag=clash", nil))
	if len(got) != 1 {
		t.Fatalf("慢请求应记一条: %v", got)
	}
	for _, leak := range []string{"s3cret", "Zx9Kq2Lm7Zp4Rt8Wv1Yb", "clash"} {
		if strings.Contains(got[0], leak) {
			t.Fatalf("订阅慢请求日志泄露了 %q: %s", leak, got[0])
		}
	}
	if !strings.Contains(got[0], "GET {sub}/:key → 200") {
		t.Fatalf("日志应写明路由形状与状态码: %s", got[0])
	}
}
