package rules

import (
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// LineMeterWanted 要不要记「用户 × 线路」的用量:有启用的突发规则勾了还在的线路。主机每轮统计、构造快照时问它
// (副机按快照里的设置记,见 hub.LineMeterSetting)。时段规则不用:它只按线路换桶位,不算用量。
// 按字符串读:手工改库存成 TEXT 的行照样读得出来(规则判定那边逐行容错,这里也不能一行坏了整个功能失灵)。
func LineMeterWanted(db *gorm.DB) (bool, error) {
	var raws []sql.NullString
	if err := db.Model(&model.Rule{}).Where("enabled = ? AND kind = ?", true, KindBurst).Pluck("skip_line_ids", &raws).Error; err != nil {
		return false, err
	}
	var ids []uint
	for _, r := range raws {
		if r.Valid {
			for id := range SkipSet(json.RawMessage(r.String)) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return false, nil
	}
	var n int64
	if err := db.Model(&model.Line{}).Where("id IN ?", ids).Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// LineUsage 主机上「用户 × 线路」的流量累计,只给勾了「忽略部分线路」的突发规则用:这种规则的用量
// 只算这个用户在没被忽略的线路上跑的流量,用户表里的总用量分不出线路,所以另记一份(按倍率计费的字节,和用户用量同一口径)。
//
// 每台服务器(主机自己也算一台)报来的是它这次开始计数以来的累计(model.LineMeter),这里按台记住上一份,只加增量:
//   - 头一回见到这台(主机刚启动、功能刚打开、这台刚开始报):只记下来当基线,不加 —— 不然这台攒了很久的累计
//     会一下子落进突发窗口,误触发;
//   - 这台换了 Boot(重启过、功能关掉又打开):计数是从 0 重来的,这一份整份都是新流量,全加;
//   - 同一个 Boot:按差值加;某一项比上一份小,是那一项闲置太久被清掉后从 0 重记的(见 jobs 的 meterIdle),按新出现的整份加。
//
// 只在内存里:主机重启后从空开始(突发窗口本来也只在内存里,重启后重来)。没有规则用得着时 SetOn(false) 清空,
// 这时报上来的也不收,免得功能再打开时把关着那段的流量算进去。
type LineUsage struct {
	mu    sync.Mutex
	on    bool
	nodes map[uint]*meterSeen
	acc   map[string]map[uint]*accVal // 用户名 → 线路 id → 计费字节
	now   func() int64                // 测试里换掉;nil = 当前时间
}

type meterKey struct {
	user string
	line uint
}

type meterSeen struct {
	boot string
	last map[meterKey][2]int64 // Raw, Billed
}

// accVal 一个「用户 × 线路」的累计与最近一次涨的时间。
type accVal struct{ n, at int64 }

// usageIdle 一项多久没涨就清掉(突发窗口最长 24 小时,再久的累计用不着;之后又涨了就从 0 记,判定那边看到变小会把窗口重开)。
const usageIdle = 25 * 3600

func (u *LineUsage) clock() int64 {
	if u.now != nil {
		return u.now()
	}
	return time.Now().Unix()
}

// SetOn 有没有规则要用它;从开到关清空全部记录。
func (u *LineUsage) SetOn(on bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !on && u.on {
		u.nodes, u.acc = nil, nil
	}
	u.on = on
}

// Feed 收一台服务器报来的累计。ratio 是这台服务器的流量倍率(≤0 当 1),只乘 Raw。
func (u *LineUsage) Feed(node uint, ratio float64, m model.LineMeter) {
	if ratio <= 0 {
		ratio = 1
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.on {
		return
	}
	if u.nodes == nil {
		u.nodes = map[uint]*meterSeen{}
	}
	if u.acc == nil {
		u.acc = map[string]map[uint]*accVal{}
	}
	cur := make(map[meterKey][2]int64, len(m.Rows))
	for _, r := range m.Rows {
		if r.User == "" || r.Line == 0 {
			continue
		}
		k := meterKey{r.User, r.Line}
		v := cur[k]
		cur[k] = [2]int64{v[0] + r.Raw, v[1] + r.Billed}
	}
	seen := u.nodes[node]
	if seen == nil {
		u.nodes[node] = &meterSeen{boot: m.Boot, last: cur}
		return
	}
	fresh := seen.boot != m.Boot
	now := u.clock()
	for k, v := range cur {
		dRaw, dBilled := v[0], v[1]
		if old, ok := seen.last[k]; ok && !fresh && v[0] >= old[0] && v[1] >= old[1] {
			dRaw, dBilled = v[0]-old[0], v[1]-old[1]
		}
		add := int64(0)
		if dRaw > 0 {
			add += int64(float64(dRaw) * ratio)
		}
		if dBilled > 0 {
			add += dBilled
		}
		if add <= 0 {
			continue
		}
		lines := u.acc[k.user]
		if lines == nil {
			lines = map[uint]*accVal{}
			u.acc[k.user] = lines
		}
		a := lines[k.line]
		if a == nil {
			a = &accVal{}
			lines[k.line] = a
		}
		a.n += add
		a.at = now
	}
	seen.boot, seen.last = m.Boot, cur
}

// Prune 清掉太久没涨的累计(见 usageIdle)。判定每轮调一次。
func (u *LineUsage) Prune() {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.clock()
	for user, lines := range u.acc {
		for line, a := range lines {
			if now-a.at > usageIdle {
				delete(lines, line)
			}
		}
		if len(lines) == 0 {
			delete(u.acc, user)
		}
	}
}

// Sum 这个用户在 skip 以外的线路上的累计(计费字节)。
func (u *LineUsage) Sum(user string, skip map[uint]bool) int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	var n int64
	for line, a := range u.acc[user] {
		if !skip[line] {
			n += a.n
		}
	}
	return n
}
