package web

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 新建线路勾「分配给全部现有用户」只分给主面板用户:代理名下的用户只能用授权给代理的线路,
// 新线路此时还没授权给任何代理,分过去就是越权(以前代理用户会直接拿到这条线路)。
func TestAssignLineToMainUsersSkipsResellerUsers(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	rs := model.Reseller{Name: "r", Enabled: true}
	db.Create(&rs)
	main := model.User{Name: "m", Enabled: true}
	sub := model.User{Name: "ru", Enabled: true, ResellerId: rs.Id}
	db.Create(&main)
	db.Create(&sub)
	line := model.Line{Name: "new", Protocol: "hysteria2", Port: 30443, Enabled: true}
	db.Create(&line)

	if err := assignLineToMainUsers(db, line.Id); err != nil {
		t.Fatal(err)
	}
	var got []uint
	db.Model(&model.UserLine{}).Where("line_id = ?", line.Id).Pluck("user_id", &got)
	if len(got) != 1 || got[0] != main.Id {
		t.Fatalf("只应分给主面板用户 #%d,实际 %v", main.Id, got)
	}
}
