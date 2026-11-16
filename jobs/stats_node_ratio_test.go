package jobs

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func sumSeries(t *testing.T, s *Scheduler, resource, tag string, up bool) int64 {
	t.Helper()
	var total int64
	if err := s.d.DB.Model(&model.Stats{}).Where("resource = ? AND tag = ? AND direction = ?", resource, tag, up).
		Select("COALESCE(SUM(traffic),0)").Scan(&total).Error; err != nil {
		t.Fatal(err)
	}
	return total
}

// 主机本机:节点单独设了倍率的那部分按节点的倍率,其余按本机倍率;时序同一个算法,一个用户一个方向合成一行。
func TestStatsNodeRatioOnMaster(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "nr.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.User{Name: "alice", Enabled: true})
	tr := core.NewStatsTracker()
	s := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(string) string { return "" }, LocalRatio: func() float64 { return 2 }})

	// 普通线路 1000,倍率 3 的节点 400,倍率 0.5 的节点 200(都是下行);上行只有倍率 3 的节点 10
	tr.RestoreStats([]model.Stats{
		{Resource: "user", Tag: "alice", Direction: false, Traffic: 1000},
		{Resource: core.StatUserX, Tag: core.UserXTag("alice", 3), Direction: false, Traffic: 400},
		{Resource: core.StatUserX, Tag: core.UserXTag("alice", 0.5), Direction: false, Traffic: 200},
		{Resource: core.StatUserX, Tag: core.UserXTag("alice", 3), Direction: true, Traffic: 10},
		{Resource: "inbound", Tag: "in", Direction: false, Traffic: 1600},
	})
	if !s.runStatsTracker(tr) {
		t.Fatal("落库失败")
	}
	if left := tr.SnapshotStats(); len(*left) != 0 {
		t.Fatalf("记完账计数器应恰好归零,还剩 %+v", *left)
	}
	var u model.User
	db.Where("name = ?", "alice").First(&u)
	if wantDown, wantUp := int64(1000*2+400*3+100), int64(30); u.Down != wantDown || u.Up != wantUp {
		t.Fatalf("用量应是 下行 %d / 上行 %d,实际 %d / %d", wantDown, wantUp, u.Down, u.Up)
	}
	if got := sumSeries(t, s, "user", "alice", false); got != 3300 {
		t.Fatalf("用户时序应和用量同一个数 3300,实际 %d", got)
	}
	var rows int64
	db.Model(&model.Stats{}).Where("resource = ? AND tag = ? AND direction = ?", "user", "alice", false).Count(&rows)
	if rows != 1 {
		t.Fatalf("同一个用户同一方向同一桶只该有一行,实际 %d 行", rows)
	}
	if got := sumSeries(t, s, "inbound", "in", false); got != 1600 {
		t.Fatalf("线路维度保持真实流量 1600,实际 %d", got)
	}
	var stray int64
	db.Model(&model.Stats{}).Where("resource = ?", core.StatUserX).Count(&stray)
	if stray != 0 {
		t.Fatalf("userx 只是数据面内部的计数,不能落进时序表(%d 行)", stray)
	}
	if users := s.Onlines().Users; len(users) != 1 || users[0] != "alice" {
		t.Fatalf("alice 应在在线名单里且只出现一次:%v", users)
	}

	// 这一轮只有走单独倍率节点的流量(没有 user 行):照样算在线、照样记账
	tr.RestoreStats([]model.Stats{{Resource: core.StatUserX, Tag: core.UserXTag("alice", 3), Direction: false, Traffic: 100}})
	if !s.runStatsTracker(tr) {
		t.Fatal("第二轮落库失败")
	}
	db.Where("name = ?", "alice").First(&u)
	if u.Down != 3300+300 {
		t.Fatalf("第二轮应再记 300,实际累计 %d", u.Down)
	}
}

// 副机:账本记原始字节,另带四个附带数(o = 走了单独倍率节点的原始字节,x = 乘好的);时序也是原始字节。
func TestStatsNodeRatioOnAgent(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "nra.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.User{Name: "alice", Enabled: true})
	tr := core.NewStatsTracker()
	s := New(Deps{DB: db, IsNode: func() bool { return true }, Setting: func(string) string { return "" }})
	for round := 1; round <= 2; round++ {
		tr.RestoreStats([]model.Stats{
			{Resource: "user", Tag: "alice", Direction: false, Traffic: 1000},
			{Resource: core.StatUserX, Tag: core.UserXTag("alice", 3), Direction: false, Traffic: 400},
			{Resource: core.StatUserX, Tag: core.UserXTag("alice", 3), Direction: true, Traffic: 10},
		})
		if !s.runStatsTracker(tr) {
			t.Fatalf("第 %d 轮落库失败", round)
		}
		var c model.AgentCounter
		if err := db.Where("user_name = ?", "alice").First(&c).Error; err != nil {
			t.Fatal(err)
		}
		n := int64(round)
		if c.Down != 1400*n || c.ODown != 400*n || c.XDown != 1200*n || c.Up != 10*n || c.OUp != 10*n || c.XUp != 30*n {
			t.Fatalf("第 %d 轮后账本不对:%+v", round, c)
		}
	}
	if got := sumSeries(t, s, "user", "alice", false); got != 2800 {
		t.Fatalf("副机的时序记原始字节 2×1400,实际 %d", got)
	}
}
