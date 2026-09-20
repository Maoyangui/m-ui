package core

import (
	"net"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// 改限速要对在线连接立刻生效:连接持有的是用户条目,桶原地改速率;从不限到限、从限到不限也一样。
func TestLimitsApplyToLiveConnections(t *testing.T) {
	l := NewLimiter()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	wrapped := l.wrapConn(a, "alice", "1.2.3.4").(*limitedConn)
	if wrapped.user.up.Load() != nil || wrapped.user.down.Load() != nil {
		t.Fatal("没下发策略时不该限速")
	}
	// 之后才下发限速:同一条连接立刻带上桶
	l.SetLimits(map[string]UserLimitSpec{"alice": {UpMbps: 8, DownMbps: 100}})
	up, down := wrapped.user.up.Load(), wrapped.user.down.Load()
	if up == nil || down == nil || up.Limit() != rate.Limit(8*125000) || down.Burst() != 100*125000 {
		t.Fatalf("在线连接应立刻拿到新桶: %v %v", up, down)
	}
	// 改速率:桶不换(不回到满桶),速率原地变
	l.SetLimits(map[string]UserLimitSpec{"alice": {UpMbps: 2, DownMbps: 100}})
	if got := wrapped.user.up.Load(); got != up || got.Limit() != rate.Limit(2*125000) || got.Burst() != 2*125000 {
		t.Fatalf("应原地改速率: %v", got)
	}
	if wrapped.user.down.Load() != down {
		t.Fatal("没变的方向桶不该换")
	}
	// 取消限速:桶置空,连接放开
	l.SetLimits(map[string]UserLimitSpec{})
	if wrapped.user.up.Load() != nil || wrapped.user.down.Load() != nil {
		t.Fatal("取消限速后在线连接应放开")
	}
	// 代理池同理
	l.SetGroups(map[string]GroupLimitSpec{"r1": {DownMbps: 50}})
	l.SetLimits(map[string]UserLimitSpec{"bob": {Group: "r1"}})
	c, d := net.Pipe()
	defer c.Close()
	defer d.Close()
	wb := l.wrapConn(c, "bob", "5.6.7.8").(*limitedConn)
	if wb.group == nil || wb.group.down.Load() == nil || wb.group.down.Load().Limit() != rate.Limit(50*125000) {
		t.Fatal("代理池桶应挂到连接上")
	}
	l.SetGroups(map[string]GroupLimitSpec{"r1": {DownMbps: 5}})
	if wb.group.down.Load().Limit() != rate.Limit(5*125000) {
		t.Fatal("代理池改速率应原地生效")
	}
}

func TestLiveConnectionFollowsResellerPoolChanges(t *testing.T) {
	l := NewLimiter()
	l.SetGroups(map[string]GroupLimitSpec{
		"r1": {DownMbps: 10},
		"r2": {DownMbps: 20},
	})
	l.SetLimits(map[string]UserLimitSpec{"alice": {Group: "r1"}})
	a, _ := net.Pipe()
	defer a.Close()
	c := l.wrapConn(a, "alice", "1.2.3.4").(*limitedConn)
	if c.currentGroup() != c.group {
		t.Fatal("连接初始代理池不正确")
	}
	l.SetLimits(map[string]UserLimitSpec{"alice": {Group: "r2"}})
	if g := c.currentGroup(); g == nil || g == c.group || g.down.Load() == nil || g.down.Load().Limit() != rate.Limit(20*125000) {
		t.Fatal("用户换代理后,已有连接应使用新代理池")
	}
	l.SetLimits(map[string]UserLimitSpec{"alice": {}})
	if c.currentGroup() != nil {
		t.Fatal("用户移出代理后,已有连接不应继续使用旧代理池")
	}
}

// 踢线后本机在线 IP 立刻清掉,不用等空闲窗口;策略与外部 IP 不动,设备重连照常登记。
func TestForgetClearsActiveIPsImmediately(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"alice": {DeviceLimit: 1}})
	l.SetExternalIPs(map[string][]string{"alice": {"9.9.9.9"}})
	if l.AllowConn("alice", "1.1.1.1") {
		t.Fatal("外部已占满名额,本机新设备应被拒")
	}
	l.SetExternalIPs(map[string][]string{})
	if !l.AllowConn("alice", "1.1.1.1") {
		t.Fatal("名额空出后应放行")
	}
	if ips := l.ActiveIPs("alice"); len(ips) != 1 {
		t.Fatalf("应记着一个在线 IP,实际 %v", ips)
	}
	l.Forget("alice")
	l.Forget("") // 空名字无害
	if ips := l.ActiveIPs("alice"); len(ips) != 0 {
		t.Fatalf("踢线后在线 IP 应立刻清空,实际 %v", ips)
	}
	if !l.AllowConn("alice", "2.2.2.2") {
		t.Fatal("清掉之后新设备应占到名额")
	}
	if l.AllowConn("alice", "3.3.3.3") {
		t.Fatal("设备上限策略不该因 Forget 丢失")
	}
}

func TestIdleDeviceResumeRechecksDeviceLimit(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"alice": {DeviceLimit: 1}})
	if !l.AllowConn("alice", "old") {
		t.Fatal("首台设备应放行")
	}
	l.mu.Lock()
	l.ips["alice"]["old"] = time.Now().Unix() - int64(l.idleWindow.Seconds()) - 1
	l.mu.Unlock()
	if !l.AllowConn("alice", "new") {
		t.Fatal("旧设备息屏后,新设备应放行")
	}
	if l.keepaliveFor("alice", "old")() {
		t.Fatal("息屏设备恢复流量时应重新执行设备上限")
	}
}
