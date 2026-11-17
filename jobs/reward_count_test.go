package jobs

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 流量奖励数的是真实流量:主机本机记账时,按原始字节累计(服务器倍率是 2 也不乘),面板内部的用户
// (线路作上游的专用凭据)不算 —— 那份流量在入口那台已经按真正的用户记过一次。副机不在本机累计(主机并账时再加)。
func TestRewardCountsRawBytesOnMaster(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "reward.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.User{Name: "alice", Enabled: true})
	db.Create(&model.User{Name: "bob", Enabled: true})
	tr := core.NewStatsTracker()
	feed := func() {
		tr.RestoreStats([]model.Stats{
			{Resource: "user", Tag: "alice", Direction: false, Traffic: 1000},
			{Resource: "user", Tag: "alice", Direction: true, Traffic: 100},
			{Resource: "user", Tag: "bob" + model.ShareSuffix, Direction: false, Traffic: 40}, // 共享订阅的那一份也是真人的流量
			{Resource: "user", Tag: model.RelayUserName(3), Direction: false, Traffic: 500},   // 面板内部:线路作上游
			{Resource: "inbound", Tag: "in", Direction: false, Traffic: 1540},
		})
	}
	s := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(string) string { return "" }, LocalRatio: func() float64 { return 2 }})
	for round := int64(1); round <= 2; round++ {
		feed()
		if !s.runStatsTracker(tr) {
			t.Fatalf("第 %d 轮落库失败", round)
		}
		if got := model.RewardBytes(db); got != 1140*round {
			t.Fatalf("第 %d 轮后真实流量应为 %d(不乘倍率、不含内部用户),实际 %d", round, 1140*round, got)
		}
	}

	// 副机:只记本机账本,不动奖励计数
	ndb, err := database.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(ndb)
	n := New(Deps{DB: ndb, IsNode: func() bool { return true }, Setting: func(string) string { return "" }})
	feed()
	if !n.runStatsTracker(tr) {
		t.Fatal("副机落库失败")
	}
	if got := model.RewardBytes(ndb); got != 0 {
		t.Fatalf("副机不在本机累计奖励流量,实际 %d", got)
	}
}

func TestRewardCounterOnlyGrows(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "raise.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if model.RewardBytes(db) != 0 {
		t.Fatal("没有记录时是 0")
	}
	_ = model.AddRewardBytes(db, 0, 1)
	_ = model.AddRewardBytes(db, -5, 1)
	_ = model.AddRewardBytes(db, 300, 1)
	_ = model.AddRewardBytes(db, 700, 2)
	if got := model.RewardBytes(db); got != 1000 {
		t.Fatalf("累计: %d", got)
	}
	_ = model.RaiseRewardBytes(db, 400, 3) // 比手上的小:不动
	if got := model.RewardBytes(db); got != 1000 {
		t.Fatalf("不能倒退: %d", got)
	}
	_ = model.RaiseRewardBytes(db, 5000, 4) // 登记处记着的更大(本机是接任 / 还原回来的):往上接
	_ = model.AddRewardBytes(db, 1, 5)
	if got := model.RewardBytes(db); got != 5001 {
		t.Fatalf("往上接之后继续累计: %d", got)
	}
}
