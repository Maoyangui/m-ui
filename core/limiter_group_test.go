package core

import (
	"net"
	"testing"
)

// 代理设备池:名下用户同时在线的不同 IP 总数不能超过池上限;用户自己的上限仍各自算;池满只拒新设备。
func TestGroupDevicePool(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{
		"a": {Group: "r1", DeviceLimit: 5}, // 用户自己的上限比池大,以池为准
		"b": {Group: "r1"},                 // 用户不限设备,只受池限
		"c": {Group: "r2"},
		"x": {}, // 主面板用户,不在任何池
	})
	l.SetGroups(map[string]GroupLimitSpec{"r1": {DeviceLimit: 3}, "r2": {DeviceLimit: 1}})

	for _, c := range []struct{ user, ip string }{{"a", "1.1.1.1"}, {"a", "1.1.1.2"}, {"b", "1.1.1.3"}} {
		if !l.AllowConn(c.user, c.ip) {
			t.Fatalf("池未满,%s@%s 应放行", c.user, c.ip)
		}
	}
	if l.AllowConn("b", "1.1.1.4") {
		t.Fatal("池已满(3 台),b 的新设备应被拒")
	}
	if l.AllowConn("a", "1.1.1.5") {
		t.Fatal("池已满,a 的新设备也应被拒,哪怕 a 自己的上限还没到")
	}
	if !l.AllowConn("a", "1.1.1.1") || !l.AllowConn("b", "1.1.1.3") {
		t.Fatal("已在池里的设备应照常放行")
	}
	if !l.AllowConn("c", "9.9.9.9") || l.AllowConn("c", "9.9.9.8") {
		t.Fatal("另一个池独立计数")
	}
	if !l.AllowConn("x", "5.5.5.5") || !l.AllowConn("x", "5.5.5.6") {
		t.Fatal("不在池里的用户不受池限")
	}
	st := l.GroupState()
	if st["r1"].Devices != 3 || st["r1"].Rejects != 2 || st["r2"].Rejects != 1 {
		t.Fatalf("池状态不对:%+v", st)
	}
	if again := l.GroupState(); again["r1"].Rejects != 0 {
		t.Fatal("拒绝计数取走后应清零")
	}
}

// 同一个 IP 用了组里两个用户,只占一个名额;别的机器上在线的 IP(Hub 下发)也计入池。
func TestGroupDevicePoolCountsDistinctIPsAcrossMachines(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"a": {Group: "r1"}, "b": {Group: "r1"}})
	l.SetGroups(map[string]GroupLimitSpec{"r1": {DeviceLimit: 2}})
	l.SetExternalIPs(map[string][]string{"b": {"8.8.8.8"}}) // b 在另一台机器上有一台设备在线

	if !l.AllowConn("a", "1.1.1.1") {
		t.Fatal("池里 1(远端)+1 = 2,应放行")
	}
	if !l.AllowConn("b", "1.1.1.1") {
		t.Fatal("同一 IP 换个用户不占新名额")
	}
	if l.AllowConn("a", "1.1.1.2") {
		t.Fatal("远端 1 + 本机 1 已满,新 IP 应被拒")
	}
	if !l.AllowConn("a", "8.8.8.8") {
		t.Fatal("远端已在线的 IP 切到本机不占新名额")
	}
}

// 带宽池:属于池的用户,连接会同时挂上用户桶和池桶;池不限速且无设备池时不多包一层。
func TestGroupBandwidthBuckets(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"a": {Group: "r1", UpMbps: 10}, "b": {Group: "r1"}, "c": {Group: "r9"}, "x": {}})
	l.SetGroups(map[string]GroupLimitSpec{"r1": {UpMbps: 100, DownMbps: 50}, "r9": {}})
	pipe := func() net.Conn { a, _ := net.Pipe(); return a }

	ca, ok := l.wrapConn(pipe(), "a", "1.1.1.1").(*limitedConn)
	if !ok || ca.up == nil || ca.gup == nil || ca.gdown == nil || ca.down != nil {
		t.Fatalf("a 应有自己的上行桶 + 池的上下行桶:%+v", ca)
	}
	cb, ok := l.wrapConn(pipe(), "b", "1.1.1.2").(*limitedConn)
	if !ok || cb.up != nil || cb.gup == nil || cb.gup != ca.gup || cb.gdown != ca.gdown {
		t.Fatal("b 自己不限速,但要和 a 共用同一对池桶")
	}
	if _, wrapped := l.wrapConn(pipe(), "c", "1.1.1.3").(*limitedConn); wrapped {
		t.Fatal("池不限速也不限设备时不该多包一层")
	}
	if _, wrapped := l.wrapConn(pipe(), "x", "1.1.1.4").(*limitedConn); wrapped {
		t.Fatal("主面板无限制用户不该被包装")
	}
	if got := ca.gup.Limit(); float64(got) != 100*125000 {
		t.Fatalf("池上行桶速率应为 100 Mbps = %d B/s,实际 %v", 100*125000, got)
	}
}
