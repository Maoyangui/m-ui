package web

import (
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// 流量时序按用户名记:删号一并删掉、改名跟着搬,同名的新用户看不到前一个人的流量历史(审计 MB28)。
func TestUserStatsFollowDeleteAndRename(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s := NewServer(run)
	db := run.DB()
	count := func(name string) int64 {
		var n int64
		db.Model(&model.Stats{}).Where("resource = ? AND tag = ?", "user", name).Count(&n)
		return n
	}
	alice := model.User{Name: "alice", Enabled: true, SubToken: "a1a1a1a1a1a1a1a1a1a1a1a1", Credentials: []byte(`{}`)}
	db.Create(&alice)
	db.Create(&model.Stats{DateTime: 1000, Resource: "user", Tag: "alice", Direction: true, Traffic: 5})
	db.Create(&model.Stats{DateTime: 1000, Resource: "line", Tag: "alice", Direction: true, Traffic: 7}) // 同名的别的维度不动

	w := httptest.NewRecorder()
	body := `{"name":"alice2","enabled":true,"lineIds":[],"extIds":[]}`
	s.handleUserItem(w, httptest.NewRequest("PUT", "http://x/app/api/users/"+strconv.Itoa(int(alice.Id)), strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("改名失败: %d %s", w.Code, w.Body.String())
	}
	if count("alice") != 0 || count("alice2") != 1 {
		t.Fatalf("改名后时序应搬到新名下: 旧 %d 新 %d", count("alice"), count("alice2"))
	}

	db.First(&alice, alice.Id)
	if err := s.deleteUser(alice, "test"); err != nil {
		t.Fatal(err)
	}
	if count("alice2") != 0 {
		t.Fatal("删号后该用户名的时序应删掉")
	}
	var line int64
	db.Model(&model.Stats{}).Where("resource = ?", "line").Count(&line)
	if line != 1 {
		t.Fatal("线路维度的同名时序不该被删")
	}
}
