package runner

import (
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

func TestLimitSpecsOverlayRuleStates(t *testing.T) {
	users := []model.User{
		{Id: 1, Name: "a", SpeedUp: 50, SpeedDown: 100},
		{Id: 2, Name: "b"},
		{Id: 3, Name: "c", ResellerId: 7, SpeedDown: 30},
	}
	resellers := []model.Reseller{{Id: 7, SpeedDown: 200}}
	states := []model.LimitState{
		{UserName: "a", DownMbps: 20, TightenOnly: true}, // 100 → 20
		{UserName: "a", UpMbps: 80, TightenOnly: true},   // 只升不降:不能放宽到 80,还是 50
		{UserName: "b", UpMbps: 10, DownMbps: 10},        // 不限的用户被规则限住
	}
	specs, groups := limitSpecs(users, resellers, states)
	if a := specs["a"]; a.UpMbps != 50 || a.DownMbps != 20 {
		t.Fatalf("a: %+v", a)
	}
	if b := specs["b"]; b.UpMbps != 10 || b.DownMbps != 10 {
		t.Fatalf("b: %+v", b)
	}
	if c := specs["c"]; c.DownMbps != 30 || c.Group != model.ResellerGroup(7) {
		t.Fatalf("没规则的用户保持自己的限速与代理池: %+v", c)
	}
	if groups[model.ResellerGroup(7)].DownMbps != 200 {
		t.Fatal("代理池不受规则影响")
	}
}
