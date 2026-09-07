package core

import (
	"net"
	"testing"

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
