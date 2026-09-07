// Package rules 限速规则:时段限速与突发限速。
//
// 规则只在主机判定(和配额、到期一样),命中时给目标用户写一条 LimitState,随快照下发副机;
// 主机与副机计算限速时把生效中的状态叠加到用户自己的限速上,再交给数据面的限速器。
// 用户自己的限速是常态,规则是临时接管:条件消失(走出时段 / 惩罚到期 / 规则停用)就退回常态。
package rules

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

const (
	KindSchedule = "schedule" // 时段限速
	KindBurst    = "burst"    // 突发限速
)

// ParseIDs 解出 json 数组里的 id;解析不了当空。
func ParseIDs(raw json.RawMessage) []uint {
	var ids []uint
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &ids)
	}
	return ids
}

// Targets 此刻被规则覆盖的用户:全部 / 指定用户 / 指定代理名下的全部用户(动态,以后新建的也算)。
// 停用的用户不参与:他连不上,限不限无意义,状态也不该留着。
func Targets(rule model.Rule, users []model.User) []model.User {
	uid := map[uint]bool{}
	for _, id := range ParseIDs(rule.UserIds) {
		uid[id] = true
	}
	rid := map[uint]bool{}
	for _, id := range ParseIDs(rule.ResellerIds) {
		rid[id] = true
	}
	var out []model.User
	for _, u := range users {
		if !u.Enabled {
			continue
		}
		if rule.AllUsers || uid[u.Id] || (u.ResellerId != 0 && rid[u.ResellerId]) {
			out = append(out, u)
		}
	}
	return out
}

// ParseHM "HH:MM" → 当天第几分钟;解析不了返回 -1。
func ParseHM(s string) int {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return -1
	}
	hh, err1 := strconv.Atoi(strings.TrimSpace(h))
	mm, err2 := strconv.Atoi(strings.TrimSpace(m))
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return -1
	}
	return hh*60 + mm
}

// dayOf Go 的星期(周日 = 0)换成 1..7(周一 = 1)。
func dayOf(t time.Time) int {
	if d := int(t.Weekday()); d != 0 {
		return d
	}
	return 7
}

// DaySet "1,2,5" → {1,2,5};空 = 每天(返回空集合,调用方按"不限"处理)。
func DaySet(days string) map[int]bool {
	set := map[int]bool{}
	for _, p := range strings.Split(days, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n >= 1 && n <= 7 {
			set[n] = true
		}
	}
	return set
}

// InWindow 时段规则此刻是否在窗口内(loc = 面板时区)。
// 结束早于开始 = 跨午夜,这段按开始那天的星期算:周五 23:00 到 02:00,周六 01:00 也属于周五那段。
func InWindow(rule model.Rule, now time.Time, loc *time.Location) bool {
	start, end := ParseHM(rule.Start), ParseHM(rule.End)
	if start < 0 || end < 0 {
		return false
	}
	if loc == nil {
		loc = time.Local
	}
	t := now.In(loc)
	cur := t.Hour()*60 + t.Minute()
	days := DaySet(rule.Days)
	on := func(day time.Time) bool { return len(days) == 0 || days[dayOf(day)] }
	switch {
	case start == end:
		return on(t) // 全天
	case end > start:
		return cur >= start && cur < end && on(t)
	case cur >= start:
		return on(t)
	case cur < end:
		return on(t.AddDate(0, 0, -1))
	}
	return false
}

// Effective 用户自己的限速叠加生效中的规则,返回最终上下行 Mbps(0 = 不限)。
// 每条状态贡献一个值:方向上填 0 的不参与;开了"只升不降"的贡献 min(规则值, 用户自己的值);
// 多条同时生效取最小(最严)。没有状态碰这个方向就是用户自己的值。
func Effective(ownUp, ownDown int, states []model.LimitState) (up, down int) {
	return effectiveDir(ownUp, states, func(s model.LimitState) int { return s.UpMbps }),
		effectiveDir(ownDown, states, func(s model.LimitState) int { return s.DownMbps })
}

func effectiveDir(own int, states []model.LimitState, val func(model.LimitState) int) int {
	best, found := 0, false
	for _, st := range states {
		v := val(st)
		if v <= 0 {
			continue
		}
		if st.TightenOnly && own > 0 && own < v {
			v = own
		}
		if !found || v < best {
			best, found = v, true
		}
	}
	if !found {
		return own
	}
	return best
}

// Active 只留此刻仍生效的状态(时段状态 Until 为 0;突发状态到期即失效,副机据此在主机失联时也能自行放开)。
func Active(states []model.LimitState, now int64) []model.LimitState {
	out := states[:0:0]
	for _, st := range states {
		if st.Until == 0 || st.Until > now {
			out = append(out, st)
		}
	}
	return out
}

// HumanBytes 1.2 GB / 300 MB 这样的可读流量。
func HumanBytes(n int64) string {
	const gb, mb = 1 << 30, 1 << 20
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	case n >= mb:
		return fmt.Sprintf("%.0f MB", float64(n)/mb)
	}
	return fmt.Sprintf("%d B", n)
}

// LimitText "上行 20 / 下行 20 Mbps";填 0 的方向写"不改"。
func LimitText(up, down int) string {
	f := func(v int) string {
		if v <= 0 {
			return "不改"
		}
		return strconv.Itoa(v)
	}
	return "上行 " + f(up) + " / 下行 " + f(down) + " Mbps"
}
