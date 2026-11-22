package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 副机上的外部 API 只读:写操作会被主机下一次快照覆盖,调用方却拿到了"成功"。
func TestPublicAPIReadOnlyOnNode(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	for k, v := range map[string]string{"nodeMode": "true", "apiEnabled": "true", "apiToken": "tok-0123456789"} {
		db.Create(&model.Setting{Key: k, Value: v})
	}
	db.Create(&model.User{Name: "alice", Enabled: true})
	call := func(method, path, body string) int {
		r := httptest.NewRequest(method, "http://x/app/api/v1/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer tok-0123456789")
		w := httptest.NewRecorder()
		s.handlePublicAPI(w, r)
		return w.Code
	}
	if c := call("GET", "ping", ""); c != 200 {
		t.Fatalf("副机上 ping 应照常,得 %d", c)
	}
	if c := call("GET", "users", ""); c != 200 {
		t.Fatalf("副机上只读应照常,得 %d", c)
	}
	for _, req := range [][3]string{{"POST", "users", `{"name":"bob"}`}, {"DELETE", "users/alice", ""}, {"PATCH", "users/alice", `{"enabled":false}`}, {"POST", "users/alice/rotate", ""}} {
		if c := call(req[0], req[1], req[2]); c != 403 {
			t.Fatalf("副机上 %s %s 应 403,得 %d", req[0], req[1], c)
		}
	}
	var n int64
	db.Model(&model.User{}).Where("name = ?", "alice").Count(&n)
	if n != 1 {
		t.Fatal("被拒的写操作不该改库")
	}
	// 用户名正好叫 kick:改 / 删这个用户的路径也以 /kick 结尾,不能被当成踢线放过去;真正的踢线照样放行
	db.Create(&model.User{Name: "kick", Enabled: true})
	for _, req := range [][3]string{{"DELETE", "users/kick", ""}, {"PATCH", "users/kick", `{"enabled":false}`}, {"PUT", "users/kick", `{"remark":"x"}`}} {
		if c := call(req[0], req[1], req[2]); c != 403 {
			t.Fatalf("副机上 %s %s 应 403,得 %d", req[0], req[1], c)
		}
	}
	if c := call("POST", "users/kick/kick", ""); c == 403 {
		t.Fatalf("副机上踢线应照常放行,得 %d", c)
	}
	var k model.User
	if db.Where("name = ?", "kick").First(&k).Error != nil || !k.Enabled {
		t.Fatal("被拒的写操作不该动用户 kick")
	}
}

// isKickPath 只认 users/{名字}/kick;名字叫 kick 的用户本身、别的子接口都不算。
func TestIsKickPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/app/api/v1/users/bob/kick":       true,
		"/app/api/v1/users/kick/kick":      true,
		"/app/api/v1/users/bob/kick/":      true,
		"/app/api/v1/users/kick":           false,
		"/app/api/v1/users/kick/disable":   false,
		"/app/api/v1/kick":                 false,
		"/app/api/v1/users//kick":          false,
		"/api/v1/api/v1/users/bob/kick":    true,
		"/app/api/v1/users/bob/kick/extra": false,
	} {
		if got := isKickPath(p); got != want {
			t.Errorf("isKickPath(%q) = %v,应为 %v", p, got, want)
		}
	}
}
