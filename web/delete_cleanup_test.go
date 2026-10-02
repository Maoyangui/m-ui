package web

import (
	"encoding/json"
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

	// 删服务器 A:只收窄到 A 的那条分配撤掉(以前清掉收窄行就退回"该线路的全部服务器",范围静默扩大,审计 M062);
	// 收窄里还有别的机器的只去掉 A。受影响的人列在返回里
	if n := count(&model.UserLineNode{}, "node_id = ?", 2); n != 1 {
		t.Fatalf("前提:还剩一条指向 A 的收窄行,实际 %d", n)
	}
	db.Create(&model.Node{Name: "C", ApiUrl: "http://c", Enabled: true, Sort: 3}) // 线路部署在三台上,收窄到主机与 A 才算真收窄
	u2 := model.User{Name: "u2", Enabled: true}
	db.Create(&u2)
	s.setUserLineRefs(u2.Id, []model.LineRef{{LineId: 2, NodeIds: []uint{1, 2}}})
	req = httptest.NewRequest("DELETE", "http://x/app/api/nodes/2", nil)
	w = httptest.NewRecorder()
	s.handleNodeItem(w, req)
	if w.Code != 200 {
		t.Fatalf("删服务器应成功: %d %s", w.Code, w.Body.String())
	}
	if n := count(&model.UserLineNode{}, "node_id = ?", 2); n != 0 {
		t.Fatalf("服务器删了,指向它的收窄行应清掉,剩 %d", n)
	}
	if refs := s.userLineRefs(u.Id); len(refs) != 0 {
		t.Fatalf("只收窄到 A 的分配应撤掉,不能退回全部服务器: %+v", refs)
	}
	if refs := s.userLineRefs(u2.Id); len(refs) != 1 || len(refs[0].NodeIds) != 1 || refs[0].NodeIds[0] != 1 {
		t.Fatalf("收窄到主机与 A 的应只剩主机: %+v", refs)
	}
	var resp struct {
		RevokedUsers []string `json:"revokedUsers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.RevokedUsers) != 1 || resp.RevokedUsers[0] != "u" {
		t.Fatalf("返回里应列出被撤分配的用户 u: %s", w.Body.String())
	}
}
