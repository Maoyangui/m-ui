package hub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 磁盘告警:快满了发一次、满了再发一次,在告警线上下来回不反复发,降下来一截才说恢复;主机自己的盘指路到运维页。
func TestDiskAlertText(t *testing.T) {
	const G, M = int64(1) << 30, int64(1) << 20
	disk := func(used, free int64) *model.DiskStat {
		return &model.DiskStat{Total: used + free, Used: used, Free: free, Logs: 3 * G, Swap: 4 * G}
	}
	var alerted string
	step := func(d *model.DiskStat, want string) {
		t.Helper()
		got := DiskAlertText(false, "tw <1>", d, &alerted)
		if want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Fatalf("应 %q,实际 %q(已发 %q)", want, got, alerted)
		}
	}
	step(nil, "")                        // 老副机没报
	step(disk(50*G, 50*G), "")           // 宽裕
	step(disk(91*G, 9*G), "磁盘快满了")       // 过线:发一次
	step(disk(92*G, 8*G), "")            // 还在线上:不重复
	step(disk(98*G, 2*G), "磁盘满了")        // 变严重:再发
	step(disk(99*G, 1*G), "")            // 一直满着:不重复
	step(disk(89*G, 11*G), "")           // 掉回 89%:还没降够,不说恢复(也不再发快满了)
	step(disk(91*G, 9*G), "")            // 又上去:已经发过,不发
	step(disk(80*G, 20*G), "磁盘空间恢复")     // 降够了
	step(disk(8*G+800*M, 250*M), "磁盘满了") // 小盘直接满:一步到位发「满了」
	got := DiskAlertText(false, "tw <1>", disk(0, 0), &alerted)
	if got != "" {
		t.Fatalf("读不到不说恢复也不告警: %q", got)
	}
	var a2 string
	if m := DiskAlertText(false, "tw <1>", disk(95*G, 5*G), &a2); !strings.Contains(m, "tw &lt;1&gt;") || !strings.Contains(m, "系统日志 3.0 GB") || !strings.Contains(m, "服务器页对它点「清理磁盘」") {
		t.Fatalf("副机:名字转义、写出占地方的、指路到服务器页: %q", m)
	}
	var a3 string
	if m := DiskAlertText(true, "", disk(95*G, 5*G), &a3); !strings.Contains(m, "主机 ") || !strings.Contains(m, "运维页") {
		t.Fatalf("主机自己的盘:指路到运维页: %q", m)
	}
}
