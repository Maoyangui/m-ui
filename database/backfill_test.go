package database

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 老用户补归属:操作记录优先,其次额度与限速完全一样且只有一个套餐对得上;猜不准的留空,只做一次。
func TestBackfillUserPlans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	monthly := model.Plan{Name: "月付", VolumeGB: 100, DeviceLimit: 3}
	yearly := model.Plan{Name: "年付", VolumeGB: 200}
	twinA := model.Plan{Name: "双胞胎A", VolumeGB: 50}
	twinB := model.Plan{Name: "双胞胎B", VolumeGB: 50}
	rsPlan := model.Plan{Name: "代理套餐", ResellerId: 9, VolumeGB: 100, DeviceLimit: 3}
	open := model.Plan{Name: "什么都不限"}
	for _, p := range []*model.Plan{&monthly, &yearly, &twinA, &twinB, &rsPlan, &open} {
		if err := db.Create(p).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk := func(name string, resellerID uint, gb float64, dev int) uint {
		u := model.User{Name: name, Enabled: true, ResellerId: resellerID, Volume: model.GBToBytes(gb), DeviceLimit: dev}
		if err := db.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
		return u.Id
	}
	byLog := mk("按记录", 0, 7, 0)          // 额度已被改过,对不上任何套餐,但操作记录里有
	renamed := mk("被顶替", 0, 100, 3)        // 额度对得上月付,但操作记录说最后套的是年付,以记录为准
	byLimit := mk("按额度", 0, 100, 3)        // 只有月付对得上
	twin := mk("对不准", 0, 50, 0)            // 两个套餐都对得上,猜不准
	none := mk("没有对得上的", 0, 1, 0)         // 一个都对不上
	unlimited := mk("不限量", 0, 0, 0)        // 什么限制都没有的套餐不认
	rsUser := mk("代理的用户", 9, 100, 3)      // 只看同一归属的套餐
	bound := mk("已归入", 0, 200, 0)          // 已经有归属的不动
	db.Model(&model.User{}).Where("id = ?", bound).Update("plan_id", twinA.Id)
	for _, c := range []model.Change{
		{Key: "user", Action: "plan:renew:月付", Obj: []byte(`"按记录"`)},
		{Key: "user", Action: "plan:renew:月付", Obj: []byte(`"被顶替"`)},
		{Key: "user", Action: "update:plan:年付", Obj: []byte(`"被顶替"`)},
		{Key: "user", Action: "create", Obj: []byte(`"没有对得上的"`)},
		{Key: "line", Action: "plan:renew:月付", Obj: []byte(`"没有对得上的"`)}, // 不是用户的记录
	} {
		if err := db.Create(&c).Error; err != nil {
			t.Fatal(err)
		}
	}
	db.Exec("DELETE FROM settings WHERE key = ?", planBackfillKey)
	Close(db)

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint]uint{byLog: monthly.Id, renamed: yearly.Id, byLimit: monthly.Id, twin: 0, none: 0, unlimited: 0, rsUser: rsPlan.Id, bound: twinA.Id}
	for id, plan := range want {
		var u model.User
		db.First(&u, id)
		if u.PlanId != plan {
			t.Errorf("用户 %s 应归入 %d,实际 %d", u.Name, plan, u.PlanId)
		}
	}
	// 只做一次:管理员之后手动移出的,下次启动不能又被补回去
	db.Model(&model.User{}).Where("id = ?", byLimit).Update("plan_id", 0)
	Close(db)
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer Close(db)
	var u model.User
	db.First(&u, byLimit)
	if u.PlanId != 0 {
		t.Fatalf("补归属只该做一次,又补回去了: %d", u.PlanId)
	}
}
