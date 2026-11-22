package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 用户名正好叫 v1 / api 时外部 API 的子接口照常能用(以前路由按最后一个 /v1/ 切路径,「…/users/v1/traffic」只剩「traffic」,
// 这个用户的流量、订阅地址、启停等全部 404 —— v0.8.1 就有);面板路径自己含 /api/v1/ 时也一样;
// 调用日志记下的路径、动作、对象和路由看到的一致。
func TestPublicAPIUserNamedV1(t *testing.T) {
	e := newPCEnv(t)
	for _, n := range []string{"v1", "api", "bob"} {
		e.mkUser(model.User{Name: n}, 1)
	}
	k := e.mkKey(0, "测试", permUsersRead, permUsersUpdate)
	src := "203.0.113.9:4000"
	for _, name := range []string{"bob", "v1", "api"} {
		for _, sub := range []string{"", "/traffic", "/sub", "/plan-ops"} {
			if w := e.keyCall(false, src, k.Token, "GET", "users/"+name+sub, ""); w.Code != 200 {
				t.Fatalf("GET users/%s%s 应 200,得 %d %s", name, sub, w.Code, w.Body.String())
			}
		}
	}
	if w := e.keyCall(false, src, k.Token, "POST", "users/v1/disable", ""); w.Code != 200 {
		t.Fatalf("POST users/v1/disable 应 200,得 %d %s", w.Code, w.Body.String())
	}
	var u model.User
	e.db.Where("name = ?", "v1").First(&u)
	if u.Enabled {
		t.Fatal("停用 v1 没生效")
	}
	// 面板路径本身是 /api/v1/:挂载点在 /api/v1/api/v1/
	r := httptest.NewRequest("GET", "http://panel.example:2053/api/v1/api/v1/users/v1/traffic", strings.NewReader(""))
	r.RemoteAddr = src
	r.Header.Set("Authorization", "Bearer "+k.Token)
	w := httptest.NewRecorder()
	e.s.handlePublicAPI(w, r)
	if w.Code != 200 {
		t.Fatalf("面板路径是 /api/v1/ 时 GET users/v1/traffic 应 200,得 %d %s", w.Code, w.Body.String())
	}
	// 不是接口的路径照旧 404
	for _, p := range []string{"", "v1", "nope"} {
		if w := e.keyCall(false, src, k.Token, "GET", p, ""); w.Code != 404 {
			t.Fatalf("GET %q 应 404,得 %d", p, w.Code)
		}
	}
	// 日志:v1 的子接口记成 user.traffic / user.disable,对象是 v1,路径从 /api/v1/users/v1 起
	rows := waitLogs(t, e, k.Id, 16)
	seen := map[string]bool{}
	for _, x := range rows {
		if x.Target == "v1" {
			seen[x.Action] = true
			if !strings.HasPrefix(x.Path, "/api/v1/users/v1") {
				t.Fatalf("v1 那条日志的路径不对: %+v", x)
			}
		}
	}
	for _, a := range []string{"user.get", "user.traffic", "user.sub", "user.planOps", "user.disable"} {
		if !seen[a] {
			t.Fatalf("v1 的日志里缺 %s,得 %v", a, seen)
		}
	}
}
