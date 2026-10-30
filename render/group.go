package render

import (
	"encoding/json"
	"sort"

	"github.com/Maoyangui/m-ui/database/model"
)

// GroupType 上游组:几条上游放在一起,内核定时测,用延迟最低的那条能通的,挂了自动换。
// 就是 sing-box 的 urltest 出站;成员写的是上游名(也就是出站标签),改名时一起改(见 web)。
const GroupType = "urltest"

// IsGroup 这条上游是不是上游组。
func IsGroup(u model.Upstream) bool { return u.Type == GroupType }

// GroupMembers 上游组的成员名。
func GroupMembers(u model.Upstream) []string {
	var o struct {
		Outbounds []string `json:"outbounds"`
	}
	_ = json.Unmarshal(u.Options, &o)
	return o.Outbounds
}

// ExpandGroups used 里的上游组,把成员也算上:线路只写了组,成员的出站同样要渲染、要巡检。
func ExpandGroups(used map[uint]bool, all []model.Upstream) {
	byName := make(map[string]uint, len(all))
	for _, u := range all {
		byName[u.Name] = u.Id
	}
	for _, u := range all {
		if !used[u.Id] || !IsGroup(u) {
			continue
		}
		for _, name := range GroupMembers(u) {
			if id, ok := byName[name]; ok {
				used[id] = true
			}
		}
	}
}

// ExpandIDs 同 ExpandGroups,给只有一串 id 的地方用。
func ExpandIDs(ids []uint, all []model.Upstream) []uint {
	used := make(map[uint]bool, len(ids))
	for _, id := range ids {
		used[id] = true
	}
	ExpandGroups(used, all)
	out := make([]uint, 0, len(used))
	for id := range used {
		out = append(out, id)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}
