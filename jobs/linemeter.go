package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database/model"
)

// lineMeter 本机「用户 × 线路」的累计(见 model.LineMeter)。只在有启用的突发规则勾了「忽略部分线路」时记
// (Deps.LineMeterOn;默认不记):每轮统计从数据面取走一次(core.StatsTracker.TakeLineUsage),按线路 id 攒着。
// 副机随报告交给主机,主机自己的交给规则判定(见 rules.LineUsage)。只在内存里,进程重启从 0 记(换一个 Boot)。
type lineMeter struct {
	mu   sync.Mutex
	boot string // 这次开始记的代号;空 = 没在记
	rows map[lineMeterKey]*meterVal
}

type lineMeterKey struct {
	user string
	line uint
}

// meterVal 一个「用户 × 线路」的累计:raw 按服务器倍率计费的原始字节,billed 走了单独设倍率的节点、已经乘好的字节,at 最近一次涨的时间。
type meterVal struct{ raw, billed, at int64 }

// meterIdle 一个「用户 × 线路」多久没涨就不再报(突发规则的窗口最长 24 小时,再久没动的累计用不着):
// 报告不会随着用过的人越来越多一直变大。之后又有流量就从 0 重记,主机看到比上一份小、按新出现的算(见 rules.LineUsage)。
const meterIdle = 25 * 3600

// meterRound 每轮统计调一次(调用方持着 statsMu)。功能没开时也要取走,计数器才不会一直攒着;取走就扔。
func (s *Scheduler) meterRound(tracker *core.StatsTracker) {
	use := tracker.TakeLineUsage()
	on := s.d.LineMeterOn != nil && s.d.LineMeterOn()
	var ids map[string]uint
	if on && len(use) > 0 && s.d.LineIDs != nil {
		ids = s.d.LineIDs()
	}
	s.meter.round(on, use, ids, time.Now().Unix())
}

// round 记一轮:没开就清空(下次打开换一个 Boot);开着就把这一轮的字节按线路 id 加上,再清掉太久没涨的。
func (m *lineMeter) round(on bool, use []core.LineUse, ids map[string]uint, now int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !on {
		m.boot, m.rows = "", nil
		return
	}
	if m.boot == "" {
		m.boot = newMeterBoot()
		m.rows = map[lineMeterKey]*meterVal{}
	}
	for _, u := range use {
		id := ids[u.Inbound]
		if id == 0 || u.User == "" { // 不是线路的入站,或线路刚改名、数据面还没换上新名字:这一笔不记
			continue
		}
		k := lineMeterKey{u.User, id}
		v := m.rows[k]
		if v == nil {
			v = &meterVal{}
			m.rows[k] = v
		}
		if u.Ratio > 0 { // 走了单独设倍率的节点:按节点倍率乘好(和记账的 x 一样)
			v.billed += int64(float64(u.Bytes) * u.Ratio)
		} else { // 按服务器倍率:报原始字节,主机按这台的倍率乘
			v.raw += u.Bytes
		}
		v.at = now
	}
	for k, v := range m.rows {
		if now-v.at > meterIdle {
			delete(m.rows, k)
		}
	}
}

// LineMeter 本机「用户 × 线路」的累计;没在记返回 false。
func (s *Scheduler) LineMeter() (model.LineMeter, bool) {
	s.meter.mu.Lock()
	defer s.meter.mu.Unlock()
	if s.meter.boot == "" {
		return model.LineMeter{}, false
	}
	m := model.LineMeter{Boot: s.meter.boot, Rows: make([]model.LineMeterRow, 0, len(s.meter.rows))}
	for k, v := range s.meter.rows {
		m.Rows = append(m.Rows, model.LineMeterRow{User: k.user, Line: k.line, Raw: v.raw, Billed: v.billed})
	}
	sort.Slice(m.Rows, func(i, j int) bool {
		if m.Rows[i].User != m.Rows[j].User {
			return m.Rows[i].User < m.Rows[j].User
		}
		return m.Rows[i].Line < m.Rows[j].Line
	})
	return m, true
}

// newMeterBoot 一次计数的代号:主机看它变没变,分辨这台的累计是不是从 0 重来了。
func newMeterBoot() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return strconv.FormatInt(time.Now().Unix(), 36) + "-" + hex.EncodeToString(b)
}
