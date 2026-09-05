package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 线路 × 服务器分配:整理、落库、回读、代理授权范围、套餐范围。
func TestLineRefs(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	db.Create(&model.Node{Name: "A", ApiUrl: "http://a", Enabled: true, Sort: 2})
	db.Create(&model.Node{Name: "B", ApiUrl: "http://b", Enabled: true, Sort: 3})
	db.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})                        // 全部服务器
	db.Create(&model.Line{Name: "ab", Protocol: "anytls", Port: 30444, Enabled: true, NodeIds: []byte(`[2,3]`)}) // 只在 A、B
	u := model.User{Name: "u", Enabled: true}
	db.Create(&u)

	// 整理:勾满 = 全部;勾一部分 = 收窄;勾了没部署的服务器 = 忽略;同一线路多条合并
	got := s.normalizeRefs([]model.LineRef{{LineId: 1, NodeIds: []uint{1, 2, 3}}, {LineId: 2, NodeIds: []uint{2}}, {LineId: 2, NodeIds: []uint{1}}})
	if len(got) != 2 || got[0].NodeIds != nil || len(got[1].NodeIds) != 1 || got[1].NodeIds[0] != 2 {
		t.Fatalf("整理结果不对: %+v", got)
	}
	if got := s.normalizeRefs([]model.LineRef{{LineId: 2, NodeIds: []uint{2}}, {LineId: 2}}); got[0].NodeIds != nil {
		t.Fatalf("任一处写了全部就是全部: %+v", got)
	}

	// 落库 + 回读
	s.setUserLineRefs(u.Id, []model.LineRef{{LineId: 1}, {LineId: 2, NodeIds: []uint{3}}})
	refs := s.userLineRefs(u.Id)
	if len(refs) != 2 || refs[0].NodeIds != nil || len(refs[1].NodeIds) != 1 || refs[1].NodeIds[0] != 3 {
		t.Fatalf("回读不对: %+v", refs)
	}
	var n int64
	db.Model(&model.UserLineNode{}).Where("user_id = ?", u.Id).Count(&n)
	if n != 1 {
		t.Fatalf("收窄行应只有 1 条,实际 %d", n)
	}
	if m := s.userLineRefMap(); len(m[u.Id]) != 2 {
		t.Fatalf("整表映射不对: %+v", m)
	}
	// 老写法(只给 lineIds)= 全部服务器
	s.setUserLines(u.Id, []uint{2})
	if refs := s.userLineRefs(u.Id); len(refs) != 1 || refs[0].NodeIds != nil {
		t.Fatalf("lineIds 应等于全部服务器: %+v", refs)
	}

	// 代理授权范围:hk 全部、ab 只有 A
	rs := model.Reseller{Name: "r", Enabled: true}
	db.Create(&rs)
	s.setResellerLineRefs(rs.Id, []model.LineRef{{LineId: 1}, {LineId: 2, NodeIds: []uint{2}}})
	if err := s.checkRefsGranted(rs.Id, []model.LineRef{{LineId: 1}, {LineId: 2, NodeIds: []uint{2}}}); err != nil {
		t.Fatalf("授权范围内应通过: %v", err)
	}
	if err := s.checkRefsGranted(rs.Id, []model.LineRef{{LineId: 2}}); err == nil || !strings.Contains(err.Error(), "部分服务器") {
		t.Fatalf("授权只有 A 时不能分配全部: %v", err)
	}
	if err := s.checkRefsGranted(rs.Id, []model.LineRef{{LineId: 2, NodeIds: []uint{3}}}); err == nil {
		t.Fatal("授权外的服务器应被拒")
	}
	if err := s.checkRefsGranted(rs.Id, []model.LineRef{{LineId: 1, NodeIds: []uint{3}}}); err != nil {
		t.Fatalf("线路授权了全部时可以只拿一台: %v", err)
	}
	// 授权行按具体服务器存着,但已经覆盖了线路现在部署的全部服务器 → 等于全部,拿全部也放行
	db.Where("reseller_id = ?", rs.Id).Delete(&model.ResellerLineNode{})
	db.Create(&model.ResellerLineNode{ResellerId: rs.Id, LineId: 2, NodeId: 2})
	db.Create(&model.ResellerLineNode{ResellerId: rs.Id, LineId: 2, NodeId: 3})
	if err := s.checkRefsGranted(rs.Id, []model.LineRef{{LineId: 2}}); err != nil {
		t.Fatalf("授权覆盖了全部部署时应等于全部: %v", err)
	}
	// 线路撤出了授权的那台服务器:授权不能反过来变成全部,这条线路代理什么都分不了
	db.Model(&model.Line{}).Where("id = ?", 2).Update("node_ids", []byte(`[3]`))
	db.Where("reseller_id = ? AND line_id = 2", rs.Id).Delete(&model.ResellerLineNode{})
	db.Create(&model.ResellerLineNode{ResellerId: rs.Id, LineId: 2, NodeId: 2})
	if err := s.checkRefsGranted(rs.Id, []model.LineRef{{LineId: 2}}); err == nil {
		t.Fatal("授权的服务器已没有这条线路时不能放宽成全部")
	}
	if err := s.checkRefsGranted(rs.Id, []model.LineRef{{LineId: 2, NodeIds: []uint{3}}}); err == nil {
		t.Fatal("授权外的服务器仍应被拒")
	}
	db.Model(&model.Line{}).Where("id = ?", 2).Update("node_ids", []byte(`[2,3]`))
	s.setResellerLineRefs(rs.Id, []model.LineRef{{LineId: 1}, {LineId: 2, NodeIds: []uint{2}}})

	// 套餐:LineIds + LineNodes
	p := model.Plan{Name: "p", LineIds: []byte(`[1,2]`), LineNodes: []byte(`{"2":[3]}`)}
	pr := planRefs(p)
	if len(pr) != 2 || pr[0].NodeIds != nil || len(pr[1].NodeIds) != 1 || pr[1].NodeIds[0] != 3 {
		t.Fatalf("套餐范围不对: %+v", pr)
	}
	bad := model.Plan{LineIds: []byte(`[1]`), LineNodes: []byte(`{"2":[3]}`)}
	if err := validatePlanRefs(&bad); err == nil {
		t.Fatal("范围里出现套餐没带的线路应被拒")
	}
	ids, nodes := lineRefsJSON(pr)
	if string(ids) != "[1,2]" || string(nodes) != `{"2":[3]}` {
		t.Fatalf("写回套餐字段不对: %s %s", ids, nodes)
	}

	// 用户接口:给 lineRefs 就按它落库;只给 lineIds 仍是全部
	req := httptest.NewRequest("POST", "http://x/app/api/users", strings.NewReader(`{"name":"v","enabled":true,"lineRefs":[{"lineId":2,"nodeIds":[2]}]}`))
	w := httptest.NewRecorder()
	s.handleUsers(w, req)
	if w.Code != 200 {
		t.Fatalf("建用户应通过: %d %s", w.Code, w.Body.String())
	}
	var v model.User
	db.Where("name = ?", "v").First(&v)
	if refs := s.userLineRefs(v.Id); len(refs) != 1 || len(refs[0].NodeIds) != 1 || refs[0].NodeIds[0] != 2 {
		t.Fatalf("接口写入的范围不对: %+v", refs)
	}
}
