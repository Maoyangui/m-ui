package web

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 主面板收回代理的线路后:名下用户的分配在面板上按授权显示(收回的不再出现),
// 代理续费 / 经外部 API 改用户不会再因为这条被收回的线路报"含未授权的线路";库里的分配不动。
func TestRevokedGrantNoLongerBlocksReseller(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Line{Name: "A", Protocol: "hysteria2", Port: 30443, Enabled: true})
	db.Create(&model.Line{Name: "B", Protocol: "anytls", Port: 30444, Enabled: true})
	rs := model.Reseller{Name: "r", Enabled: true}
	db.Create(&rs)
	if err := s.setResellerLines(rs.Id, []uint{1, 2}); err != nil {
		t.Fatal(err)
	}
	u := model.User{Name: "ru", Enabled: true, ResellerId: rs.Id}
	db.Create(&u)
	s.setUserLines(u.Id, []uint{1, 2})

	if err := s.setResellerLines(rs.Id, []uint{1}); err != nil { // 收回 B
		t.Fatal(err)
	}
	if refs := s.userLineRefs(u.Id); len(refs) != 1 || refs[0].LineId != 1 {
		t.Fatalf("生效的分配应只剩 A: %+v", refs)
	}
	if m := s.userLineRefMap(); len(m[u.Id]) != 1 {
		t.Fatalf("列表里也应只剩 A: %+v", m[u.Id])
	}
	if err := s.checkResellerPlan(rs.Id, u, model.Plan{Name: "p", ResellerId: rs.Id}); err != nil {
		t.Fatalf("不带线路的套餐续费不该被收回的线路挡住: %v", err)
	}
	if err := s.checkResellerUser(rs.Id, u.Id, &u, s.userLineRefs(u.Id)); err != nil {
		t.Fatalf("不改线路的编辑不该被挡: %v", err)
	}
	var n int64
	db.Model(&model.UserLine{}).Where("user_id = ?", u.Id).Count(&n)
	if n != 2 {
		t.Fatalf("收回授权不删用户的分配,应仍有 2 条,实际 %d", n)
	}
}
