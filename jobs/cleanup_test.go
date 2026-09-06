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
