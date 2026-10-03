package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 批量生成的序号模式一律用随机订阅令牌:用户名作订阅地址时 vip001、vip002… 能顺着猜(审计 MB29)。
func TestBulkSeqUsesRandomSubToken(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	w := httptest.NewRecorder()
	s.handleUsersBulk(w, httptest.NewRequest("POST", "http://x/app/api/users/bulk", strings.NewReader(`{"prefix":"vip","count":3,"nameMode":"seq"}`)))
	if w.Code != 200 {
		t.Fatalf("批量生成失败: %d %s", w.Code, w.Body.String())
	}
	var users []model.User
	db.Find(&users)
	if len(users) != 3 {
		t.Fatalf("应生成 3 个: %d", len(users))
	}
	for _, u := range users {
		if u.SubToken == "" || strings.Contains(w.Body.String(), "/sub/"+u.Name) {
			t.Fatalf("序号模式的订阅地址不能是用户名: %+v\n%s", u, w.Body.String())
		}
	}
}
