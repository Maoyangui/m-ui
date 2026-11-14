package model

import (
	"strconv"
	"strings"
)

// DiskStat 一台服务器上 m-ui 数据目录所在那块盘的占用(副机随报告上报,主机自己的在状态接口里):
// 服务器页、概况「需要处理」、Telegram 按它提醒「快满了」,运维页「清理磁盘」清的就是 Logs / Cache 这些常见的大头。
// 老版本副机不报(nil)。
type DiskStat struct {
	Path  string `json:"path"`            // 统计的挂载点(数据目录所在)
	Total int64  `json:"total"`           // 总容量(字节)
	Used  int64  `json:"used"`            // 已用(和 df 的 Used 一样)
	Free  int64  `json:"free"`            // 普通进程还能用的(和 df 的 Avail 一样,不含给 root 预留的)
	Swap  int64  `json:"swap,omitempty"`  // 本机 swap 总量(swap 文件也占这块盘)
	Logs  int64  `json:"logs,omitempty"`  // /var/log 合计(syslog、journal 等;谷歌云 Ops Agent 报错刷满的就是这里)
	Cache int64  `json:"cache,omitempty"` // 软件包缓存(apt / dnf / yum 下载的安装包,清掉不影响运行)
	// OpsAgent 装了谷歌云 Ops Agent 且没屏蔽:它上报失败时把整段堆栈刷进 syslog,清理磁盘时问要不要一起停用
	OpsAgent bool  `json:"opsAgent,omitempty"`
	At       int64 `json:"at"` // 统计时间
}

// 磁盘告警的级别(DiskStat.Level)。
const (
	DiskWarn = "warn" // 快满了:用了 90% 以上,或只剩不到 1GB
	DiskBad  = "bad"  // 满了:用了 97% 以上,或只剩不到 300MB —— 写库、写日志、签证书随时会失败
)

// DiskLevel 按已用与可用算告警级别(前端 issues.js 的 diskLevel 同一口径;百分比和 df 的 Use% 一样算)。
// 读不到(两个都是 0)不告警。
func DiskLevel(used, free int64) string {
	if used < 0 || free < 0 || used+free <= 0 {
		return ""
	}
	pct := float64(used) / float64(used+free)
	switch {
	case free < 300<<20 || pct >= 0.97:
		return DiskBad
	case free < 1<<30 || pct >= 0.90:
		return DiskWarn
	}
	return ""
}

// Level 这块盘的告警级别;nil(老副机没报)不告警。
func (d *DiskStat) Level() string {
	if d == nil {
		return ""
	}
	return DiskLevel(d.Used, d.Free)
}

// UsedPct 已用百分比(0–100,向上取整,和 df 的 Use% 一样;读不到时 0)。
func (d *DiskStat) UsedPct() int {
	if d == nil || d.Used+d.Free <= 0 {
		return 0
	}
	return int((d.Used*100 + d.Used + d.Free - 1) / (d.Used + d.Free))
}

// DiskRecovered 告警过的盘降到这以下才算恢复:比告警线低一截(用了不到 87%、剩 1.5GB 以上),在 90% 上下来回时不反复发。
func DiskRecovered(used, free int64) bool {
	if used < 0 || free < 0 || used+free <= 0 {
		return true
	}
	return float64(used)/float64(used+free) < 0.87 && free >= 1536<<20
}

// Brief 「已用 92%,剩 781 MB(共 8.7 GB)」:告警、通知里用。
func (d *DiskStat) Brief() string {
	if d == nil {
		return ""
	}
	return "已用 " + strconv.Itoa(d.UsedPct()) + "%,剩 " + diskBytes(d.Free) + "(共 " + diskBytes(d.Total) + ")"
}

// Hogs 常见的大头:「系统日志 3.2 GB、软件包缓存 400 MB、swap 4.0 GB」(没有的不写)。
func (d *DiskStat) Hogs() string {
	if d == nil {
		return ""
	}
	var parts []string
	for _, p := range []struct {
		name string
		n    int64
	}{{"系统日志", d.Logs}, {"软件包缓存", d.Cache}, {"swap", d.Swap}} {
		if p.n >= 100<<20 {
			parts = append(parts, p.name+" "+diskBytes(p.n))
		}
	}
	return strings.Join(parts, "、")
}

func diskBytes(n int64) string {
	const gb, mb = 1 << 30, 1 << 20
	switch {
	case n >= gb:
		return strconv.FormatFloat(float64(n)/gb, 'f', 1, 64) + " GB"
	case n >= mb:
		return strconv.FormatInt(n/mb, 10) + " MB"
	}
	return strconv.FormatInt(n, 10) + " B"
}
