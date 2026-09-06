package web

import (
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

// 面板显示的 CPU 占用:固定窗口采样,和面板刷新频率脱钩。
//
// 之前用的是 gopsutil 的 cpu.Percent(0, false),它算的是"距上次调用以来"的平均值。
// 面板刷新变快之后(平时 5 秒、操作后 2 秒,换页还会立刻补一次),两次调用可能只隔几百毫秒,
// 窗口里正好赶上一次垃圾回收,就会显示几十个百分点 —— 看着像机器很忙,其实两分钟平均只有 2%。
// 这里改成自己按 /proc 的累计时间算差值,并且窗口至少 5 秒:谁来问、问多勤,都不影响这个数。
var (
	cpuMu   sync.Mutex
	cpuPrev cpu.TimesStat
	cpuAt   time.Time
	cpuPct  float64
)

const cpuWindow = 5 * time.Second

// cpuBusy 各状态累计时间之和(不含 guest:Linux 上它已经计入 user,再加会重复)。
func cpuBusy(t cpu.TimesStat) (busy, total float64) {
	total = t.User + t.Nice + t.System + t.Idle + t.Iowait + t.Irq + t.Softirq + t.Steal
	return total - t.Idle - t.Iowait, total
}

// cpuPercent 返回最近一个窗口内的整机 CPU 占用百分比。
// 首次调用只取基线返回 0,下一次(至少 5 秒后)才有真实值。
func cpuPercent() (float64, bool) {
	cpuMu.Lock()
	defer cpuMu.Unlock()
	now := time.Now()
	if !cpuAt.IsZero() && now.Sub(cpuAt) < cpuWindow {
		return cpuPct, true
	}
	ts, err := cpu.Times(false)
	if err != nil || len(ts) == 0 {
		return cpuPct, !cpuAt.IsZero()
	}
	cur := ts[0]
	ok := !cpuAt.IsZero()
	if ok {
		b0, t0 := cpuBusy(cpuPrev)
		b1, t1 := cpuBusy(cur)
		if t1 > t0 {
			cpuPct = (b1 - b0) / (t1 - t0) * 100
			if cpuPct < 0 {
				cpuPct = 0
			} else if cpuPct > 100 {
				cpuPct = 100
			}
		}
	}
	cpuPrev, cpuAt = cur, now
	return cpuPct, ok
}
