package core

import (
	"net"
	"net/netip"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

// 「忽略部分线路」(用户 11-23)的数据面部分:按「用户 × 线路」计数;规则忽略的线路上换用单独的桶位。

// 每条用户连接都按「用户 × 线路」记上下行合计,单独设了倍率的节点带上倍率;和 user / userx / 线路这几样的计数互不影响;
// 取走即清零;没有用户的连接不记。
func TestTakeLineUsage(t *testing.T) {
	s := NewStatsTracker()
	s.SetNodes(NodeTable{"贵": {Direct: NodeSpec{Key: "2:s1", Ratio: 2}}})
	md := func(inbound, user string) adapter.InboundContext {
		return adapter.InboundContext{Inbound: inbound, User: user, Source: M.Socksaddr{Addr: netip.MustParseAddr("198.51.100.7"), Port: 4000}}
	}
	feed := func(inbound, user string, up, down int64) {
		read, write := s.countersFor(md(inbound, user), "out")
		for _, c := range read {
			c.Add(up)
		}
		for _, c := range write {
			c.Add(down)
		}
	}
	feed("便宜", "alice", 100, 900)
	feed("贵", "alice", 10, 30)
	feed("便宜", "", 5, 5) // 没有用户(内部连接)
	got := map[string]LineUse{}
	for _, u := range s.TakeLineUsage() {
		got[u.User+"|"+u.Inbound] = u
	}
	if len(got) != 2 || got["alice|便宜"].Bytes != 1000 || got["alice|便宜"].Ratio != 0 || got["alice|贵"].Bytes != 40 || got["alice|贵"].Ratio != 2 {
		t.Fatalf("按用户 × 线路记上下行合计、带上节点倍率: %+v", got)
	}
	if left := s.TakeLineUsage(); len(left) != 0 {
		t.Fatalf("取走即清零: %+v", left)
	}
	// 和记账的计数器互不影响:user / userx 照常
	rows := map[string]int64{}
	for _, st := range *s.SnapshotStats() {
		rows[st.Resource+"|"+st.Tag] += st.Traffic
	}
	if rows["user|alice"] != 1000 || rows["userx|"+UserXTag("alice", 2)] != 40 {
		t.Fatalf("记账的计数器不受影响: %v", rows)
	}
}

// 规则忽略了某条线路:走这条线路的连接用这个用户在这条线路上单独的桶位,走别的线路的用用户的;
// 策略一变(规则解除),在线连接跟着改回用户的桶位,不用重连。
func TestLineLimitEntries(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"alice": {DownMbps: 5, Lines: map[string]LineLimit{"便宜": {}}}})
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	cheap := l.wrapConnNode(a, "alice", "198.51.100.1", "便宜", NodeSpec{}).(*limitedConn)
	other := l.wrapConnNode(b, "alice", "198.51.100.1", "正常", NodeSpec{}).(*limitedConn)
	if e := cheap.currentUser(); e == cheap.user || e.down.Load() != nil {
		t.Fatal("忽略的线路上应换用单独的桶位,而且不限")
	}
	if e := other.currentUser(); e != other.user || e.down.Load() == nil || e.down.Load().Limit() != 5*125000 {
		t.Fatal("别的线路用用户的桶位,限 5 Mbps")
	}
	// 规则解除:线路上单独的桶位撤掉,在线连接改回用户的(用户的也不限了)
	l.SetLimits(map[string]UserLimitSpec{})
	if e := cheap.currentUser(); e != cheap.user || e.down.Load() != nil {
		t.Fatal("规则解除后改回用户的桶位")
	}
	// 再触发:忽略的线路上的单独桶位限 20(另一条状态还管着它)
	l.SetLimits(map[string]UserLimitSpec{"alice": {DownMbps: 5, Lines: map[string]LineLimit{"便宜": {DownMbps: 20}}}})
	if e := cheap.currentUser(); e == cheap.user || e.down.Load() == nil || e.down.Load().Limit() != 20*125000 {
		t.Fatal("线路上单独的桶位按那条线路上的限速")
	}
	if e := other.currentUser(); e != other.user {
		t.Fatal("别的线路还是用户的桶位")
	}
	// 不知道走哪条线路的连接(老的调用、测试)一律用用户的
	if c := l.wrapConn(a, "alice", "198.51.100.1").(*limitedConn); c.currentUser() != c.user {
		t.Fatal("没有线路的连接用用户的桶位")
	}
}
