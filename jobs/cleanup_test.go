package jobs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 日志清理:流量时序、订阅访问日志、审计日志各按各的设置清,互不牵连。
// 关键点是"默认不改变老行为":没设过的时候,订阅日志跟随流量记录保留,审计日志一条都不删。
func TestCleanupRetention(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)

	now := time.Now()
	day := func(n int) int64 { return now.AddDate(0, 0, -n).Unix() }
	seed := func() {
		db.Exec("DELETE FROM sub_logs")
		db.Exec("DELETE FROM changes")
		db.Exec("DELETE FROM stats")
		for _, d := range []int{0, 2, 10, 45} {
			db.Create(&model.SubLog{Ts: day(d), User: "u"})
			db.Create(&model.Change{DateTime: day(d), Actor: "admin", Key: "user", Action: "update"})
			db.Create(&model.Stats{DateTime: day(d), Resource: "user", Tag: "u"})
		}
	}
	count := func(tbl interface{}) int64 {
		var n int64
		db.Model(tbl).Count(&n)
		return n
	}
	run := func(settings map[string]string) {
		s := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(k string) string { return settings[k] }})
		s.runCleanup()
	}

	// 1) 什么都没设:订阅日志跟随 trafficAge 的默认 30 天,审计一条不删
	seed()
	run(map[string]string{})
	if n := count(&model.SubLog{}); n != 3 {
		t.Fatalf("默认应按 30 天清订阅日志(留 3 条),实际 %d", n)
	}
	if n := count(&model.Change{}); n != 4 {
		t.Fatalf("审计日志默认不该被清,实际剩 %d", n)
	}
	if n := count(&model.Stats{}); n != 3 {
		t.Fatalf("流量时序应按 30 天清,实际 %d", n)
	}

	// 2) 订阅日志单独设 1 天:只影响它,流量时序照旧
	seed()
	run(map[string]string{"subLogAge": "1"})
	if n := count(&model.SubLog{}); n != 1 {
		t.Fatalf("订阅日志设 1 天应只剩今天那条,实际 %d", n)
	}
	if n := count(&model.Stats{}); n != 3 {
		t.Fatalf("订阅日志的设置不该动到流量时序,实际 %d", n)
	}

	// 3) 订阅日志设 0 = 不自动清理
	seed()
	run(map[string]string{"subLogAge": "0"})
	if n := count(&model.SubLog{}); n != 4 {
		t.Fatalf("设 0 应一条不删,实际剩 %d", n)
	}

	// 4) 审计日志设 7 天
	seed()
	run(map[string]string{"auditAge": "7"})
	if n := count(&model.Change{}); n != 2 {
		t.Fatalf("审计设 7 天应剩 2 条,实际 %d", n)
	}

	// 5) 流量记录设 0(不记录流量)时,订阅与审计的清理仍要照常工作
	seed()
	run(map[string]string{"trafficAge": "0", "subLogAge": "1", "auditAge": "1"})
	if n := count(&model.SubLog{}); n != 1 {
		t.Fatalf("trafficAge=0 时订阅日志仍要按自己的设置清,实际 %d", n)
	}
	if n := count(&model.Change{}); n != 1 {
		t.Fatalf("trafficAge=0 时审计日志仍要按自己的设置清,实际 %d", n)
	}
	if n := count(&model.Stats{}); n != 4 {
		t.Fatalf("trafficAge=0 表示不清理流量时序,实际剩 %d", n)
	}
}

// 套餐记录(兼幂等键)固定保留一年,和审计日志的设置无关;总量超过上限时裁掉最旧的。
func TestCleanupPlanOps(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	now := time.Now()
	for _, d := range []int{400, 366, 10, 0} {
		db.Create(&model.PlanOp{UserId: 1, Op: "renew", CreatedAt: now.AddDate(0, 0, -d).Unix()})
	}
	count := func() int64 {
		var n int64
		db.Model(&model.PlanOp{}).Count(&n)
		return n
	}
	s := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(k string) string { return map[string]string{"auditAge": "1"}[k] }})
	s.runCleanup()
	if n := count(); n != 2 {
		t.Fatalf("应删掉一年以前的 2 条、留 2 条,实际剩 %d", n)
	}
	// 总量上限:多出来的最旧几条被裁掉
	extra := int64(model.PlanOpMaxRows + 3)
	if err := db.Exec(`INSERT INTO plan_ops (user_id, op, periods, created_at)
		WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < ?)
		SELECT x, 'renew', 1, ? FROM c`, extra, now.Unix()).Error; err != nil {
		t.Fatal(err)
	}
	s.runCleanup()
	if n := count(); n != model.PlanOpMaxRows {
		t.Fatalf("裁到 %d 条,实际 %d", model.PlanOpMaxRows, n)
	}
	var oldest int64
	db.Model(&model.PlanOp{}).Select("MIN(created_at)").Scan(&oldest)
	if oldest != now.Unix() {
		t.Fatal("裁掉的应是最旧的那几条")
	}
}

// 条数上限按作用域(主面板 / 每个代理)各算各的:一个代理刷出再多的记录,也只裁他自己最旧的,
// 主面板和别的代理的记录与幂等键都还在(以前按全表裁,代理刷一轮就能把别人一年内的幂等键挤掉,重试又加一次时)。
func TestCleanupPlanOpsPerScope(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	now := time.Now()
	key := func(s string) *string { return &s }
	old := now.AddDate(0, 0, -30).Unix()
	db.Create(&model.PlanOp{ResellerId: 0, RequestId: key("order-1"), UserId: 1, Op: "renew", CreatedAt: old})
	db.Create(&model.PlanOp{ResellerId: 0, UserId: 1, Op: "stack", CreatedAt: old})
	db.Create(&model.PlanOp{ResellerId: 8, RequestId: key("order-1"), UserId: 2, Op: "renew", CreatedAt: old})
	// 代理 7 刷了上限 + 3 条,都比上面的新
	if err := db.Exec(`INSERT INTO plan_ops (reseller_id, user_id, op, periods, created_at)
		WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < ?)
		SELECT 7, 3, 'renew', 1, ? + x FROM c`, int64(model.PlanOpMaxRows+3), now.Unix()-int64(model.PlanOpMaxRows+10)).Error; err != nil {
		t.Fatal(err)
	}
	s := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(string) string { return "" }})
	s.runCleanup()
	count := func(rid uint) int64 {
		var n int64
		db.Model(&model.PlanOp{}).Where("reseller_id = ?", rid).Count(&n)
		return n
	}
	if n0, n8, n7 := count(0), count(8), count(7); n0 != 2 || n8 != 1 || n7 != model.PlanOpMaxRows {
		t.Fatalf("各作用域各自封顶:主面板 %d(应 2)、代理 8 %d(应 1)、代理 7 %d(应 %d)", n0, n8, n7, model.PlanOpMaxRows)
	}
	var keys int64
	db.Model(&model.PlanOp{}).Where("request_id = ?", "order-1").Count(&keys)
	if keys != 2 {
		t.Fatalf("主面板与代理 8 的幂等键不该被代理 7 挤掉: %d", keys)
	}
	var oldest int64
	db.Model(&model.PlanOp{}).Where("reseller_id = ?", 7).Select("MIN(created_at)").Scan(&oldest)
	if oldest != now.Unix()-int64(model.PlanOpMaxRows+10)+4 {
		t.Fatalf("代理 7 裁掉的应是他自己最旧的 3 条: %d", oldest)
	}
}
