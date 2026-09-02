package web

import (
	"encoding/json"
	"testing"

	"github.com/fangjunsheng555/m-ui/database/model"
)

func TestApplyPlanRenewExtendsFromExpiryAndResetsTraffic(t *testing.T) {
	now := int64(1_000_000)
	u := model.User{Expiry: now + 5*86400, Up: 10, Down: 20, TotalUp: 1, TotalDown: 2, Enabled: false}
	p := model.Plan{VolumeGB: 100, Days: 30, DeviceLimit: 3, SpeedDown: 50, AutoReset: true, ResetDays: 30}
	applyPlan(&u, p, "renew", now)
	if u.Expiry != now+35*86400 {
		t.Fatalf("续费应从原到期顺延 30 天,实际 %d", u.Expiry)
	}
	if u.Up != 0 || u.Down != 0 || u.TotalUp != 11 || u.TotalDown != 22 {
		t.Fatalf("续费应清零并累计历史: %+v", u)
	}
	if u.Volume != 100<<30 || u.DeviceLimit != 3 || u.SpeedDown != 50 || !u.AutoReset || u.NextReset != now+30*86400 || !u.Enabled {
		t.Fatalf("套餐字段未套用: %+v", u)
	}
}

func TestApplyPlanExtendKeepsTraffic(t *testing.T) {
	now := int64(1_000_000)
	u := model.User{Expiry: now - 100, Up: 10, Down: 20, NextReset: now + 10}
	p := model.Plan{VolumeGB: 50, Days: 7}
	applyPlan(&u, p, "extend", now)
	if u.Expiry != now+7*86400 {
		t.Fatalf("已过期用户延期应从现在起算,实际 %d", u.Expiry)
	}
	if u.Up != 10 || u.Down != 20 {
		t.Fatal("延期不应清零流量")
	}
	if u.NextReset != 0 {
		t.Fatal("套餐不自动重置时应清空 next_reset")
	}
}

func TestApplyPlanNewUnlimitedAndLines(t *testing.T) {
	now := int64(1_000_000)
	ids, _ := json.Marshal([]uint{3, 5})
	u := model.User{Expiry: now + 999}
	p := model.Plan{VolumeGB: 0, Days: 0, LineIds: ids}
	got := applyPlan(&u, p, "new", now)
	if u.Expiry != 0 || u.Volume != 0 {
		t.Fatalf("0 天/0GB 应为不限: %+v", u)
	}
	if len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Fatalf("应返回套餐线路: %v", got)
	}
}
