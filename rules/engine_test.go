package rules

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

const gb = int64(1 << 30)

func newEngine(t *testing.T) (*Engine, *gorm.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	return &Engine{DB: db, Location: func() *time.Location { return time.UTC }}, db
}

func statesOf(t *testing.T, db *gorm.DB) []model.LimitState {
	t.Helper()
	var st []model.LimitState
	db.Order("id asc").Find(&st)
	return st
}

func setUsage(db *gorm.DB, id uint, down int64) {
	db.Model(&model.User{}).Where("id = ?", id).Update("down", down)
}

// 突发:10 分钟 1 GB → 20 分钟 20 Mbps;到期自动解除;解除后再跑到阈值再次触发;用量清零不误判。
func TestBurstTriggerExpireRetrigger(t *testing.T) {
	e, db := newEngine(t)
	db.Create(&model.User{Name: "alice", Enabled: true})
	db.Create(&model.User{Name: "bob", Enabled: true})
	db.Create(&model.Rule{Name: "burst", Enabled: true, Kind: KindBurst, UserIds: []byte(`[1]`),
		WindowMin: 10, ThresholdBytes: gb, PenaltyMin: 20, UpMbps: 20, DownMbps: 20, TightenOnly: true})
	var notes []string
	e.Notify = func(s string) { notes = append(notes, s) }
	t0 := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	tick := func(sec int64) bool {
		changed, err := e.Tick(t0.Add(time.Duration(sec) * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if tick(0) {
		t.Fatal("没用量不该触发")
	}
	setUsage(db, 1, gb+gb/5) // 1.2 GB
	setUsage(db, 2, 5*gb)    // bob 不在规则里
	if !tick(60) {
		t.Fatal("10 分钟内 1.2 GB 应触发")
	}
	st := statesOf(t, db)
	if len(st) != 1 || st[0].UserName != "alice" || st[0].RuleName != "burst" || st[0].DownMbps != 20 || !st[0].TightenOnly {
		t.Fatalf("状态不对: %+v", st)
	}
	if st[0].Until != t0.Unix()+60+20*60 {
		t.Fatalf("到期时间应是触发后 20 分钟: %d", st[0].Until)
	}
	if len(notes) != 1 {
		t.Fatalf("应通知一次: %v", notes)
	}
	setUsage(db, 1, 3*gb)
	if tick(120) {
		t.Fatal("惩罚期内继续跑不该再写状态")
	}
	// 到期解除
	if !tick(60 + 20*60 + 1) {
		t.Fatal("到期应解除")
	}
	if len(statesOf(t, db)) != 0 {
		t.Fatal("到期后状态应删除")
	}
	// 解除之后再跑 1.5 GB → 再次触发(窗口从解除那一刻重新起算)
	setUsage(db, 1, 3*gb+gb+gb/2)
	if !tick(60 + 20*60 + 30) {
		t.Fatal("再次达到阈值应再次触发")
	}
	if len(statesOf(t, db)) != 1 || len(notes) != 2 {
		t.Fatalf("应有第二次状态与通知: %d %v", len(statesOf(t, db)), notes)
	}
	// 用量清零(周期重置)后从 0 涨到 0.5 GB:不能因为"倒退"误判,也没到阈值
	db.Where("1 = 1").Delete(&model.LimitState{})
	e.windows = nil
	setUsage(db, 1, 4*gb)
	tick(5000)
	setUsage(db, 1, 0)
	tick(5010)
	setUsage(db, 1, gb/2)
	if tick(5020) {
		t.Fatal("清零后只跑了 0.5 GB 不该触发")
	}
	var audits int64
	db.Model(&model.Change{}).Where("key = ? AND action = ?", "rule", "apply").Count(&audits)
	if audits != 2 {
		t.Fatalf("应有两条 apply 审计: %d", audits)
	}
}

// 时段:在窗口内写状态,走出窗口删除;规则停用立即解除;停用的用户不参与。
func TestScheduleWindowAndDisable(t *testing.T) {
	e, db := newEngine(t)
	db.Create(&model.User{Name: "a", Enabled: true})
	db.Create(&model.User{Name: "b", Enabled: true, ResellerId: 3})
	db.Create(&model.User{Name: "c", Enabled: true})
	db.Create(&model.Rule{Name: "peak", Enabled: true, Kind: KindSchedule, ResellerIds: []byte(`[3]`), UserIds: []byte(`[1]`),
		Start: "19:00", End: "23:00", UpMbps: 30, DownMbps: 30, TightenOnly: true})
	in := time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)
	if ch, _ := e.Tick(in); !ch {
		t.Fatal("窗口内应写状态")
	}
	st := statesOf(t, db)
	if len(st) != 2 || st[0].Until != 0 {
		t.Fatalf("a 与代理 3 名下的 b 应各一条时段状态: %+v", st)
	}
	if ch, _ := e.Tick(in.Add(time.Minute)); ch {
		t.Fatal("窗口内再判不该有变化")
	}
	// 用户 b 被停用 → 他的状态清掉
	db.Model(&model.User{}).Where("id = ?", 2).Update("enabled", false)
	if ch, _ := e.Tick(in.Add(2 * time.Minute)); !ch || len(statesOf(t, db)) != 1 {
		t.Fatal("停用的用户应移出状态")
	}
	// 改了规则的限值 → 状态跟着更新
	db.Model(&model.Rule{}).Where("id = ?", 1).Update("down_mbps", 10)
	if ch, _ := e.Tick(in.Add(3 * time.Minute)); !ch || statesOf(t, db)[0].DownMbps != 10 {
		t.Fatal("规则改了限值应同步到生效中的状态")
	}
	// 走出窗口
	if ch, _ := e.Tick(time.Date(2026, 9, 11, 23, 0, 0, 0, time.UTC)); !ch || len(statesOf(t, db)) != 0 {
		t.Fatal("出窗口应删除状态")
	}
	// 再进窗口后停用规则
	e.Tick(in.Add(24 * time.Hour))
	if len(statesOf(t, db)) != 1 {
		t.Fatal("第二天再进窗口应再写状态")
	}
	db.Model(&model.Rule{}).Where("id = ?", 1).Update("enabled", false)
	if ch, _ := e.Tick(in.Add(24*time.Hour + time.Minute)); !ch || len(statesOf(t, db)) != 0 {
		t.Fatal("规则停用应立即解除")
	}
	var lifts int64
	db.Model(&model.Change{}).Where("key = ? AND action = ?", "rule", "lift").Count(&lifts)
	if lifts != 3 {
		t.Fatalf("应有三条 lift 审计(b 停用、出窗口、规则停用): %d", lifts)
	}
}

// 同一用户挂两条突发规则:各自独立触发、独立计时,叠加时取最严。
func TestTwoBurstRulesIndependent(t *testing.T) {
	e, db := newEngine(t)
	db.Create(&model.User{Name: "a", Enabled: true, SpeedDown: 100})
	db.Create(&model.Rule{Name: "r1", Enabled: true, Kind: KindBurst, AllUsers: true, WindowMin: 10, ThresholdBytes: gb, PenaltyMin: 20, DownMbps: 20, TightenOnly: true})
	db.Create(&model.Rule{Name: "r2", Enabled: true, Kind: KindBurst, AllUsers: true, WindowMin: 30, ThresholdBytes: 2 * gb, PenaltyMin: 30, DownMbps: 10, TightenOnly: true})
	t0 := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	e.Tick(t0)
	setUsage(db, 1, gb+gb/10)
	e.Tick(t0.Add(time.Minute))
	if st := statesOf(t, db); len(st) != 1 || st[0].RuleName != "r1" {
		t.Fatalf("只有 r1 达到阈值: %+v", st)
	}
	setUsage(db, 1, 2*gb+gb/10)
	e.Tick(t0.Add(2 * time.Minute))
	st := statesOf(t, db)
	if len(st) != 2 {
		t.Fatalf("r2 也应触发: %+v", st)
	}
	if _, down := Effective(0, 100, st); down != 10 {
		t.Fatalf("两条叠加取最严 10: %d", down)
	}
	// r1 到期(20 分钟)后 r2 还在
	e.Tick(t0.Add(22 * time.Minute))
	st = statesOf(t, db)
	if len(st) != 1 || st[0].RuleName != "r2" {
		t.Fatalf("r1 到期解除,r2 仍在: %+v", st)
	}
}
