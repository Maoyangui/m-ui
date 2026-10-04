package hub

import (
	"encoding/json"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/selfupdate"
)

// 有线路用分流规则才抬副机版本门槛:旧副机会把规则丢掉却照常应用,宁可拒收;没用这个功能时升级主机不影响旧副机同步。
func TestMinNodeForRouteRules(t *testing.T) {
	plain := []model.Line{{Name: "a"}, {Name: "b", RouteRules: json.RawMessage(`[]`)}, {Name: "c", RouteRules: json.RawMessage(`null`)}}
	if got := minNodeFor(plain); got != MinNodeVersion {
		t.Fatalf("没有分流规则时门槛不变: %s", got)
	}
	withRules := append(plain, model.Line{Name: "d", RouteRules: json.RawMessage(`[{"type":"domain","values":["x.com"],"to":0}]`)})
	if got := minNodeFor(withRules); got != RouteRulesMinNode {
		t.Fatalf("有分流规则时门槛要抬到 %s: %s", RouteRulesMinNode, got)
	}
	if !selfupdate.Newer(RouteRulesMinNode, MinNodeVersion) {
		t.Fatal("分流规则的门槛必须高于基础门槛")
	}
	// 线路变了分流规则要算快照变化(副机要重载);存成 null 和空串一样
	a := []model.Line{{Id: 1, Name: "a"}}
	b := []model.Line{{Id: 1, Name: "a", RouteRules: json.RawMessage(`[{"type":"domain","values":["x.com"],"to":0}]`)}}
	if sameLines(a, b) {
		t.Fatal("加了分流规则要算线路变化")
	}
	if !sameLines(a, []model.Line{{Id: 1, Name: "a", RouteRules: json.RawMessage(`null`)}}) {
		t.Fatal("null 与空要视为相同")
	}
}
