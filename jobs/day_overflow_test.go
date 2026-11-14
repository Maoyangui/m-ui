package jobs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// SQL 里的时间加法按天数上限饱和:首次连接开始计时(now + activate_days × 86400)、周期重置排下次时间
// (now + reset_days × 86400)。以前天数一大就溢出,SQLite 把结果存成 REAL,之后整表读用户都会失败;
// Go 里算的下次重置时间则会绕成负数,用户从此不再重置或者每分钟都重置。
func TestDayMathStaysInteger(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	const huge = 1 << 50 // 天;× 86400 远超 int64
	db.Create(&model.User{Name: "waiting", Enabled: true, ActivateDays: huge})
	db.Create(&model.User{Name: "fresh", Enabled: true, AutoReset: true, ResetDays: huge}) // next_reset = 0:首次排期
	db.Create(&model.User{Name: "due", Enabled: true, AutoReset: true, ResetDays: huge, NextReset: 1, Up: 5})
	s := New(Deps{DB: db, IsNode: func() bool { return false }, ReloadUsers: func() error { return nil }})
	now := time.Now().Unix()
	if err := s.writeLedger(false, []string{"waiting"}, map[string]*userBytes{"waiting": {up: 1}}, now, func(n int64) int64 { return n }); err != nil {
		t.Fatal(err)
	}
	s.runDeplete()

	for _, col := range []string{"expiry", "next_reset", "activate_days"} {
		var bad int64
		db.Raw("SELECT COUNT(*) FROM users WHERE typeof(" + col + ") <> 'integer'").Scan(&bad)
		if bad > 0 {
			t.Fatalf("%s 有 %d 行不是 INTEGER", col, bad)
		}
	}
	var all []model.User
	if err := db.Find(&all).Error; err != nil {
		t.Fatalf("整表读用户失败: %v", err)
	}
	byName := map[string]model.User{}
	for _, u := range all {
		byName[u.Name] = u
	}
	if u := byName["waiting"]; u.ActivateDays != 0 || u.Expiry <= now {
		t.Fatalf("首次连接后开始计时,到期在以后: %+v", u)
	}
	for _, n := range []string{"fresh", "due"} {
		if u := byName[n]; u.NextReset <= now {
			t.Fatalf("%s 的下次重置应在以后(封顶,不绕成负数): %d", n, u.NextReset)
		}
	}
	if u := byName["due"]; u.Up != 0 || u.TotalUp != 5 {
		t.Fatalf("到点的照常重置: %+v", u)
	}
}
