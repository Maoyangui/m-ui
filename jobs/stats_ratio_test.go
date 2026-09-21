package jobs

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 主机本机倍率不是 1 时:用量与时序按倍率记,计数器却要按快照原值扣。
// 曾经的错法是就地把快照缩放后再 ConsumeStats:倍率 2 会多扣一倍、计数器变负,下一轮的真实流量先被负数吞掉,
// 用户隔一轮从在线名单消失、倍率在稳态下被抵消成 1。这里跑两轮,账本必须恰好是 2 倍、计数器每轮归零。
func TestStatsRatioDoesNotDriftCounters(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "ratio.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.User{Name: "alice", Enabled: true})
	tr := core.NewStatsTracker()
	feed := func() {
		tr.RestoreStats([]model.Stats{
			{Resource: "user", Tag: "alice", Direction: false, Traffic: 1000},
			{Resource: "user", Tag: "alice", Direction: true, Traffic: 100},
			{Resource: "inbound", Tag: "in", Direction: false, Traffic: 1000},
		})
	}
	s := New(Deps{
		DB:         db,
		IsNode:     func() bool { return false },
		Setting:    func(string) string { return "" },
		LocalRatio: func() float64 { return 2 },
	})
	for round := 1; round <= 2; round++ {
		feed()
		if !s.runStatsTracker(tr) {
			t.Fatalf("第 %d 轮落库失败", round)
		}
		if left := tr.SnapshotStats(); len(*left) != 0 {
			t.Fatalf("第 %d 轮后计数器应恰好归零(不为负、不残留),实际还剩 %+v", round, *left)
		}
		var u model.User
		db.Where("name = ?", "alice").First(&u)
		if u.Down != int64(2000*round) || u.Up != int64(200*round) {
			t.Fatalf("第 %d 轮后用量应按 2 倍记(down=%d up=%d),实际 down=%d up=%d", round, 2000*round, 200*round, u.Down, u.Up)
		}
		users := s.Onlines().Users
		if len(users) != 1 || users[0] != "alice" {
			t.Fatalf("第 %d 轮 alice 应在在线名单里,实际 %v", round, users)
		}
	}
	sum := func(resource, tag string, dir bool) int64 {
		var total int64
		db.Model(&model.Stats{}).Where("resource = ? AND tag = ? AND direction = ?", resource, tag, dir).Select("COALESCE(SUM(traffic),0)").Scan(&total)
		return total
	}
	if got := sum("user", "alice", false); got != 4000 {
		t.Fatalf("用户维度时序应按倍率记 2×2000=4000,实际 %d", got)
	}
	if got := sum("inbound", "in", false); got != 2000 {
		t.Fatalf("线路维度时序保持真实流量 2×1000=2000,实际 %d", got)
	}
}
