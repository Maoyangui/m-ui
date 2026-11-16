package core

import (
	"context"
	"net"
	"testing"

	"github.com/sagernet/sing-box/adapter"
)

// 挂在连接上的附加逻辑出了意外(这里用「没有匹配到出站」造一个空指针),不能带走进程:这一条连接原样放行。
func TestTrackerEntryPointsSurvivePanic(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	st := NewStatsTracker()
	got := st.RoutedConnection(context.Background(), a, adapter.InboundContext{Inbound: "in", User: "alice"}, nil, nil)
	if got != a {
		t.Fatalf("流量统计出意外时应把原连接放行,得到 %T", got)
	}
	if got := st.RoutedPacketConnection(context.Background(), nil, adapter.InboundContext{Inbound: "in"}, nil, nil); got != nil {
		t.Fatalf("UDP 同理:原样返回,得到 %T", got)
	}

	// 连接登记:来源地址是空的(Addr 零值)这类意外也一样兜住;登记到一半的不留在表里
	ct := NewConnTracker(nil)
	ct.connections = nil // 往 nil map 里登记会 panic:模拟登记途中出错
	if got := ct.RoutedConnection(context.Background(), a, adapter.InboundContext{Inbound: "in", User: "alice"}, nil, nil); got != a {
		t.Fatalf("连接登记出意外时应把原连接放行,得到 %T", got)
	}
	if got := ct.RoutedPacketConnection(context.Background(), nil, adapter.InboundContext{Inbound: "in"}, nil, nil); got != nil {
		t.Fatalf("UDP 同理,得到 %T", got)
	}
}
