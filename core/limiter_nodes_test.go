package core

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

func nodeTab(spec NodeSpec) NodeTable { return NodeTable{"in": {Direct: spec}} }

// 节点的用户容量:数的是同时在线的不同用户;满了只拒新用户,已经在线的(包括他的新连接、新设备)照常;
// 有人空闲超时就让出名额。
func TestNodeCapacity(t *testing.T) {
	l := NewLimiter()
	spec := NodeSpec{Key: "1:s1", Cap: 2}
	l.SetNodes(nodeTab(spec))
	if got := l.NodeFor("in", netip.MustParseAddr("198.51.100.1")); got != spec {
		t.Fatalf("查不到节点:%+v", got)
	}
	allow := func(user, ip string) (bool, string) { return l.AllowConnNode(user, ip, spec) }
	for _, u := range []string{"alice", "bob"} {
		if ok, why := allow(u, "198.51.100.1"); !ok {
			t.Fatalf("%s 应放行:%s", u, why)
		}
	}
	if ok, why := allow("carol", "198.51.100.3"); ok || why != RejectNode {
		t.Fatalf("第三个用户应因节点容量被拒,实际 ok=%v why=%q", ok, why)
	}
	// 被容量拒掉的连接不占设备名额
	if ips := l.ActiveIPs("carol"); len(ips) != 0 {
		t.Fatalf("被拒的用户不该被登记成在线设备:%v", ips)
	}
	// 已经在线的用户再开连接、换设备都不受影响
	if ok, _ := allow("alice", "198.51.100.9"); !ok {
		t.Fatal("已经在线的用户不该被自己占着的名额挡住")
	}
	// 别的节点(没设容量)不受影响
	if ok, _ := l.AllowConnNode("carol", "198.51.100.3", NodeSpec{}); !ok {
		t.Fatal("没设容量的节点不该拒")
	}
	if got := l.NodeUsers()["1:s1"]; len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("节点上在线的用户应是 alice、bob:%v", got)
	}
	// bob 空闲超时:让出名额,carol 进得来
	l.mu.Lock()
	l.nstates["1:s1"].seen["bob"] = time.Now().Unix() - int64(l.idleWindow.Seconds()) - 1
	l.mu.Unlock()
	if ok, why := allow("carol", "198.51.100.3"); !ok {
		t.Fatalf("有人下线后应放行:%s", why)
	}
	// 有流量就一直算在线:alice 快超时了,流量回调把她续上,dave 还是进不来
	l.mu.Lock()
	l.nstates["1:s1"].seen["alice"] = time.Now().Unix() - int64(l.idleWindow.Seconds()) + 1
	l.mu.Unlock()
	if !l.keepaliveFor("alice", "198.51.100.1", "1:s1")() {
		t.Fatal("流量回调不该失败")
	}
	l.mu.Lock()
	fresh := l.nstates["1:s1"].seen["alice"] >= time.Now().Unix()-1
	l.mu.Unlock()
	if !fresh {
		t.Fatal("有流量的用户应被续成在线")
	}
	if ok, why := allow("dave", "198.51.100.4"); ok || why != RejectNode {
		t.Fatalf("名额仍然是满的,dave 应被拒:ok=%v why=%q", ok, why)
	}
	// 容量调小不踢人;调大 / 撤掉后新用户进得来
	spec.Cap = 1
	l.SetNodes(nodeTab(spec))
	if ok, _ := allow("alice", "198.51.100.1"); !ok {
		t.Fatal("调小容量不该挡住已经在线的用户")
	}
	l.SetNodes(nil)
	if ok, _ := l.AllowConnNode("dave", "198.51.100.4", l.NodeFor("in", netip.MustParseAddr("198.51.100.4"))); !ok {
		t.Fatal("撤掉节点设置后不该再拒")
	}
}

// 空用户(内部凭据、无鉴权)不受节点层限制,也不占容量。
func TestNodeCapacityIgnoresAnonymous(t *testing.T) {
	l := NewLimiter()
	spec := NodeSpec{Key: "1:s1", Cap: 1}
	l.SetNodes(nodeTab(spec))
	for i := 0; i < 3; i++ {
		if ok, _ := l.AllowConnNode("", "198.51.100.1", spec); !ok {
			t.Fatal("空用户不该被拒")
		}
	}
	if ok, _ := l.AllowConnNode("alice", "198.51.100.1", spec); !ok {
		t.Fatal("空用户不占容量,alice 应放行")
	}
}

// 节点的两层桶位:节点总限速所有用户共用一对桶,每用户限速每人一对;改速率原地生效(在线连接跟着变),撤掉后桶置空。
func TestNodeSpeedBuckets(t *testing.T) {
	l := NewLimiter()
	spec := NodeSpec{Key: "1:s1", Speed: 100, UserSpeed: 20}
	l.SetNodes(nodeTab(spec))
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c1 := l.wrapConnNode(a, "alice", "198.51.100.1", "", spec).(*limitedConn)
	c2 := l.wrapConnNode(b, "alice", "198.51.100.1", "", spec).(*limitedConn)
	c3 := l.wrapConnNode(b, "bob", "198.51.100.2", "", spec).(*limitedConn)
	if c1.node == nil || c1.node != c3.node {
		t.Fatal("同一个节点上的连接应共用节点总限速的桶位")
	}
	if c1.nodeUser == nil || c1.nodeUser != c2.nodeUser || c1.nodeUser == c3.nodeUser {
		t.Fatal("每用户限速:同一个用户的连接共用一对桶,不同用户各一对")
	}
	rate := func(e *userEntry) (up, down float64) {
		if u := e.up.Load(); u != nil {
			up = float64(u.Limit())
		}
		if d := e.down.Load(); d != nil {
			down = float64(d.Limit())
		}
		return
	}
	if up, down := rate(c1.node); up != 100*125000 || down != 100*125000 {
		t.Fatalf("节点总限速应是 100 Mbps:%v / %v", up, down)
	}
	if up, down := rate(c1.nodeUser); up != 20*125000 || down != 20*125000 {
		t.Fatalf("每用户限速应是 20 Mbps:%v / %v", up, down)
	}
	// 没单独设的节点:不挂节点层的桶
	if c := l.wrapConnNode(a, "alice", "198.51.100.1", "", NodeSpec{}).(*limitedConn); c.node != nil || c.nodeUser != nil {
		t.Fatal("节点什么都没设时不该挂节点层的桶位")
	}
	// 改速率:原来那条连接手里的桶跟着变
	spec.Speed, spec.UserSpeed = 50, 0
	l.SetNodes(nodeTab(spec))
	if up, _ := rate(c1.node); up != 50*125000 {
		t.Fatalf("节点总限速改成 50 后在线连接没跟上:%v", up)
	}
	if up, down := rate(c1.nodeUser); up != 0 || down != 0 {
		t.Fatalf("每用户限速撤掉后桶应置空:%v / %v", up, down)
	}
	// 整个节点的设置撤掉:桶都置空
	l.SetNodes(nil)
	if up, down := rate(c1.node); up != 0 || down != 0 {
		t.Fatalf("节点设置撤掉后总限速的桶应置空:%v / %v", up, down)
	}
}

// 中转入口按来源 IP 认:经入口进来的连接用入口的容量,直连的用直连节点的,互不占名额。
func TestNodeCapacityBySource(t *testing.T) {
	l := NewLimiter()
	relay := netip.MustParseAddr("203.0.113.9")
	l.SetNodes(NodeTable{"in": {
		Direct:  NodeSpec{Key: "1:s1", Cap: 1},
		Sources: map[netip.Addr]NodeSpec{relay: {Key: "1:f1", Cap: 1}},
	}})
	direct := l.NodeFor("in", netip.MustParseAddr("198.51.100.1"))
	via := l.NodeFor("in", relay)
	if direct.Key != "1:s1" || via.Key != "1:f1" {
		t.Fatalf("节点认错了:直连 %+v,经中转 %+v", direct, via)
	}
	if ok, _ := l.AllowConnNode("alice", "198.51.100.1", direct); !ok {
		t.Fatal("alice 直连应放行")
	}
	if ok, _ := l.AllowConnNode("bob", relay.String(), via); !ok {
		t.Fatal("bob 经中转应放行:中转入口有自己的容量")
	}
	if ok, why := l.AllowConnNode("carol", "198.51.100.3", direct); ok || why != RejectNode {
		t.Fatalf("直连节点已满,carol 应被拒:ok=%v why=%q", ok, why)
	}
}
