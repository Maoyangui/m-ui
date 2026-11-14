package hub

import (
	"testing"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 跨版本混跑与回滚:升级是一台一台做的(主机、副机谁先都有),升级守护自动回滚、手动回滚都保留新库。
// 另一侧照 0.7.16 的真代码模拟 —— 老程序只认游标、账本的 up / down 两列:插入新行时主人、代号等新列取默认值 0,
// 改已有的行时原样不动。

// cursorV0716 0.7.16 的游标表结构。
type cursorV0716 struct {
	NodeId   uint   `gorm:"primaryKey;autoIncrement:false"`
	UserName string `gorm:"primaryKey"`
	Up       int64
	Down     int64
}

func (cursorV0716) TableName() string { return "traffic_cursors" }

// counterV0716 0.7.16 的副机账本表结构。
type counterV0716 struct {
	UserName string `gorm:"primaryKey"`
	Up       int64
	Down     int64
}

func (counterV0716) TableName() string { return "agent_counters" }

// mergeV0716 照 0.7.16 主机的 ApplyCounters 并账:纪元变了只建基线,计数变小一律当回绕从 0 计,游标只写计数。只记用户用量。
func mergeV0716(t *testing.T, db *gorm.DB, nodeId uint, epoch string, counters []model.AgentCounter) {
	t.Helper()
	err := db.Transaction(func(tx *gorm.DB) error {
		baseline := false
		if epoch != "" {
			key := LedgerEpochSetting(nodeId)
			var prev string
			if err := tx.Model(&model.Setting{}).Select("value").Where("key = ?", key).Scan(&prev).Error; err != nil {
				return err
			}
			if prev != epoch {
				baseline = prev != ""
				if !baseline {
					var n int64
					if err := tx.Model(&cursorV0716{}).Where("node_id = ?", nodeId).Count(&n).Error; err != nil {
						return err
					}
					baseline = n == 0
				}
				if err := upsertSetting(tx, key, epoch); err != nil {
					return err
				}
			}
		}
		save := func(c model.AgentCounter) error {
			return tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "node_id"}, {Name: "user_name"}},
				DoUpdates: clause.AssignmentColumns([]string{"up", "down"}),
			}).Create(&cursorV0716{NodeId: nodeId, UserName: c.UserName, Up: c.Up, Down: c.Down}).Error
		}
		for _, c := range counters {
			if baseline {
				if err := save(c); err != nil {
					return err
				}
				continue
			}
			var cur cursorV0716
			if err := tx.Where("node_id = ? AND user_name = ?", nodeId, c.UserName).Limit(1).Find(&cur).Error; err != nil {
				return err
			}
			if c.Up < cur.Up || c.Down < cur.Down {
				cur.Up, cur.Down = 0, 0 // 回绕
			}
			dUp, dDown := c.Up-cur.Up, c.Down-cur.Down
			if dUp <= 0 && dDown <= 0 {
				continue
			}
			if err := tx.Model(&model.User{}).Where("name = ?", c.UserName).
				Updates(map[string]interface{}{"up": gorm.Expr("up + ?", dUp), "down": gorm.Expr("down + ?", dDown)}).Error; err != nil {
				return err
			}
			if err := save(c); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// useV0716 回滚到 0.7.16 的副机记账:已有的行(不管是谁的、哪一本)接着加计数,新插入的行主人、代号都是 0。
func (r *recreateRig) useV0716(name string, n int64) {
	r.t.Helper()
	if err := r.agent.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_name"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"up":   gorm.Expr("agent_counters.up + ?", n),
			"down": gorm.Expr("agent_counters.down + ?", n),
		}),
	}).Create(&counterV0716{UserName: name, Up: n, Down: n}).Error; err != nil {
		r.t.Fatal(err)
	}
}

// reportV0716 0.7.16 副机的报告:只有用户名和计数。
func (r *recreateRig) reportV0716() []model.AgentCounter {
	r.t.Helper()
	var rows []counterV0716
	if err := r.agent.Find(&rows).Error; err != nil {
		r.t.Fatal(err)
	}
	out := make([]model.AgentCounter, len(rows))
	for i, x := range rows {
		out[i] = model.AgentCounter{UserName: x.UserName, Up: x.Up, Down: x.Down}
	}
	return out
}

// ledger 副机现在的整本账(本版副机的报告:带主人和代号)。
func (r *recreateRig) ledger() []model.AgentCounter {
	r.t.Helper()
	var cs []model.AgentCounter
	if err := r.agent.Find(&cs).Error; err != nil {
		r.t.Fatal(err)
	}
	return cs
}

// merge 本版主机并一份报告。
func (r *recreateRig) merge(at int64, epoch string, cs []model.AgentCounter) MergeResult {
	r.t.Helper()
	res, err := MergeCounters(r.host, 2, "n2", epoch, cs, at, 60, 1)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

// useFor 本版副机这一轮记了 name 上下行各 n 字节。
func (r *recreateRig) useFor(name string, n int64) {
	r.t.Helper()
	if err := model.AddAgentCounters(r.agent, []model.AgentCounter{{UserName: name, Up: n, Down: n}}, 100); err != nil {
		r.t.Fatal(err)
	}
}

// usageOf 主机上 name 这个用户现在记了多少(下行)。
func (r *recreateRig) usageOf(name string) int64 {
	r.t.Helper()
	var u model.User
	if err := r.host.Where("name = ?", name).First(&u).Error; err != nil {
		r.t.Fatal(err)
	}
	return u.Down
}

// wipeLedger 副机账本整本清掉(之后同一个人从 0 重开一本)。
func (r *recreateRig) wipeLedger() {
	r.t.Helper()
	if err := r.agent.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.AgentCounter{}).Error; err != nil {
		r.t.Fatal(err)
	}
}

// 先升副机:主机还是 0.7.16 时,副机新开的账本(记了主人和代号)由老主机按计数并,游标只有计数(主人、代号都是 0)。
// 主机升级后的第一份报告是同一本账,只计增量 —— 以前当成"升级后新开的另一本"从 0 计,整本重复计费(应计 1050,实计 2050)。
func TestMixedVersionAgentUpgradedFirst(t *testing.T) {
	r := newRecreateRig(t)
	r.newUser(100)
	r.sync()
	r.use(1000)
	mergeV0716(t, r.host, 2, "", r.ledger())
	if got := r.usageOf("alice"); got != 1000 {
		t.Fatalf("老主机应计 1000,实际 %d", got)
	}
	// 主机升级到本版
	r.use(50)
	res := r.report(1005)
	if got := r.usageOf("alice"); got != 1050 {
		t.Fatalf("主机升级后只计增量,应是 1050,实际 %d(%+v)", got, res)
	}
	if res.Recreated != 0 || res.Reopened != 0 || res.Regressed != 0 || res.Restarted != 0 {
		t.Fatalf("同一本账,不是删了又建、重开或回退: %+v", res)
	}
	// 之后游标记下了主人和代号:账本真的重开时照样从 0 计
	r.wipeLedger()
	r.use(30)
	if res := r.report(1010); res.Reopened != 1 {
		t.Fatalf("游标记下代号后,重开的账本应认得出来: %+v", res)
	}
	if got := r.usageOf("alice"); got != 1080 {
		t.Fatalf("重开的账本从 0 计,应是 1080,实际 %d", got)
	}
}

// 新装的副机(默认装最新版)挂到还是 0.7.16 的主机上:老主机第一份只建基线;主机升级后照常只计增量,
// 不把加入前的基线连同整本账再计一遍(应计 230,以前实计 1430)。
func TestMixedVersionNewAgentJoinsOldHost(t *testing.T) {
	r := newRecreateRig(t)
	r.newUser(100)
	r.sync()
	r.use(1000) // 加入之前账本上就有的
	mergeV0716(t, r.host, 2, "ep1", r.ledger())
	r.use(200)
	mergeV0716(t, r.host, 2, "ep1", r.ledger())
	if got := r.usageOf("alice"); got != 200 {
		t.Fatalf("老主机只计加入后的 200,实际 %d", got)
	}
	r.use(30)
	res := r.merge(1000, "ep1", r.ledger())
	if got := r.usageOf("alice"); got != 230 {
		t.Fatalf("主机升级后只计增量,应是 230,实际 %d(%+v)", got, res)
	}
}

// 主机是本版,副机回滚到 0.7.16(库保留)再升回来:老副机只改计数、行里的主人和代号原样留着,上报不带它们;
// 副机升回来后报出原来的代号,是同一本账,只计增量(以前游标代号被改成 0,当成重开整本重计:应计 1150,实计 2250)。
func TestMixedVersionAgentRolledBack(t *testing.T) {
	r := newRecreateRig(t)
	r.newUser(100)
	r.sync()
	r.use(1000)
	r.report(1000)
	// 副机回滚到 0.7.16
	r.useV0716("alice", 100)
	r.merge(1005, "", r.reportV0716())
	if got := r.usageOf("alice"); got != 1100 {
		t.Fatalf("老副机的报告按增量计,应是 1100,实际 %d", got)
	}
	// 副机升回本版:同一行接着记,报出原来的主人和代号
	r.use(50)
	res := r.report(1010)
	if got := r.usageOf("alice"); got != 1150 {
		t.Fatalf("副机升回来后是同一本账,应是 1150,实际 %d(%+v)", got, res)
	}
	if res.Reopened != 0 || res.Recreated != 0 || res.Regressed != 0 || res.Restarted != 0 {
		t.Fatalf("同一本账,不是重开、删了又建或回退: %+v", res)
	}
	// 游标仍认得这一本:之后真的重开照样从 0 计
	r.wipeLedger()
	r.use(20)
	if res := r.report(1015); res.Reopened != 1 {
		t.Fatalf("重开的账本应认得出来: %+v", res)
	}
	if got := r.usageOf("alice"); got != 1170 {
		t.Fatalf("应是 1170,实际 %d", got)
	}
}

// 副机回滚到 0.7.16 期间第一次有这个人的流量(本版主机此前没有他的游标),副机升回来后报出原来的代号:
// 游标是按老副机的报告写的(不知道代号),只能按计数判断 —— 没变小就是同一本。
func TestMixedVersionAgentRolledBackFirstReport(t *testing.T) {
	r := newRecreateRig(t)
	r.newUser(100)
	r.sync()
	r.use(1000) // 本版副机开的账本,还没来得及报
	r.useV0716("alice", 100)
	r.merge(1000, "", r.reportV0716())
	if got := r.usageOf("alice"); got != 1100 {
		t.Fatalf("应是 1100,实际 %d", got)
	}
	r.use(50)
	res := r.report(1005)
	if got := r.usageOf("alice"); got != 1150 {
		t.Fatalf("同一本账只计增量,应是 1150,实际 %d(%+v)", got, res)
	}
}

// 主机回滚到 0.7.16(库保留)再升回来:回滚期间老主机新插入的游标主人、代号都是 0,已有的只改计数。
// 升回本版后都只计增量(以前回滚期间才有流量的 bob 被整本重计:应计 510,实计 1010)。
func TestMixedVersionHostRolledBack(t *testing.T) {
	r := newRecreateRig(t)
	r.newUser(100)
	if err := r.host.Create(&model.User{Name: "bob", Enabled: true, CreatedAt: 150}).Error; err != nil {
		t.Fatal(err)
	}
	r.sync()
	r.use(1000)
	r.report(1000)
	// 主机回滚到 0.7.16
	r.use(100)
	r.useFor("bob", 500)
	mergeV0716(t, r.host, 2, "", r.ledger())
	// 主机升回本版
	r.use(10)
	r.useFor("bob", 10)
	res := r.report(1005)
	if a, b := r.usageOf("alice"), r.usageOf("bob"); a != 1110 || b != 510 {
		t.Fatalf("升回来后都只计增量,alice 应 1110、bob 应 510,实际 %d、%d(%+v)", a, b, res)
	}
	if res.Recreated != 0 || res.Reopened != 0 || res.Regressed != 0 || res.Restarted != 0 {
		t.Fatalf("都是同一本账: %+v", res)
	}
	// 游标上的代号老主机没动过(同一本账接着记):之后同一本真的回退,照常按账本回退对齐不计
	r.agent.Model(&model.AgentCounter{}).Where("user_name = ?", "bob").Updates(map[string]interface{}{"up": 400, "down": 400})
	if res := r.report(1010); res.Regressed != 1 || r.usageOf("bob") != 510 {
		t.Fatalf("同一本账计数变小是账本回退,bob 应仍是 510,实际 %d(%+v)", r.usageOf("bob"), res)
	}
}

// 主机回滚期间这个人的账本换了一本(改名后又改回 / 删了又建),老主机按计数并了新的那本、游标上的主人和代号还是旧的:
// 升回本版后不能信这些旧记录,不能把老主机已经计过的再从 0 计一遍。
func TestMixedVersionHostRolledBackLedgerChanged(t *testing.T) {
	t.Run("改名后又改回", func(t *testing.T) {
		r := newRecreateRig(t)
		u := r.newUser(100)
		r.sync()
		r.use(1000)
		r.report(1000)
		// 主机回滚到 0.7.16;期间改名又改回,副机那本清掉、同一个人从 0 重开
		r.host.Model(&model.User{}).Where("id = ?", u.Id).Update("name", "bob")
		r.sync()
		r.host.Model(&model.User{}).Where("id = ?", u.Id).Update("name", "alice")
		r.sync()
		r.use(200)
		mergeV0716(t, r.host, 2, "", r.ledger()) // 比游标小:老主机当回绕,从 0 计了 200
		if got := r.usageOf("alice"); got != 1200 {
			t.Fatalf("老主机应计到 1200,实际 %d", got)
		}
		// 主机升回本版
		r.use(50)
		res := r.report(1005)
		if got := r.usageOf("alice"); got != 1250 {
			t.Fatalf("老主机已经计过这一本的 200,只计增量,应是 1250,实际 %d(%+v)", got, res)
		}
	})
	t.Run("删了又建", func(t *testing.T) {
		r := newRecreateRig(t)
		old := r.newUser(100)
		r.sync()
		r.use(1000)
		r.report(1000)
		r.host.Delete(&model.User{}, old.Id)
		r.sync()
		r.newUser(200)
		r.sync()
		r.use(200)
		mergeV0716(t, r.host, 2, "", r.ledger())
		if got := r.usageOf("alice"); got != 200 {
			t.Fatalf("老主机给新 alice 计了 200,实际 %d", got)
		}
		r.use(50)
		res := r.report(1005)
		if got := r.usageOf("alice"); got != 250 {
			t.Fatalf("老主机已经计过新 alice 这一本的 200,只计增量,应是 250,实际 %d(%+v)", got, res)
		}
	})
}

// 副机的库回滚到账本重开之前(虚机快照):报回来的是游标已经换下去的上一本。按账本回退处理(对齐、告警),
// 不当成又重开了一本从 0 再计一遍(以前应计 1300,实计 2300,也不告警)。
func TestAgentLedgerRollbackAcrossReopen(t *testing.T) {
	r := newRecreateRig(t)
	r.newUser(100)
	r.sync()
	r.use(1000)
	r.report(1000)
	snap := r.ledger() // 虚机快照
	r.wipeLedger()     // 之后账本整本清掉、同一个人从 0 重开
	r.use(300)
	if res := r.report(1005); res.Reopened != 1 {
		t.Fatalf("重开的账本从 0 计: %+v", res)
	}
	if got := r.usageOf("alice"); got != 1300 {
		t.Fatalf("应是 1300,实际 %d", got)
	}
	// 副机回滚到快照:账本回到重开之前那一本
	r.wipeLedger()
	if err := r.agent.Create(&snap).Error; err != nil {
		t.Fatal(err)
	}
	res := r.report(1010)
	if got := r.usageOf("alice"); got != 1300 {
		t.Fatalf("回到上一本是账本回退,不该再计,应是 1300,实际 %d(%+v)", got, res)
	}
	if res.Regressed != 1 || res.Reopened != 0 {
		t.Fatalf("应按账本回退处理并告警: %+v", res)
	}
	r.use(50)
	r.report(1015)
	if got := r.usageOf("alice"); got != 1350 {
		t.Fatalf("之后在这一本上照常按增量,应是 1350,实际 %d", got)
	}
}
