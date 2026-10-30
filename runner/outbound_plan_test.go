package runner

import (
	"reflect"
	"testing"
)

func TestPlanOutbounds(t *testing.T) {
	const (
		a  = `{"type":"socks","tag":"a","server_port":1}`
		a2 = `{"type":"socks","tag":"a","server_port":9}`
		b  = `{"type":"socks","tag":"b","server_port":2}`
		c  = `{"type":"socks","tag":"c","server_port":3}`
		g  = `{"type":"urltest","tag":"g","outbounds":["a","b"]}`
		g2 = `{"type":"urltest","tag":"g","outbounds":["a","c"]}`
		d  = `{"type":"direct","tag":"direct"}`
	)
	cases := []struct {
		name          string
		applied, want map[string]string
		plan          outboundPlan
	}{
		{"没变化", map[string]string{"direct": d, "a": a}, map[string]string{"direct": d, "a": a}, outboundPlan{}},
		{"普通上游增删改:逐个换,没有组的事",
			map[string]string{"direct": d, "a": a, "b": b}, map[string]string{"direct": d, "a": a2, "c": c},
			outboundPlan{swap: []string{"a", "b", "c"}}},
		{"成员改了:内容没变的组也要重建,先摘后装",
			map[string]string{"a": a, "b": b, "g": g}, map[string]string{"a": a2, "b": b, "g": g},
			outboundPlan{dropGroups: []string{"g"}, swap: []string{"a"}, addGroups: []string{"g"}}},
		{"没被组引用的上游改了:组不动",
			map[string]string{"a": a, "b": b, "c": c, "g": g}, map[string]string{"a": a, "b": b, "c": `{"type":"socks","tag":"c","server_port":8}`, "g": g},
			outboundPlan{swap: []string{"c"}}},
		{"组换成员:新成员在装组之前到位,旧成员在摘组之后摘",
			map[string]string{"a": a, "b": b, "g": g}, map[string]string{"a": a, "c": c, "g": g2},
			outboundPlan{dropGroups: []string{"g"}, swap: []string{"b", "c"}, addGroups: []string{"g"}}},
		{"组删了", map[string]string{"a": a, "b": b, "g": g}, map[string]string{"a": a, "b": b},
			outboundPlan{dropGroups: []string{"g"}}},
		{"新建组", map[string]string{"a": a, "b": b}, map[string]string{"a": a, "b": b, "g": g},
			outboundPlan{addGroups: []string{"g"}}},
		{"普通上游改成组:旧的先摘",
			map[string]string{"a": a, "b": b, "c": c}, map[string]string{"a": a, "b": b, "c": `{"type":"urltest","tag":"c","outbounds":["a","b"]}`},
			outboundPlan{dropGroups: []string{"c"}, addGroups: []string{"c"}}},
		{"组改成普通上游:摘组后再装",
			map[string]string{"a": a, "b": b, "g": g}, map[string]string{"a": a, "b": b, "g": `{"type":"socks","tag":"g","server_port":7}`},
			outboundPlan{dropGroups: []string{"g"}, swap: []string{"g"}}},
	}
	for _, c := range cases {
		got := planOutbounds(c.applied, c.want)
		if !reflect.DeepEqual(got, c.plan) {
			t.Errorf("%s:\n要 %+v\n得 %+v", c.name, c.plan, got)
		}
	}
}
