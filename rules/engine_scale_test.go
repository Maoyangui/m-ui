package rules

import (
	"fmt"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 全天时段规则选「全部用户」(审计 I4):窗口打开那一刻以前一条 INSERT 写入所有目标用户的状态,每行 10 个变量,
// 超过 3276 个用户就超出 SQLite 一条语句 32766 个变量的上限,整笔失败 —— 规则永远不生效,也不会随快照下发到副机。
// 停用规则时这些状态要一起撤掉。
func TestScheduleRuleAllUsersManyUsers(t *testing.T) {
	const n = 5000
	e, db := newEngine(t)
	us := make([]model.User, 0, n)
	for i := 0; i < n; i++ {
		us = append(us, model.User{Name: fmt.Sprintf("u%05d", i), Enabled: true})
	}
	if err := db.CreateInBatches(&us, 500).Error; err != nil {
		t.Fatal(err)
	}
	rule := model.Rule{Name: "全天限速", Enabled: true, Kind: KindSchedule, AllUsers: true, Start: "00:00", End: "00:00", DownMbps: 10}
	if err := db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	changed, err := e.Tick(time.Now())
	if err != nil || !changed {
		t.Fatalf("窗口打开应写入全部用户的限速状态: changed=%v err=%v", changed, err)
	}
	var cnt int64
	db.Model(&model.LimitState{}).Count(&cnt)
	if cnt != n {
		t.Fatalf("应有 %d 条限速状态,实际 %d", n, cnt)
	}
	db.Model(&model.Rule{}).Where("id = ?", rule.Id).Update("enabled", false)
	if changed, err := e.Tick(time.Now()); err != nil || !changed {
		t.Fatalf("停用规则应撤掉全部状态: changed=%v err=%v", changed, err)
	}
	db.Model(&model.LimitState{}).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("停用规则后状态应清空,还剩 %d 条", cnt)
	}
}
