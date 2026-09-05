package render

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 用户在某条线路上收窄到具体服务器时,只有那台服务器的入站带这个用户;没有收窄 = 全部服务器。
func TestLineUsersPerNode(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "B", ApiUrl: "http://b", Enabled: true})
	db.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})
	all := model.User{Name: "all", Enabled: true}
	onlyB := model.User{Name: "onlyB", Enabled: true}
	db.Create(&all)
	db.Create(&onlyB)
	db.Create(&model.UserLine{UserId: all.Id, LineId: 1})
	db.Create(&model.UserLine{UserId: onlyB.Id, LineId: 1})
	db.Create(&model.UserLineNode{UserId: onlyB.Id, LineId: 1, NodeId: 2})

	names := func(self uint) []string {
		by, err := loadLineUsers(db, self)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, u := range by[1] {
			out = append(out, u.Name)
		}
		return out
	}
	if got := names(1); len(got) != 1 || got[0] != "all" {
		t.Fatalf("主机上应只有 all: %v", got)
	}
	if got := names(2); len(got) != 2 {
		t.Fatalf("B 上应有两人: %v", got)
	}
	if got := names(0); len(got) != 2 {
		t.Fatalf("没有本机记录时按全部算: %v", got)
	}
}
