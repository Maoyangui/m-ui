package jobs

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 代理额度按"名下用户用量之和"判定:超了只在代理行上打 depleted 标记(渲染与订阅据此撤下他的用户),
// 用户行不动;没超的代理与主面板直属用户一律不受影响;补量后标记清掉。
func TestResellerQuotaMarksDepleted(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)

	db.Create(&model.Reseller{Name: "over", Enabled: true, Volume: 10 << 30})
	db.Create(&model.Reseller{Name: "under", Enabled: true, Volume: 10 << 30})
	db.Create(&model.User{Name: "a1", Enabled: true, ResellerId: 1, Up: 6 << 30})
	db.Create(&model.User{Name: "a2", Enabled: true, ResellerId: 1, Down: 5 << 30}) // a1+a2 = 11G > 10G
	db.Create(&model.User{Name: "b1", Enabled: true, ResellerId: 2, Up: 1 << 30})
	db.Create(&model.User{Name: "direct", Enabled: true, Up: 100 << 30})

	reloaded := false
	s := New(Deps{
		DB:          db,
		IsNode:      func() bool { return false },
		ReloadUsers: func() error { reloaded = true; return nil },
	})
	s.runDeplete()

	enabled := func(name string) bool {
		var u model.User
		db.Where("name = ?", name).First(&u)
		return u.Enabled
	}
	depleted := func(id uint) bool {
		var rs model.Reseller
		db.First(&rs, id)
		return rs.Depleted
	}
	if !enabled("a1") || !enabled("a2") {
		t.Fatal("超额代理的用户行不该被改动")
	}
	if !depleted(1) || depleted(2) {
		t.Fatal("超额的代理应标 depleted,未超额的不标")
	}
	if !enabled("b1") || !enabled("direct") {
		t.Fatal("别的代理与直属用户不该受影响")
	}
	if !reloaded {
		t.Fatal("标记变化后应热更新数据面(把他的用户撤下)")
	}
	// 补量:标记清掉
	reloaded = false
	db.Model(&model.Reseller{}).Where("id = ?", 1).Update("volume", 20<<30)
	s.runDeplete()
	if depleted(1) || !reloaded {
		t.Fatal("额度改大后应清掉 depleted 并重载")
	}
}

// 超量停用记原因;周期重置只解禁因超量被停的,手动停用的不复活。
func TestDepleteReasonAndResetOnlyRestoresQuota(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.User{Name: "over", Enabled: true, Volume: 100, Up: 100, AutoReset: true, ResetDays: 30, NextReset: 1})
	db.Create(&model.User{Name: "paused", Enabled: true, Volume: 1000, Up: 1, AutoReset: true, ResetDays: 30, NextReset: 1})
	db.Model(&model.User{}).Where("name = ?", "paused").Updates(map[string]interface{}{"enabled": false, "disabled_reason": model.DisabledManual})
	forgot := map[string]bool{}
	s := New(Deps{DB: db, IsNode: func() bool { return false }, ReloadUsers: func() error { return nil }, Forget: func(k string) { forgot[k] = true }})
	s.runDeplete() // 第一轮:over 的周期重置先到(next_reset=1),清零后不再超量;paused 也重置但仍是手动停用
	var over, paused model.User
	db.Where("name = ?", "over").First(&over)
	db.Where("name = ?", "paused").First(&paused)
	if over.Up != 0 || !over.Enabled {
		t.Fatalf("over 应被周期重置并保持启用: %+v", over)
	}
	if paused.Enabled || paused.DisabledReason != model.DisabledManual {
		t.Fatalf("手动停用的用户周期重置后不该复活: %+v", paused)
	}
	if !forgot["quota:over"] {
		t.Fatal("重置后应清掉用量告警去重键")
	}
	// 再超量:停用并记 quota
	db.Model(&model.User{}).Where("name = ?", "over").Update("up", 100)
	s.runDeplete()
	db.Where("name = ?", "over").First(&over)
	if over.Enabled || over.DisabledReason != model.DisabledQuota {
		t.Fatalf("超量应停用并记原因 quota: %+v", over)
	}
}

// 定时任务里出 panic 不能把进程带走。
func TestRunSafelyRecovers(t *testing.T) {
	done := false
	runSafely("测试", func() { panic("boom") })
	runSafely("测试", func() { done = true })
	if !done {
		t.Fatal("panic 之后后续任务仍应正常执行")
	}
}
