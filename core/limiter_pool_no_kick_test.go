package core

import "testing"

// 代理池同样只拒新设备:0.6.10 的收敛会把池里排在字典序后面的 IP 对**所有**用户一并拒掉,
// 于是 u1 那台一直在线的 9.9.9.9 会被 u2 在副机上新加的 1.1.1.1 挤下线,而且之后池已被占满连不回来。
func TestPoolReconcileNeverKicksConnectedDevices(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"u1": {Group: "p"}, "u2": {Group: "p"}})
	l.SetGroups(map[string]GroupLimitSpec{"p": {DeviceLimit: 2}})
	if !l.AllowConn("u1", "9.9.9.9") {
		t.Fatal("前提:u1 的设备先在本机登记上")
	}
	// 副机恢复上报:u2 在失联期间那边登记了两台,并集 3 > 池上限 2
	victims := l.SetExternalIPs(map[string][]string{"u2": {"1.1.1.1", "5.5.5.5"}})
	if len(victims) != 0 {
		t.Fatalf("池超限不能踢已连接的设备,却要断开 %v", victims)
	}
	if !l.AllowConn("u1", "9.9.9.9") {
		t.Fatal("u1 一直在线的设备必须继续放行")
	}
	if l.AllowConn("u2", "7.7.7.7") {
		t.Fatal("池已满,u2 的新设备应拒绝")
	}
	if l.AllowConn("u1", "8.8.8.8") {
		t.Fatal("池已满,u1 的新设备也应拒绝")
	}
}
