package database

import "testing"

// 新的套餐操作沿用 plan:<动作>:<套餐名> 的审计写法,老库补「所属套餐」时照样认得出套餐名。
func TestPlanNameInActionNewOps(t *testing.T) {
	for _, op := range []string{"renew", "extend", "replace", "stack", "prorate"} {
		if got := planNameInAction("plan:" + op + ":高级月付"); got != "高级月付" {
			t.Errorf("plan:%s:高级月付 → %q", op, got)
		}
	}
	if got := planNameInAction("plan:stack:带:冒号"); got != "带:冒号" {
		t.Errorf("套餐名里有冒号: %q", got)
	}
}
