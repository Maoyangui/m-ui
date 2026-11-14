package runner

import (
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/ops"
)

// 磁盘占用要遍历 /var/log、软件包缓存,统计一次缓存这么久(副机每次报告都带、概况每次刷新都要)。
const diskStatTTL = 5 * time.Minute

type diskCache struct {
	mu      sync.Mutex
	st      *model.DiskStat
	alerted string // 本机磁盘已经发过哪一级的告警(主机巡检自己的盘用)
}

// diskStatOf 换掉统计函数(测试用)。
var diskStatOf = ops.DiskStatOf

// DiskStat 本机数据目录所在盘的占用(带 5 分钟缓存;清理磁盘之后调 ForgetDiskStat 重新统计)。
func (r *Runner) DiskStat() *model.DiskStat {
	r.disk.mu.Lock()
	defer r.disk.mu.Unlock()
	if st := r.disk.st; st != nil && time.Since(time.Unix(st.At, 0)) < diskStatTTL {
		c := *st
		return &c
	}
	st := diskStatOf(r.DataDir())
	if st.Total <= 0 { // 读不到(开发机、权限):不报,主机当老副机处理
		r.disk.st = nil
		return nil
	}
	r.disk.st = &st
	c := st
	return &c
}

// ForgetDiskStat 丢掉缓存的磁盘占用(清理磁盘、建 swap 之后马上看得到变化)。
func (r *Runner) ForgetDiskStat() {
	r.disk.mu.Lock()
	r.disk.st = nil
	r.disk.mu.Unlock()
}

// 本机磁盘多久查一次(主机自己的盘;副机的随报告上报、由主机的 hub 告警)。
var (
	diskFirstCheck = 2 * time.Minute
	diskCheckEvery = 10 * time.Minute
)

// diskLoop 主机巡检自己的磁盘:快满了、满了各发一次 Telegram(变严重再发),降下来一截再说恢复。副机不发(主机替它发)。
func (r *Runner) diskLoop(stop <-chan struct{}) {
	first := time.NewTimer(diskFirstCheck)
	defer first.Stop()
	select {
	case <-first.C:
	case <-stop:
		return
	}
	r.checkDisk()
	t := time.NewTicker(diskCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.checkDisk()
		case <-stop:
			return
		}
	}
}

func (r *Runner) checkDisk() {
	st := r.DiskStat()
	if st == nil || r.IsNode() {
		return
	}
	r.disk.mu.Lock()
	msg := hub.DiskAlertText(true, "", st, &r.disk.alerted)
	r.disk.mu.Unlock()
	if msg != "" {
		logger.Warning("本机磁盘:", st.Brief())
		r.notifier.Event("tgOnCore", msg)
	}
}
