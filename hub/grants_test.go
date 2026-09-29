package hub

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 快照下发的是与代理授权取过交集的分配:主机收回代理的线路,副机那边名下用户随之撤下(修订号变化触发推送);
// 授权表随快照带过去(副机提升为主机后还在),旧版主机不发授权表时副机不动本机的。
func TestSnapshotUsesEffectiveResellerLines(t *testing.T) {
	master, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(master)
	node, err := database.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(node)
	master.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true})
	master.Create(&model.Node{Name: "B", ApiUrl: "http://b", Enabled: true})
	master.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})
	master.Create(&model.Line{Name: "jp", Protocol: "anytls", Port: 30444, Enabled: true})
	rs := model.Reseller{Name: "r", Enabled: true}
	master.Create(&rs)
	master.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 1})
	master.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 2})
	u := model.User{Name: "ru", Enabled: true, ResellerId: rs.Id}
	master.Create(&u)
	master.Create(&model.UserLine{UserId: u.Id, LineId: 1})
	master.Create(&model.UserLine{UserId: u.Id, LineId: 2})

	apply := func(snap Snapshot) {
		t.Helper()
		snap.SelfNodeId = 2
		if _, _, err := ApplySnapshot(node, snap); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := BuildSnapshot(master, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.UserLines) != 2 || len(snap.ResellerLines) != 2 {
		t.Fatalf("授权内的分配与授权表都应下发: %+v %+v", snap.UserLines, snap.ResellerLines)
	}
	apply(snap)

	master.Where("reseller_id = ? AND line_id = ?", rs.Id, 2).Delete(&model.ResellerLine{})
	snap2, _ := BuildSnapshot(master, func(string) string { return "" })
	if snap2.Revision == snap.Revision {
		t.Fatal("收回授权应改变修订号(副机据此热更新)")
	}
	if len(snap2.UserLines) != 1 || snap2.UserLines[0].LineId != 1 {
		t.Fatalf("收回的线路不该再下发: %+v", snap2.UserLines)
	}
	apply(snap2)
	var n int64
	node.Model(&model.UserLine{}).Where("line_id = ?", 2).Count(&n)
	if n != 0 {
		t.Fatal("副机上该用户应已撤下收回的线路")
	}
	node.Model(&model.ResellerLine{}).Count(&n)
	if n != 1 {
		t.Fatalf("副机的授权表应与主机一致,实际 %d 条", n)
	}
	// 旧版主机的快照没有授权表:副机保留本机的
	snap2.ResellerLines, snap2.ResellerLineNodes = nil, nil
	apply(snap2)
	node.Model(&model.ResellerLine{}).Count(&n)
	if n != 1 {
		t.Fatalf("旧版快照不该清掉副机的授权表,实际 %d 条", n)
	}
}
