package sub

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/camo"
)

// 订阅端口(用户 N2):根目录、陌生路径、订阅路径本身(没有用户名)开着页面伪装时回应伪装站,没开是普通 404;
// 正确的订阅地址、用户名不对时的「订阅地址无效」说明页都不受影响。
func TestSubRootCamouflage(t *testing.T) {
	s, _ := shareServer(t)
	t.Cleanup(func() { camo.SetSource(nil) })
	paths := []string{"/", "/wp-admin/", "/robots.txt", "/sub/"}
	camo.SetSource(func() http.Handler { return nil })
	for _, p := range paths {
		if w := doReq(s, http.MethodGet, p, "Mozilla/5.0"); w.Code != 404 || strings.Contains(w.Body.String(), "<html") {
			t.Fatalf("没开页面伪装时 %s 应是普通 404: %d", p, w.Code)
		}
	}
	site := camo.New("mirror", 11, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
	camo.SetSource(func() http.Handler { return site })
	if w := doReq(s, http.MethodGet, "/", "Mozilla/5.0"); w.Code != 200 || !strings.Contains(w.Body.String(), site.Name()) {
		t.Fatalf("开着页面伪装时根目录应是伪装站: %d", w.Code)
	}
	// 订阅路径本身(没有用户名)交给伪装站:伪装站里没有这一页,回的是它自己风格的 404
	if w := doReq(s, http.MethodGet, "/sub/", "Mozilla/5.0"); w.Code != 404 || !strings.Contains(w.Body.String(), site.Name()) || !strings.Contains(w.Body.String(), "<b>404</b>") {
		t.Fatalf("开着页面伪装时 /sub/ 应是伪装站的 404: %d", w.Code)
	}
	if w := doReq(s, http.MethodGet, "/robots.txt", "Mozilla/5.0"); w.Code != 200 || !strings.Contains(w.Body.String(), "User-agent") {
		t.Fatalf("伪装站的 robots.txt: %d", w.Code)
	}
	if w := doReq(s, http.MethodGet, "/wp-admin/", "Mozilla/5.0"); w.Code != 404 || !strings.Contains(w.Body.String(), site.Name()) {
		t.Fatalf("陌生路径是伪装站自己的 404: %d", w.Code)
	}
	if w := doReq(s, http.MethodGet, "/sub/alice", "clash"); w.Code != 200 || strings.Contains(w.Body.String(), site.Name()) {
		t.Fatalf("正确的订阅地址照常: %d", w.Code)
	}
	if w := doReq(s, http.MethodGet, "/sub/nobody", "Mozilla/5.0"); strings.Contains(w.Body.String(), site.Name()) {
		t.Fatal("订阅路径下用户名不对时照旧是「订阅地址无效」的说明页,不是伪装站")
	}
}
