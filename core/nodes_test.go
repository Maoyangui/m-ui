package core

import (
	"net/netip"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

// 一条连接算哪个节点的:来源对得上中转入口就是那个入口(它没单独设东西也不能套直连的设置),否则是直连。
func TestNodeTableLookup(t *testing.T) {
	relay, plainRelay := netip.MustParseAddr("203.0.113.9"), netip.MustParseAddr("2001:db8::9")
	tb := NodeTable{"line-a": {
		Direct:  NodeSpec{Key: "1:s2", Ratio: 2},
		Sources: map[netip.Addr]NodeSpec{relay: {Key: "1:f1", Ratio: 5}, plainRelay: {}},
	}}
	for _, c := range []struct {
		name, inbound, src string
		want               float64
	}{
		{"直连", "line-a", "198.51.100.1", 2},
		{"经单独设了倍率的中转入口", "line-a", "203.0.113.9", 5},
		{"来源是 v4 映射成的 v6", "line-a", "::ffff:203.0.113.9", 5},
		{"经没单独设的中转入口:跟随服务器,不套直连的倍率", "line-a", "2001:db8::9", 0},
		{"别的入站", "line-b", "203.0.113.9", 0},
	} {
		if got := tb.Lookup(c.inbound, netip.MustParseAddr(c.src)).Ratio; got != c.want {
			t.Errorf("%s:倍率应是 %v,实际 %v", c.name, c.want, got)
		}
	}
	if got := tb.Lookup("line-a", netip.Addr{}).Ratio; got != 2 {
		t.Errorf("来源地址不可用时按直连,实际 %v", got)
	}
	if got := (NodeTable)(nil).Lookup("line-a", relay); got != (NodeSpec{}) {
		t.Errorf("没有表:什么都不单独设,实际 %+v", got)
	}
}

func TestUserXTag(t *testing.T) {
	for _, c := range []struct {
		user  string
		ratio float64
	}{{"alice", 2}, {"张三", 0.5}, {"a b|c", 12.3456}, {"x\x1fy", 3}} {
		u, r, ok := SplitUserX(UserXTag(c.user, c.ratio))
		if !ok || u != c.user || r != c.ratio {
			t.Errorf("%q × %v 拆回来是 %q × %v(ok=%v)", c.user, c.ratio, u, r, ok)
		}
	}
	for _, bad := range []string{"", "alice", "alice\x1f", "alice\x1fabc", "alice\x1f0", "alice\x1f-2", "\x1f2"} {
		if _, _, ok := SplitUserX(bad); ok {
			t.Errorf("%q 不该被当成合法的标签", bad)
		}
	}
}

// 走了单独设倍率的节点的连接:用户流量只记进 userx(带倍率),不在 user 里重复;线路 / 上游维度照常。
func TestStatsTrackerNodeRatio(t *testing.T) {
	s := NewStatsTracker()
	s.SetNodes(NodeTable{"in": {Direct: NodeSpec{Key: "1:s1", Ratio: 2.5}}})
	md := func(inbound string) adapter.InboundContext {
		return adapter.InboundContext{Inbound: inbound, User: "alice", Source: M.Socksaddr{Addr: netip.MustParseAddr("198.51.100.7"), Port: 4000}}
	}
	feed := func(inbound string, up, down int64) {
		read, write := s.countersFor(md(inbound), "out")
		for _, c := range read {
			c.Add(up)
		}
		for _, c := range write {
			c.Add(down)
		}
	}
	rows := func() map[string]int64 {
		out := map[string]int64{}
		for _, st := range *s.SnapshotStats() {
			dir := "down"
			if st.Direction {
				dir = "up"
			}
			out[st.Resource+"|"+st.Tag+"|"+dir] = st.Traffic
		}
		return out
	}
	feed("in", 100, 40)
	got := rows()
	if got["userx|"+UserXTag("alice", 2.5)+"|up"] != 100 || got["userx|"+UserXTag("alice", 2.5)+"|down"] != 40 {
		t.Fatalf("单独倍率的节点上的流量应记进 userx:%v", got)
	}
	if _, dup := got["user|alice|up"]; dup {
		t.Fatalf("同一份流量不能在 user 里再记一遍:%v", got)
	}
	if got["inbound|in|up"] != 100 || got["outbound|out|down"] != 40 {
		t.Fatalf("线路 / 上游维度照常记:%v", got)
	}
	// 同一个人走没单独设倍率的线路:记进 user
	feed("other", 7, 3)
	got = rows()
	if got["user|alice|up"] != 7 || got["userx|"+UserXTag("alice", 2.5)+"|up"] != 100 {
		t.Fatalf("普通线路的流量应记进 user,两边互不影响:%v", got)
	}
	// 记完账按快照原值扣掉,两种计数器都归零;还回去也都认
	snap := *s.SnapshotStats()
	s.ConsumeStats(snap)
	if left := rows(); len(left) != 0 {
		t.Fatalf("扣完应全部归零:%v", left)
	}
	s.RestoreStats(snap)
	if got = rows(); got["userx|"+UserXTag("alice", 2.5)+"|down"] != 40 || got["user|alice|down"] != 3 {
		t.Fatalf("还回去的流量不对:%v", got)
	}
	// 表撤掉以后新连接回到 user
	s.ConsumeStats(snap)
	s.SetNodes(nil)
	feed("in", 5, 5)
	if got = rows(); got["user|alice|up"] != 5 || len(got) != 6 {
		t.Fatalf("没有节点单独设倍率时都记进 user:%v", got)
	}
}
