package core

import (
	"reflect"
	"testing"
)

// 你定过的语义:设备池跨机并集超限时**只拒新设备,已连接的不动**。
// 0.6.10 曾在每次副机上报后按 IP 字典序踢掉在线设备 —— 一个连了几小时、完全合规的付费用户
// 会因为 IP 排得靠后被断网,之后池被别人占满还连不回来。这条钉住:收敛永远不产生受害者。
func TestReconcileNeverKicksConnectedDevices(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"u": {DeviceLimit: 2}})
	if !l.AllowConn("u", "1.1.1.1") || !l.AllowConn("u", "2.2.2.2") {
		t.Fatal("前提:两台本机设备先登记上")
	}
	// 副机恢复上报,带回一台失联期间登记的设备:并集 3 > 上限 2
	victims := l.SetExternalIPs(map[string][]string{"u": {"0.0.0.9"}}) // 字典序排在最前,0.6.10 的算法会留下它、踢掉 2.2.2.2
	if len(victims) != 0 {
		t.Fatalf("并集超限不能踢已连接的设备,却要断开 %v", victims)
	}
	if !l.AllowConn("u", "1.1.1.1") || !l.AllowConn("u", "2.2.2.2") {
		t.Fatal("本机已连接的设备必须继续放行")
	}
	if l.AllowConn("u", "3.3.3.3") {
		t.Fatal("并集已满,新设备应拒绝")
	}
	if l.AllowConn("u", "0.0.0.9") {
		t.Fatal("本机名额已被在线设备占满,副机那台设备切到本机也只能算新登记、应拒绝")
	}
	if victims := l.ReconcileDevices(); len(victims) != 0 {
		t.Fatalf("策略热更新后的收敛同样不能踢人: %v", victims)
	}
}

// excessExternal:本机在线的一律保留,剩余名额按固定顺序分给外部 IP,分不到的拒绝新登记。
func TestExcessExternalKeepsLocalFirst(t *testing.T) {
	set := map[string]bool{"a": true, "b": true, "c": true, "d": true}
	local := map[string]bool{"a": true, "b": true}
	for _, c := range []struct {
		limit int
		want  []string
	}{
		{5, nil},                // 放得下,没有多余的
		{4, nil},                // 刚好
		{3, []string{"d"}},      // 剩 1 个名额给外部,c 拿到,d 拒绝
		{2, []string{"c", "d"}}, // 本机已占满,外部全拒
		{1, []string{"c", "d"}}, // 本机自己就超了:已连接的照样保留,只拒外部
	} {
		if got := excessExternal(set, local, c.limit); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("limit=%d: got %v, want %v", c.limit, got, c.want)
		}
	}
}
