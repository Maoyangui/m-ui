// Package monitor 巡检与预警:定时检测上游连通性(故障/恢复告警)、数据面看门狗、
// 用户到期/用量预警、每日报告。检测结果同时提供给面板展示。
package monitor

import (
	"fmt"
	"github.com/Maoyangui/m-ui/tz"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"

	"gorm.io/gorm"
)

// UpstreamHealth 是一个上游的最近巡检结果。
type UpstreamHealth struct {
	Id        uint   `json:"id"`
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	DelayMs   int    `json:"delayMs"`
	Method    string `json:"method"`
	Error     string `json:"error,omitempty"`
	CheckedAt int64  `json:"checkedAt"`
	Fails     int    `json:"fails"` // 连续失败次数
}

type Deps struct {
	DB          *gorm.DB
	Setting     func(string) string
	CoreRunning func() bool
	Check       func(model.Upstream) (ok bool, delayMs int, method, errStr string)
	Notify      *notify.Notifier
	// UsedUpstreams 本机线路真正用到的上游 id。上游通不通只在跑这条线路的机器上量才有意义:
	// 主机在香港、落地从别的机器出去,主机测得通不代表那台通;主机不部署线路时更是一条都不该测。
	UsedUpstreams func() map[uint]bool
	// RemoteHealth 各副机上报的结果(只有主机有),和本机结果合在一起判断告警
	RemoteHealth func() []NodeHealth
	SelfName     func() string // 本机在服务器列表里的名字,告警文案里点名是哪台
	SelfNodeId   func() uint
	Alerting     func() bool // 只有主机发告警:副机没有通知配置,也不该重复发
}

// NodeHealth 某台服务器上某条上游的巡检结果(主机汇总各机结果时用)。
type NodeHealth struct {
	NodeId    uint   `json:"nodeId"`
	NodeName  string `json:"nodeName"`
	Id        uint   `json:"id"`
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	DelayMs   int    `json:"delayMs"`
	Method    string `json:"method"`
	Error     string `json:"error,omitempty"`
	CheckedAt int64  `json:"checkedAt"`
	Fails     int    `json:"fails"`
}

type Monitor struct {
	d        Deps
	mu       sync.Mutex
	runMu    sync.Mutex               // 串行化整轮巡检:面板点"立即巡检"时定时那轮可能正在跑,两轮同时跑会把失败次数多加一次
	results  map[uint]*UpstreamHealth // 本机测出来的结果(副机上报的就是它)
	alerted  map[string]bool          // "服务器 id:上游 id" → 已经告过警,避免同一条故障反复发
	lastRun  int64
	coreDown bool
	stop     chan struct{}
	wg       sync.WaitGroup
}

func New(d Deps) *Monitor {
	return &Monitor{d: d, results: map[uint]*UpstreamHealth{}, alerted: map[string]bool{}, stop: make(chan struct{})}
}

// loc 面板设置的时区:日报发送时点与消息里的日期都按它算,
// 否则服务器在美国、面板设成 Asia/Shanghai 时,日报会在错误的钟点发出。
func (m *Monitor) loc() *time.Location {
	return tz.Location(m.d.Setting("timezone"))
}

// now 面板时区下的当前时间。
func (m *Monitor) now() time.Time { return time.Now().In(m.loc()) }

func (m *Monitor) settingInt(key string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(m.d.Setting(key)))
	if err != nil {
		return def
	}
	return v
}

func (m *Monitor) Start() {
	m.loop(30*time.Second, m.tickUpstreams)
	m.loop(time.Minute, m.tickCore)
	m.loop(time.Hour, m.CheckUsers)
	m.loop(time.Minute, m.tickDaily)
	logger.Info("巡检已启动:上游按设置间隔 / 数据面看门狗 1m / 用户预警 1h / 每日报告")
}

func (m *Monitor) Stop() {
	close(m.stop)
	m.wg.Wait()
}

func (m *Monitor) loop(every time.Duration, fn func()) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				func() { // 巡检里出一次 panic 不该带走整个进程
					defer func() {
						if v := recover(); v != nil {
							logger.Warning("巡检任务异常: ", v, " | ", string(debug.Stack()))
						}
					}()
					fn()
				}()
			case <-m.stop:
				return
			}
		}
	}()
}

// ---- 上游巡检 ----

func (m *Monitor) tickUpstreams() {
	interval := m.settingInt("upstreamCheckMinutes", 10)
	if interval <= 0 {
		return
	}
	if time.Now().Unix()-m.LastRun() < int64(interval*60) {
		return
	}
	m.RunUpstreamCheck()
}

// RunUpstreamCheck 巡检本机线路用到的上游,返回状态发生变化(故障/恢复)的上游名。
// 本机没有线路用到的上游一条都不测:测了也只是"从这台机器看通不通",既没人关心,还会误报。
func (m *Monitor) RunUpstreamCheck() []string {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	m.mu.Lock()
	m.lastRun = time.Now().Unix()
	m.mu.Unlock()
	threshold := m.settingInt("upstreamCheckFailThreshold", 2)
	if threshold < 1 {
		threshold = 1
	}
	var all []model.Upstream
	m.d.DB.Order("id asc").Find(&all)
	used := map[uint]bool{}
	if m.d.UsedUpstreams != nil {
		used = m.d.UsedUpstreams()
	} else {
		for _, u := range all { // 没接这个依赖(测试里)就维持老行为
			used[u.Id] = true
		}
	}
	ups := make([]model.Upstream, 0, len(all))
	for _, u := range all {
		if used[u.Id] {
			ups = append(ups, u)
		}
	}

	type res struct {
		up   model.Upstream
		ok   bool
		ms   int
		meth string
		err  string
	}
	out := make([]res, len(ups))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 12)
	for i, up := range ups {
		wg.Add(1)
		go func(i int, up model.Upstream) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ok, ms, meth, e := m.d.Check(up)
			out[i] = res{up, ok, ms, meth, e}
		}(i, up)
	}
	wg.Wait()

	now := time.Now().Unix()
	m.mu.Lock()
	seen := map[uint]bool{}
	for _, r := range out {
		seen[r.up.Id] = true
		h := m.results[r.up.Id]
		if h == nil {
			h = &UpstreamHealth{Id: r.up.Id}
			m.results[r.up.Id] = h
		}
		h.Name, h.OK, h.DelayMs, h.Method, h.Error, h.CheckedAt = r.up.Name, r.ok, r.ms, r.meth, r.err, now
		if r.ok {
			h.Fails = 0
		} else {
			h.Fails++
		}
	}
	for id := range m.results {
		if !seen[id] { // 线路改了部署或换了上游,本机不再用它,结果作废
			delete(m.results, id)
		}
	}
	local := m.snapshotLocked()
	m.mu.Unlock()
	return m.evaluateAlerts(local, threshold)
}

// snapshotLocked 本机结果转成带服务器身份的形式(调用方须持锁)。
func (m *Monitor) snapshotLocked() []NodeHealth {
	var name string
	var id uint
	if m.d.SelfName != nil {
		name = m.d.SelfName()
	}
	if m.d.SelfNodeId != nil {
		id = m.d.SelfNodeId()
	}
	out := make([]NodeHealth, 0, len(m.results))
	for _, h := range m.results {
		out = append(out, NodeHealth{NodeId: id, NodeName: name, Id: h.Id, Name: h.Name, OK: h.OK,
			DelayMs: h.DelayMs, Method: h.Method, Error: h.Error, CheckedAt: h.CheckedAt, Fails: h.Fails})
	}
	return out
}

// evaluateAlerts 把本机与各副机的结果合在一起判断告警:某条上游在**真正用它的那台服务器**上
// 连续失败到阈值才发,消息里点名是哪台;恢复同理。只有主机做这件事。
// 返回状态发生变化的上游名(面板"立即巡检"用)。
func (m *Monitor) evaluateAlerts(local []NodeHealth, threshold int) []string {
	if m.d.Alerting != nil && !m.d.Alerting() {
		return nil
	}
	merged := local
	if m.d.RemoteHealth != nil {
		merged = append(append([]NodeHealth{}, local...), m.d.RemoteHealth()...)
	}
	var changed []string
	m.mu.Lock()
	defer m.mu.Unlock()
	live := map[string]bool{}
	for _, h := range merged {
		key := fmt.Sprintf("%d:%d", h.NodeId, h.Id)
		live[key] = true
		where := h.Name
		if h.NodeName != "" {
			where = h.Name + " @ " + h.NodeName
		}
		if h.OK {
			if m.alerted[key] {
				delete(m.alerted, key)
				changed = append(changed, h.Name)
				m.d.Notify.Event("tgOnUpstream", fmt.Sprintf("🟢 <b>上游恢复</b>:%s(%d ms)", notify.Esc(where), h.DelayMs))
			}
			continue
		}
		if h.Fails >= threshold && !m.alerted[key] {
			m.alerted[key] = true
			changed = append(changed, h.Name)
			m.d.Notify.Event("tgOnUpstream", fmt.Sprintf("🔴 <b>上游故障</b>:%s\n连续 %d 次失败:%s", notify.Esc(where), h.Fails, notify.Esc(h.Error)))
		}
	}
	for key := range m.alerted { // 上游删了、线路不再用它、副机下线:挂着的告警状态清掉,别留着假红点
		if !live[key] {
			delete(m.alerted, key)
		}
	}
	return changed
}

// Results 返回全部上游的最近巡检结果(按 id 排序)。
func (m *Monitor) Results() []UpstreamHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UpstreamHealth, 0, len(m.results))
	for _, h := range m.results {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// LastRun 返回上次巡检时间(0=尚未运行)。
func (m *Monitor) LastRun() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastRun
}

// ---- 数据面看门狗 ----

func (m *Monitor) tickCore() {
	running := m.d.CoreRunning()
	if !running && !m.coreDown {
		m.coreDown = true
		m.d.Notify.Event("tgOnCore", "🔴 <b>数据面已停止</b>\n所有线路不可用,请登录面板查看日志")
	} else if running && m.coreDown {
		m.coreDown = false
		m.d.Notify.Event("tgOnCore", "🟢 <b>数据面已恢复运行</b>")
	}
}

// ---- 用户预警 ----

// CheckUsers 检查即将到期与用量接近上限的用户,每用户每天最多提醒一次。
func (m *Monitor) CheckUsers() {
	now := time.Now().Unix()
	day := m.now().Format("2006-01-02")
	days := m.settingInt("tgExpiringDays", 3)
	pct := m.settingInt("tgQuotaPercent", 80)

	var users []model.User
	m.d.DB.Where("enabled = ?", true).Find(&users)
	for _, u := range users {
		if days > 0 && u.Expiry > now && u.Expiry-now <= int64(days)*86400 {
			left := (u.Expiry - now + 86399) / 86400
			if m.d.Notify.Once("expiring:"+u.Name+":"+day, 24*time.Hour) {
				m.d.Notify.Event("tgOnUserExpiring", fmt.Sprintf("⏳ <b>用户即将到期</b>:%s\n到期 %s(剩 %d 天)", notify.Esc(u.Name), time.Unix(u.Expiry, 0).In(m.loc()).Format("2006-01-02"), left))
			}
		}
		if pct > 0 && u.Volume > 0 {
			used := u.Up + u.Down
			if used*100 >= u.Volume*int64(pct) && m.d.Notify.Once("quota:"+u.Name, 24*time.Hour) {
				m.d.Notify.Event("tgOnQuota", fmt.Sprintf("📊 <b>用户流量告急</b>:%s\n已用 %s / %s(%d%%)", notify.Esc(u.Name), human(used), human(u.Volume), used*100/u.Volume))
			}
		}
	}
}

// ---- 每日报告 ----

func (m *Monitor) tickDaily() {
	if strings.EqualFold(m.d.Setting("tgDaily"), "false") {
		return
	}
	hour := m.settingInt("tgDailyHour", 9)
	if m.now().Hour() != hour {
		return
	}
	if m.d.Notify.Once("daily:"+m.now().Format("2006-01-02"), 36*time.Hour) {
		m.d.Notify.Event("tgDaily", m.DailyReport())
	}
}

// DailyReport 生成 24 小时运营摘要。
func (m *Monitor) DailyReport() string {
	now := time.Now().Unix()
	since := now - 86400
	var total, enabled, expiringSoon int64
	m.d.DB.Model(&model.User{}).Count(&total)
	m.d.DB.Model(&model.User{}).Where("enabled = ?", true).Count(&enabled)
	m.d.DB.Model(&model.User{}).Where("enabled = ? AND expiry > ? AND expiry < ?", true, now, now+7*86400).Count(&expiringSoon)

	type dir struct {
		Direction bool
		Traffic   int64
	}
	var dirs []dir
	m.d.DB.Model(&model.Stats{}).Select("direction, SUM(traffic) AS traffic").
		Where("resource = ? AND date_time > ?", "user", since).Group("direction").Scan(&dirs)
	var up, down int64
	for _, d := range dirs {
		if d.Direction {
			up = d.Traffic
		} else {
			down = d.Traffic
		}
	}
	type top struct {
		Tag     string
		Traffic int64
	}
	var tops []top
	m.d.DB.Model(&model.Stats{}).Select("tag, SUM(traffic) AS traffic").
		Where("resource = ? AND date_time > ?", "user", since).Group("tag").Order("traffic DESC").Limit(5).Scan(&tops)

	var disabled int64
	m.d.DB.Model(&model.Change{}).Where("actor = ? AND date_time > ?", "DepleteJob", since).Count(&disabled)

	okN, badN := 0, 0
	for _, h := range m.Results() {
		if h.OK {
			okN++
		} else {
			badN++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📈 <b>m-ui 日报</b> %s\n", m.now().Format("2006-01-02"))
	fmt.Fprintf(&b, "用户:%d 启用 / %d 总计,7 天内到期 %d,昨日禁用 %d\n", enabled, total, expiringSoon, disabled)
	fmt.Fprintf(&b, "流量(24h):↑ %s  ↓ %s\n", human(up), human(down))
	if okN+badN > 0 {
		fmt.Fprintf(&b, "上游巡检:%d 正常 / %d 故障\n", okN, badN)
	}
	if len(tops) > 0 {
		b.WriteString("流量 Top:\n")
		for i, t := range tops {
			fmt.Fprintf(&b, "  %d. %s  %s\n", i+1, notify.Esc(t.Tag), human(t.Traffic))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func human(n int64) string {
	f := float64(n)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
