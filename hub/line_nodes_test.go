package hub

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 用户的服务器范围随快照同步到副机:主机收窄了,副机库里也要有同样的行;主机放开了,副机也跟着清掉。
func TestSnapshotCarriesUserLineNodes(t *testing.T) {
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
	master.Create(&model.User{Name: "u", Enabled: true})
	master.Create(&model.UserLine{UserId: 1, LineId: 1})
	master.Create(&model.UserLineNode{UserId: 1, LineId: 1, NodeId: 2})

	snap, err := BuildSnapshot(master, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.UserLineNodes) != 1 || snap.UserLineNodes[0].NodeId != 2 {
		t.Fatalf("快照应带上服务器范围: %+v", snap.UserLineNodes)
	}
	snap.SelfNodeId = 2
	if _, _, err := ApplySnapshot(node, snap); err != nil {
		t.Fatal(err)
	}
	var rows []model.UserLineNode
	node.Find(&rows)
	if len(rows) != 1 || rows[0].NodeId != 2 {
		t.Fatalf("副机库里应有同样的范围行: %+v", rows)
	}
	// 主机放开范围 → 修订号变化 → 副机的行清掉
	master.Where("user_id = ?", 1).Delete(&model.UserLineNode{})
	snap2, _ := BuildSnapshot(master, func(string) string { return "" })
	if snap2.Revision == snap.Revision {
		t.Fatal("范围变化应改变修订号")
	}
	snap2.SelfNodeId = 2
	if _, _, err := ApplySnapshot(node, snap2); err != nil {
		t.Fatal(err)
	}
	node.Find(&rows)
	if len(rows) != 0 {
		t.Fatalf("放开后副机的范围行应清掉: %+v", rows)
	}
}
