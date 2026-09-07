package hub

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 规则限速状态进快照:变了修订号要变;副机应用快照时整表替换。
func TestSnapshotCarriesLimitStates(t *testing.T) {
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
	setting := func(string) string { return "" }
	master.Create(&model.User{Name: "a", Enabled: true})
	a, _ := BuildSnapshot(master, setting)
	master.Create(&model.LimitState{RuleId: 1, UserId: 1, UserName: "a", RuleName: "r", DownMbps: 20, TightenOnly: true, Since: 1, Until: 9999999999})
	b, _ := BuildSnapshot(master, setting)
	if a.Revision == b.Revision {
		t.Fatal("多了一条限速状态,修订号必须变")
	}
	if _, _, err := ApplySnapshot(node, b); err != nil {
		t.Fatal(err)
	}
	var st []model.LimitState
	node.Find(&st)
	if len(st) != 1 || st[0].UserName != "a" || !st[0].TightenOnly || st[0].DownMbps != 20 {
		t.Fatalf("副机应拿到状态: %+v", st)
	}
	master.Where("1 = 1").Delete(&model.LimitState{})
	c, _ := BuildSnapshot(master, setting)
	if _, _, err := ApplySnapshot(node, c); err != nil {
		t.Fatal(err)
	}
	node.Find(&st)
	if len(st) != 0 {
		t.Fatal("主机解除后副机的状态应被整表替换掉")
	}
}
