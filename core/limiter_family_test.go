package core

import (
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 同一台双栈设备先走 IPv4、再走 IPv6:设备数按地址族分别算,不能被当成第二台(上限 1 也能连)。
// 这是"v6 线路连不上、面板也看不到连接"的根因:之前 v4 地址和 v6 /64 合在一起数,第二族的连接被静默拒绝。
func TestDeviceLimitCountsFamiliesSeparately(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"u": {DeviceLimit: 1}})
	if !l.AllowConn("u", "203.0.113.7") {
		t.Fatal("第一台设备(IPv4)应放行")
	}
	if !l.AllowConn("u", "2001:db8:1:2::10") {
		t.Fatal("同一台设备再走 IPv6 应放行:v4 与 v6 各自计数")
	}
	if !l.AllowConn("u", "2001:db8:1:2::abcd") {
		t.Fatal("同一个 /64 里换地址是同一台设备")
	}
	if ok, why := l.AllowConnWhy("u", "198.51.100.9"); ok || why != RejectDevice {
		t.Fatalf("第二个 IPv4 地址是第二台设备,上限 1 应拒绝(原因 device),得 ok=%v why=%q", ok, why)
	}
	if ok, why := l.AllowConnWhy("u", "2001:db8:9:9::1"); ok || why != RejectDevice {
		t.Fatalf("第二个 IPv6 /64 是第二台设备,应拒绝,得 ok=%v why=%q", ok, why)
	}
}

// 代理设备池同样按地址族分别算;池满的拒绝原因是 pool。
func TestGroupPoolCountsFamiliesSeparately(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"a": {Group: "r"}, "b": {Group: "r"}})
	l.SetGroups(map[string]GroupLimitSpec{"r": {DeviceLimit: 1}})
	if !l.AllowConn("a", "203.0.113.7") || !l.AllowConn("a", "2001:db8:1:2::1") {
		t.Fatal("池上限 1:同一台设备的 v4 与 v6 都应放行")
	}
	if ok, why := l.AllowConnWhy("b", "198.51.100.9"); ok || why != RejectPool {
		t.Fatalf("池里第二台 IPv4 设备应以 pool 拒绝,得 ok=%v why=%q", ok, why)
	}
	if got := l.GroupState()["r"].Devices; got != 1 {
		t.Fatalf("池在线设备按两族较大者显示,应为 1,得 %d", got)
	}
}

// 跨机对账也按地址族:本机一台 v4 设备 + 别处一台 v6 设备,上限 1 时互不挤占。
func TestExcessExternalPerFamily(t *testing.T) {
	set := map[string]bool{"203.0.113.7": true, "2001:db8:1:2::/64": true, "198.51.100.9": true}
	local := map[string]bool{"203.0.113.7": true}
	got := excessExternal(set, local, 1)
	if len(got) != 1 || got[0] != "198.51.100.9" {
		t.Fatalf("只有第二个 IPv4 超出,得 %v", got)
	}
}

// 被拒的连接要进"最近入站连接"并标原因;同一来源下一次放行就清掉。
func TestRecentConnRecordsRejection(t *testing.T) {
	c := NewConnTracker(nil)
	c.noteRecent("2001:db8::1", "u", "R6", "vless", RejectDevice)
	r := c.Recent(10)
	if len(r) != 1 || r[0].Rejected != RejectDevice || r[0].User != "u" {
		t.Fatalf("应记下被拒与原因,得 %+v", r)
	}
	c.noteRecent("2001:db8::1", "u", "R6", "vless", "")
	if r = c.Recent(10); r[0].Rejected != "" || r[0].Count != 2 {
		t.Fatalf("放行后应清掉拒绝标记并累加次数,得 %+v", r)
	}
}

func TestDeviceCountTakesLargerFamily(t *testing.T) {
	ips := []string{"203.0.113.7", "2001:db8:1:2::1", "2001:db8:1:2::2", "2001:db8:5:5::1"}
	if got := model.DeviceCount(ips); got != 2 {
		t.Fatalf("1 个 v4 + 2 个 v6 /64,按较大者应为 2,得 %d", got)
	}
}
