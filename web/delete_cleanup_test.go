package web

import (
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 删线路 / 删代理 / 删服务器之后,"线路 × 服务器"的收窄行不能留在库里。
// 留着的后果:快照里带着无主的行同步给每台副机;删服务器那种情况更糟 ——
// 只分到那台机器的用户,这条线路上一台都匹配不到,订阅里会悄无声息地少节点。
func TestDeleteCleansLineNodeRows(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	db.Create(&model.Node{Name: "A", ApiUrl: "http://a", Enabled: true, Sort: 2})
	db.Create(&model.Line{Name: "l1", Protocol: "hysteria2", Port: 30443, Enabled: true})
	db.Create(&model.Line{Name: "l2", Protocol: "anytls", Port: 30444, Enabled: true})
	u := model.User{Name: "u", Enabled: true}
	db.Create(&u)
	rs := model.Reseller{Name: "r", Enabled: true}
	db.Create(&rs)
	s.setUserLineRefs(u.Id, []model.LineRef{{LineId: 1, NodeIds: []uint{2}}, {LineId: 2, NodeIds: []uint{2}}})
	s.setResellerLineRefs(rs.Id, []model.LineRef{{LineId: 1, NodeIds: []uint{2}}, {LineId: 2, NodeIds: []uint{2}}})

	count := func(m interface{}, where string, args ...interface{}) int64 {
		var n int64
		db.Model(m).Where(where, args...).Count(&n)
		return n
	}
	if count(&model.UserLineNode{}, "1 = 1") != 2 || count(&model.ResellerLineNode{}, "1 = 1") != 2 {
		t.Fatal("前提:用户与代理各有两条收窄行")
	}

	// 删线路 1
	req := httptest.NewRequest("DELETE", "http://x/app/api/lines/1", nil)
	w := httptest.NewRecorder()
	s.handleLineItem(w, req)
	if w.Code != 200 {
		t.Fatalf("删线路应成功: %d %s", w.Code, w.Body.String())
	}
	if n := count(&model.UserLineNode{}, "line_id = ?", 1); n != 0 {
		t.Fatalf("线路删了,用户的收窄行应清掉,剩 %d", n)
	}
	if n := count(&model.ResellerLineNode{}, "line_id = ?", 1); n != 0 {
		t.Fatalf("线路删了,代理授权的收窄行应清掉,剩 %d", n)
	}

	// 删代理
	req = httptest.NewRequest("DELETE", "http://x/app/api/resellers/"+strconv.Itoa(int(rs.Id)), nil)
	w = httptest.NewRecorder()
	s.handleResellerItem(w, req)
	if w.Code != 200 {
		t.Fatalf("删代理应成功: %d %s", w.Code, w.Body.String())
	}
	if n := count(&model.ResellerLineNode{}, "reseller_id = ?", rs.Id); n != 0 {
		t.Fatalf("代理删了,它的收窄行应清掉,剩 %d", n)
	}

	// 删服务器 A:指向它的收窄行要清掉,用户回到"这条线路的全部服务器"
	if n := count(&model.UserLineNode{}, "node_id = ?", 2); n != 1 {
		t.Fatalf("前提:还剩一条指向 A 的收窄行,实际 %d", n)
	}
	req = httptest.NewRequest("DELETE", "http://x/app/api/nodes/2", nil)
	w = httptest.NewRecorder()
	s.handleNodeItem(w, req)
	if w.Code != 200 {
		t.Fatalf("删服务器应成功: %d %s", w.Code, w.Body.String())
	}
	if n := count(&model.UserLineNode{}, "node_id = ?", 2); n != 0 {
		t.Fatalf("服务器删了,指向它的收窄行应清掉,剩 %d", n)
	}
	if refs := s.userLineRefs(u.Id); len(refs) != 1 || refs[0].LineId != 2 || len(refs[0].NodeIds) != 0 {
		t.Fatalf("清掉之后应回到「该线路的全部服务器」: %+v", refs)
	}
}
