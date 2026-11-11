package jobs

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 主机在「设为主机」交接途中(model.SelfLedgerKey 有值):本机流量照常按倍率记进用户用量,同一笔事务里另记一本原始字节的账本,
// 原主机变为副机后接着这本账上报,新主机按整库数据里带过去的游标只计增量。不在交接途中不记。
func TestMasterSelfLedgerDuringHandover(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	alice := model.User{Name: "alice", Enabled: true, CreatedAt: 100}
	db.Create(&alice)
	tr := core.NewStatsTracker()
	feed := func(down int64) {
		tr.RestoreStats([]model.Stats{{Resource: "user", Tag: "alice", Direction: false, Traffic: down}})
	}
	s := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(string) string { return "" },
		LocalRatio: func() float64 { return 2 }})
	ledger := func() (n int64, c model.AgentCounter) {
		db.Model(&model.AgentCounter{}).Count(&n)
		db.Where("user_name = ?", "alice").Limit(1).Find(&c)
		return
	}
	feed(100)
	if !s.runStatsTracker(tr) {
		t.Fatal("落库失败")
	}
	if n, _ := ledger(); n != 0 {
		t.Fatalf("不在交接途中,主机不记账本,实际 %d 本", n)
	}
	db.Create(&model.Setting{Key: model.SelfLedgerKey, Value: "e-self"})
	feed(300)
	if !s.runStatsTracker(tr) {
		t.Fatal("落库失败")
	}
	var u model.User
	db.First(&u, alice.Id)
	n, c := ledger()
	if u.Down != 800 || n != 1 || c.Down != 300 || c.UserId != alice.Id || c.UserCreatedAt != 100 || c.LedgerId == 0 {
		t.Fatalf("交接途中:用量照常按倍率记(应 800,实际 %d),另记一本原始字节的账(应 300):%d 本 %+v", u.Down, n, c)
	}
}
