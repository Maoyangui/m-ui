package jobs

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

func openStatsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	return db
}

// activeUsers 建 n 个用户,返回他们这一轮各上行 100、下行 200 的计数。
func activeUsers(t *testing.T, db *gorm.DB, n int) []model.Stats {
	t.Helper()
	us := make([]model.User, 0, n)
	st := make([]model.Stats, 0, 2*n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("u%05d", i)
		us = append(us, model.User{Name: name, Enabled: true})
		st = append(st,
			model.Stats{Resource: "user", Tag: name, Direction: true, Traffic: 100},
			model.Stats{Resource: "user", Tag: name, Direction: false, Traffic: 200})
	}
	if err := db.CreateInBatches(&us, 500).Error; err != nil {
		t.Fatal(err)
	}
	return st
}

func sumOf(t *testing.T, db *gorm.DB, table interface{}, expr string, where ...interface{}) int64 {
	t.Helper()
	var v int64
	q := db.Model(table)
	if len(where) > 0 {
		q = q.Where(where[0], where[1:]...)
	}
	if err := q.Select("COALESCE(" + expr + ", 0)").Scan(&v).Error; err != nil {
		t.Fatal(err)
	}
	return v
}

// 一轮统计里有几千个双向都有流量的用户(审计 I4):时序以前一条 INSERT 写完,每行 5 个变量,
// 3277 个用户就超过 SQLite 一条语句 32766 个变量的上限,整笔失败;副机的账本和它在同一个事务里,一起回滚。
// 失败的这批还留在内存里,下一轮只多不少,从此每 10 秒失败一次 —— 副机账本停写,主机再也收不到这台的流量。
// 主机、副机两种模式都要:落库成功、计数器扣干净、账本合计对得上;同一个桶跑两轮,时序按 ON CONFLICT 累加。
func TestStatsManyActiveUsers(t *testing.T) {
	for _, isNode := range []bool{false, true} {
		for _, n := range []int{3277, 5000} {
			isNode, n := isNode, n
			t.Run(fmt.Sprintf("node=%v/users=%d", isNode, n), func(t *testing.T) {
				db := openStatsDB(t)
				st := activeUsers(t, db, n)
				s := New(Deps{DB: db, IsNode: func() bool { return isNode }, Setting: func(k string) string {
					if k == "statsBucketSeconds" {
						return "86400" // 两轮落在同一个桶里
					}
					return ""
				}})
				for round := int64(1); round <= 2; round++ {
					tr := core.NewStatsTracker()
					tr.RestoreStats(st)
					if !s.runStatsTracker(tr) {
						t.Fatalf("第 %d 轮统计落库失败", round)
					}
					if left := tr.SnapshotStats(); len(*left) != 0 {
						t.Fatalf("第 %d 轮后计数器应全部扣掉,还剩 %d 行", round, len(*left))
					}
					var up, down int64
					if isNode {
						up, down = sumOf(t, db, &model.AgentCounter{}, "SUM(up)"), sumOf(t, db, &model.AgentCounter{}, "SUM(down)")
					} else {
						up, down = sumOf(t, db, &model.User{}, "SUM(up)"), sumOf(t, db, &model.User{}, "SUM(down)")
					}
					if up != round*int64(n)*100 || down != round*int64(n)*200 {
						t.Fatalf("第 %d 轮后账本合计应是 %d/%d,实际 %d/%d", round, round*int64(n)*100, round*int64(n)*200, up, down)
					}
					if got := sumOf(t, db, &model.Stats{}, "SUM(traffic)", "resource = ?", "user"); got != round*int64(n)*300 {
						t.Fatalf("第 %d 轮后用户时序合计应是 %d,实际 %d", round, round*int64(n)*300, got)
					}
				}
				var rows int64
				db.Model(&model.Stats{}).Where("resource = ?", "user").Count(&rows)
				if rows > int64(4*n) {
					t.Fatalf("同一个桶两轮应累加进同一行,时序行数 %d 超过 %d", rows, 4*n)
				}
			})
		}
	}
}

// 时序只是图表;账本才是计费。时序写不进去(这里用触发器让 stats 的插入失败)时,
// 副机账本照样要记上、计数器照样扣掉 —— 以前两者在一个事务里,时序一失败账本跟着回滚,主机收不到这台的流量。
func TestLedgerCommittedEvenIfTimeseriesFails(t *testing.T) {
	for _, isNode := range []bool{true, false} {
		isNode := isNode
		t.Run(fmt.Sprintf("node=%v", isNode), func(t *testing.T) {
			db := openStatsDB(t)
			st := activeUsers(t, db, 3)
			if err := db.Exec("CREATE TRIGGER stats_broken BEFORE INSERT ON stats BEGIN SELECT RAISE(ABORT, 'disk full'); END").Error; err != nil {
				t.Fatal(err)
			}
			s := New(Deps{DB: db, IsNode: func() bool { return isNode }, Setting: func(string) string { return "" }})
			tr := core.NewStatsTracker()
			tr.RestoreStats(st)
			if !s.runStatsTracker(tr) {
				t.Fatal("时序写失败不该让这一轮算失败:账本已经记上了")
			}
			if left := tr.SnapshotStats(); len(*left) != 0 {
				t.Fatalf("账本记上了就要把计数器扣掉,否则下一轮重复记账: 还剩 %d 行", len(*left))
			}
			var up int64
			if isNode {
				up = sumOf(t, db, &model.AgentCounter{}, "SUM(up)")
			} else {
				up = sumOf(t, db, &model.User{}, "SUM(up)")
			}
			if up != 300 {
				t.Fatalf("时序失败时账本照样要记上,上行合计应是 300,实际 %d", up)
			}
		})
	}
}

// 记账连续失败要数着(副机随报告告诉主机,服务器页上看得见);失败期间计数器不扣、流量留在内存里;
// 恢复后一次记上,不重复,失败计数清零。
func TestLedgerFailuresCounted(t *testing.T) {
	db := openStatsDB(t)
	st := activeUsers(t, db, 3)
	if err := db.Exec("CREATE TRIGGER ledger_broken BEFORE INSERT ON agent_counters BEGIN SELECT RAISE(ABORT, 'disk full'); END").Error; err != nil {
		t.Fatal(err)
	}
	s := New(Deps{DB: db, IsNode: func() bool { return true }, Setting: func(string) string { return "" }})
	tr := core.NewStatsTracker()
	tr.RestoreStats(st)
	for round := 1; round <= 2; round++ {
		if s.runStatsTracker(tr) {
			t.Fatalf("第 %d 轮账本写不进去,不该算成功", round)
		}
		if got := s.LedgerFailures(); got != round {
			t.Fatalf("第 %d 轮后连续失败应记 %d 次,实际 %d", round, round, got)
		}
	}
	if left := tr.SnapshotStats(); len(*left) != len(st) {
		t.Fatalf("记账失败时计数器不能扣: 还剩 %d 行", len(*left))
	}
	if err := db.Exec("DROP TRIGGER ledger_broken").Error; err != nil {
		t.Fatal(err)
	}
	if !s.runStatsTracker(tr) || s.LedgerFailures() != 0 {
		t.Fatalf("恢复后应记上并清零失败计数: %d", s.LedgerFailures())
	}
	if up := sumOf(t, db, &model.AgentCounter{}, "SUM(up)"); up != 300 {
		t.Fatalf("恢复后一次记上、不重复,上行合计应是 300,实际 %d", up)
	}
}
