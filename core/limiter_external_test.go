package core

import "testing"

func TestLimiterExternalIPsCountTowardsDeviceLimit(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"u": {DeviceLimit: 2}})
	l.SetExternalIPs(map[string][]string{"u": {"9.9.9.9"}})
	if !l.AllowConn("u", "1.1.1.1") {
		t.Fatal("第 1 台本机设备(远端已有 1 台)应放行")
	}
	if l.AllowConn("u", "2.2.2.2") {
		t.Fatal("本机 1 + 远端 1 = 上限 2,第 3 台应拒绝")
	}
	// 远端设备下线 → 名额释放
	l.SetExternalIPs(map[string][]string{})
	if !l.AllowConn("u", "2.2.2.2") {
		t.Fatal("远端下线后名额应释放")
	}
	if l.AllowConn("u", "3.3.3.3") {
		t.Fatal("本机已 2 台,第 3 台应拒绝")
	}
}

func TestLimiterExternalSameIPSwitchesEntry(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"u": {DeviceLimit: 1}})
	l.SetExternalIPs(map[string][]string{"u": {"9.9.9.9"}})
	if l.AllowConn("u", "1.1.1.1") {
		t.Fatal("上限 1 且远端已有 1 台,新设备应拒绝")
	}
	if !l.AllowConn("u", "9.9.9.9") {
		t.Fatal("远端在线的同一 IP 切换到本入口应放行(同一设备)")
	}
}

// IPv6 的临时地址:同一个 /64 算一台设备,新地址不占名额;别的 /64 才算新设备。跨机同样。
func TestLimiterIPv6SamePrefixIsOneDevice(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"u": {DeviceLimit: 1}})
	if !l.AllowConn("u", "2001:db8:1:2::a") {
		t.Fatal("第一台")
	}
	if !l.AllowConn("u", "2001:db8:1:2::b") {
		t.Fatal("同一台设备换了个临时地址,不该被拒")
	}
	if l.AllowConn("u", "2001:db8:1:3::a") {
		t.Fatal("别的 /64 是另一台设备,应超限")
	}
	if l.AllowConn("u", "1.2.3.4") {
		t.Fatal("IPv4 也是另一台,应超限")
	}

	m := NewLimiter()
	m.SetLimits(map[string]UserLimitSpec{"u": {DeviceLimit: 1}})
	m.SetExternalIPs(map[string][]string{"u": {"2001:db8:1:2::a"}})
	if !m.AllowConn("u", "2001:db8:1:2::b") {
		t.Fatal("这台设备已在别的机器上(同一个 /64),切入口不占新名额")
	}
	if m.AllowConn("u", "2001:db8:9:9::a") {
		t.Fatal("跨机并集已满,新设备应被拒")
	}
}
