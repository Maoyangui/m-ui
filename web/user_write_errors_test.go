package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 分配写不进去时不能报成功:建号 / 改号整体回滚并返回错误;外部 API 删号失败也不能回 ok。
func TestUserWritesReportAssignmentErrors(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	for k, v := range map[string]string{"apiEnabled": "true", "apiToken": "tok-0123456789"} {
		db.Create(&model.Setting{Key: k, Value: v})
	}
	db.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})
	alice := model.User{Name: "alice", Enabled: true, Remark: "old"}
	db.Create(&alice)
	db.Exec("DROP TABLE user_exts") // 模拟写失败(磁盘满、锁等太久)

	panel := func(method, path, body string) int {
		r := httptest.NewRequest(method, "http://x/app/api/"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		if path == "users" {
			s.handleUsers(w, r)
		} else {
			s.handleUserItem(w, r)
		}
		return w.Code
	}
	api := func(method, path, body string) int {
		r := httptest.NewRequest(method, "http://x/app/api/v1/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer tok-0123456789")
		w := httptest.NewRecorder()
		s.handlePublicAPI(w, r)
		return w.Code
	}
	if c := panel("POST", "users", `{"name":"bob","enabled":true,"lineIds":[1]}`); c == 200 {
		t.Fatal("分配写失败时建号不该报成功")
	}
	if c := api("POST", "users", `{"name":"carol","lineIds":[1],"extIds":[]}`); c == 200 {
		t.Fatal("外部 API 建号同样不该报成功")
	}
	var n int64
	db.Model(&model.User{}).Where("name IN ?", []string{"bob", "carol"}).Count(&n)
	if n != 0 {
		t.Fatal("失败的建号应整体回滚,不留下没有线路的用户")
	}
	if c := panel("PUT", "users/1", `{"name":"alice","enabled":true,"remark":"new","lineIds":[1]}`); c == 200 {
		t.Fatal("分配写失败时改号不该报成功")
	}
	var cur model.User
	db.First(&cur, alice.Id)
	if cur.Remark != "old" {
		t.Fatal("失败的改号应整体回滚")
	}
	if c := api("DELETE", "users/alice", ""); c == 200 {
		t.Fatal("删号失败(用户还在)不该回 ok")
	}
}
