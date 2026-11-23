package runner

import (
	"testing"

	"github.com/Maoyangui/m-ui/core"
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
		{UserName: "a", UpMbps: 80, TightenOnly: true},   // 不超过用户自己的限速:不能放宽到 80,还是 50
		{UserName: "b", UpMbps: 10, DownMbps: 10},        // 不限的用户被规则限住
	}
	specs, groups := limitSpecs(users, resellers, states, nil)
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

// 规则忽略了某条线路:那条线路上只算管得着它的状态,单列在 Lines 里(按线路名);和别处一样的不列,已经删掉的线路不列。
func TestLimitSpecsSkipLines(t *testing.T) {
	users := []model.User{
		{Id: 1, Name: "a", SpeedDown: 100},
		{Id: 2, Name: "b"},
		{Id: 3, Name: "c", SpeedDown: 10},
	}
	states := []model.LimitState{
		{UserName: "a", DownMbps: 5, TightenOnly: true, SkipLineIds: []byte(`[1,9]`)}, // 9 号线路已经删了
		{UserName: "a", DownMbps: 20, TightenOnly: true, SkipLineIds: []byte(`[2]`)},
		{UserName: "b", DownMbps: 5, SkipLineIds: []byte(`[1]`)},
		{UserName: "c", DownMbps: 50, SkipLineIds: []byte(`[1]`)}, // 覆盖(放宽)的规则:忽略的线路上回到用户自己的 10
	}
	names := map[uint]string{1: "便宜", 2: "正常"}
	specs, _ := limitSpecs(users, nil, states, names)
	a := specs["a"]
	if _, listed := a.Lines["正常"]; a.DownMbps != 5 || len(a.Lines) != 1 || a.Lines["便宜"].DownMbps != 20 || listed {
		t.Fatalf("a:别处取最严 5;便宜线路只剩第二条 20;正常线路只剩第一条 5,和别处一样就不单列: %+v", a)
	}
	b := specs["b"]
	if b.DownMbps != 5 || len(b.Lines) != 1 || b.Lines["便宜"] != (core.LineLimit{}) {
		t.Fatalf("b:别处 5,便宜线路上不限(单列一个 0): %+v", b)
	}
	c := specs["c"]
	if c.DownMbps != 50 || c.Lines["便宜"].DownMbps != 10 {
		t.Fatalf("c:放宽的规则在忽略的线路上不生效,回到用户自己的 10: %+v", c)
	}
}
