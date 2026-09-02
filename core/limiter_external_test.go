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
