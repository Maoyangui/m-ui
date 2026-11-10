package web

import (
	"testing"

	"github.com/Maoyangui/m-ui/hub"
)

// 多服务器时本机的标上服务器名,和副机的合起来按时间排、各自限量(正常的 100 条、失败的 50 条分开算)。
func TestMergeRecent(t *testing.T) {
	local := []hub.RecentConn{{IP: "1.1.1.1", Ts: 30}, {IP: "1.1.1.2", Ts: 10}}
	remote := []hub.RecentConn{{IP: "2.2.2.2", Ts: 20, Server: "台湾"}}
	got := mergeRecent(local, remote, "香港", 2)
	if len(got) != 2 || got[0].IP != "1.1.1.1" || got[0].Server != "香港" || got[1].IP != "2.2.2.2" || got[1].Server != "台湾" {
		t.Fatalf("合并结果: %+v", got)
	}
}
