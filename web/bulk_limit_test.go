package web

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 代理批量生成要按整批算用户数上限:事务里逐个校验看不到本事务刚插入的用户,以前上限 3 的代理一次能建 5 个(审计 MB21)。
// 目前代理面板挡住了批量生成,这里直接带代理作用域调处理函数,钉住以后开放时不被绕过。
func TestResellerBulkRespectsUserLimit(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})
	rs := model.Reseller{Name: "dl", Enabled: true, UserLimit: 3}
	db.Create(&rs)
	s.setResellerLines(rs.Id, []uint{1})
	db.Create(&model.User{Name: "old", ResellerId: rs.Id, Enabled: true, SubToken: "o1o1o1o1o1o1o1o1o1o1o1o1"})

	req := httptest.NewRequest("POST", "http://x/app/api/users/bulk", strings.NewReader(`{"prefix":"b","count":5}`))
	req = req.WithContext(context.WithValue(req.Context(), scopeKey, rs.Id))
	w := httptest.NewRecorder()
	s.handleUsersBulk(w, req)
	var n int64
	db.Model(&model.User{}).Where("reseller_id = ?", rs.Id).Count(&n)
	if w.Code != 400 || n != 1 {
		t.Fatalf("超出上限的整批应拒绝且一个不建: %d %s,名下 %d 个", w.Code, w.Body.String(), n)
	}
}
