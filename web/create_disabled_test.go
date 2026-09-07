package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 建号时勾了"停用":gorm 的 default:true 会把 false 写成 true,以前建出来的是启用的用户。
func TestCreateUserDisabledStaysDisabled(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	req := httptest.NewRequest("POST", "http://x/app/api/users", strings.NewReader(`{"name":"bob","enabled":false}`))
	w := httptest.NewRecorder()
	s.handleUsers(w, req)
	if w.Code != 200 {
		t.Fatalf("建号失败: %d %s", w.Code, w.Body.String())
	}
	var u model.User
	if err := db.Where("name = ?", "bob").First(&u).Error; err != nil {
		t.Fatal(err)
	}
	if u.Enabled || u.DisabledReason != model.DisabledManual {
		t.Fatalf("建号时停用的用户应保持停用并记为手动: enabled=%v reason=%q", u.Enabled, u.DisabledReason)
	}
	// 清用量不该让他复活(手动停用)
	if err := s.resetUsage(db, u.Id); err != nil {
		t.Fatal(err)
	}
	db.First(&u, u.Id)
	if u.Enabled {
		t.Fatal("手动停用的用户重置用量后不该自动启用")
	}
}
