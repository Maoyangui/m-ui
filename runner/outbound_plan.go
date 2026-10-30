package runner

import (
	"encoding/json"
	"sort"
)

// outboundPlan 热换出站要做的事。
//
// 上游组依赖成员:内核不让摘还被组引用的出站(而且摘失败时已经从表里删了,半路上的状态不干净),
// 组也只认启动那一刻拿到的成员实例,成员换了它手里的还是关掉的旧实例。所以成员一变,引用它的组也得跟着重建,
// 先摘组、再换成员、最后装组。没有上游组时就是原来那样逐个换。
type outboundPlan struct {
	dropGroups []string // 先摘:现有的、要动的组
	swap       []string // 再换:普通出站,有旧的摘旧的,有新的装新的
	addGroups  []string // 最后装:要有的组
}

func planOutbounds(applied, want map[string]string) outboundPlan {
	changed := map[string]bool{}
	for tag, ob := range want {
		if prev, existed := applied[tag]; !existed || prev != ob {
			changed[tag] = true
		}
	}
	for tag := range applied {
		if _, keep := want[tag]; !keep {
			changed[tag] = true
		}
	}
	for tag, ob := range applied { // 内容没变、但成员要换的组
		for _, m := range outboundMembers(ob) {
			if changed[m] {
				changed[tag] = true
				break
			}
		}
	}
	var plan outboundPlan
	for tag := range changed {
		prev, existed := applied[tag]
		next, wanted := want[tag]
		wasGroup := existed && len(outboundMembers(prev)) > 0
		isGroup := wanted && len(outboundMembers(next)) > 0
		if wasGroup {
			plan.dropGroups = append(plan.dropGroups, tag)
		}
		if isGroup {
			plan.addGroups = append(plan.addGroups, tag)
		}
		if !wasGroup && !isGroup {
			plan.swap = append(plan.swap, tag)
		} else if !wasGroup && existed { // 普通出站改成了组:旧的还要摘
			plan.dropGroups = append(plan.dropGroups, tag)
		} else if !isGroup && wanted { // 组改成了普通出站:新的放到组都摘完之后装
			plan.swap = append(plan.swap, tag)
		}
	}
	sort.Strings(plan.dropGroups)
	sort.Strings(plan.swap)
	sort.Strings(plan.addGroups)
	return plan
}

// outboundMembers 上游组的成员标签;普通出站返回空。
func outboundMembers(ob string) []string {
	var o struct {
		Outbounds []string `json:"outbounds"`
	}
	_ = json.Unmarshal([]byte(ob), &o)
	return o.Outbounds
}
