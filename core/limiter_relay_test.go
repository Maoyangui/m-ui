package core

import (
	"net/netip"
	"sort"
	"testing"
	"time"
)

// 中转入口的来源(中转机的地址)不是用户的设备(用户 11-20 定):不占设备名额、不因设备数被拒;
// 用户自己的上限、所属代理池、别的机器上报来的在线 IP 都一样;在线 IP 里照常看得到。
func relayAddrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func TestRelaySourceIsNotADevice(t *testing.T) {
	const relay, relay6 = "198.51.100.7", "2001:db8:7::1"
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"alice": {DeviceLimit: 1}})
	l.SetRelaySources(relayAddrs(relay, relay6))

	// 本人直连在线,再经中转连:以前被拒(中转机算成第二台设备)
	if !l.AllowConn("alice", "1.1.1.1") {
		t.Fatal("本人的设备应放行")
	}
	if ok, why := l.AllowConnWhy("alice", relay); !ok {
		t.Fatalf("本人直连在线时经中转连应放行,却被拒: %s", why)
	}
	if !l.AllowConn("alice", relay6) || !l.AllowConn("alice", "::ffff:"+relay) {
		t.Fatal("中转机的 IPv6、4in6 写法的地址同样放行")
	}
	// 真的第二台设备照样拒
	if l.AllowConn("alice", "2.2.2.2") {
		t.Fatal("限 1 台:本人的第二台设备应被拒,不能因为有中转就放开")
	}
	// 在线 IP 里看得到中转机的地址
	ips := l.ActiveIPs("alice")
	sort.Strings(ips)
	if len(ips) != 4 || ips[0] != "1.1.1.1" {
		t.Fatalf("在线 IP 应是本人的 1 个 + 中转机的 3 个写法,实际 %v", ips)
	}
	// 有流量时的保活:中转机的地址一直放行,本人的设备也不受它影响
	if now := time.Now().Unix(); !l.touch("alice", relay, now) || !l.touch("alice", "1.1.1.1", now) {
		t.Fatal("保活应放行")
	}

	// 反过来:先经中转在线,本人再直连(以前直连被拒)
	l2 := NewLimiter()
	l2.SetLimits(map[string]UserLimitSpec{"bob": {DeviceLimit: 1}})
	l2.SetRelaySources(relayAddrs(relay))
	if !l2.AllowConn("bob", relay) || !l2.AllowConn("bob", "3.3.3.3") {
		t.Fatal("先经中转在线,本人再直连应放行")
	}
	if l2.AllowConn("bob", "4.4.4.4") {
		t.Fatal("本人的第二台设备应被拒")
	}
}

func TestRelaySourceInExternalAndPool(t *testing.T) {
	const relay = "198.51.100.7"
	// 别的机器上报来的在线 IP 里有中转机的地址(那边的出口登记的):不占这边的名额
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"alice": {DeviceLimit: 1}})
	l.SetRelaySources(relayAddrs(relay))
	l.SetExternalIPs(map[string][]string{"alice": {relay}})
	if ok, why := l.AllowConnWhy("alice", "1.1.1.1"); !ok {
		t.Fatalf("别的机器上只有经中转的连接时,本人的设备应放行,却被拒: %s", why)
	}
	// 别的机器上真有一台设备在线时照样拒
	l.SetExternalIPs(map[string][]string{"alice": {relay, "9.9.9.9"}})
	if l.AllowConn("alice", "5.5.5.5") {
		t.Fatal("别的机器上已有一台设备在线(限 1 台):这边的新设备应被拒")
	}

	// 代理的设备池:中转机的地址不占池、不被池拒,池的已用数也不算它
	p := NewLimiter()
	p.SetLimits(map[string]UserLimitSpec{"a": {Group: "r1"}, "b": {Group: "r1"}})
	p.SetGroups(map[string]GroupLimitSpec{"r1": {DeviceLimit: 1}})
	p.SetRelaySources(relayAddrs(relay))
	if !p.AllowConn("a", "1.1.1.1") {
		t.Fatal("池里第一台设备应放行")
	}
	if ok, why := p.AllowConnWhy("b", relay); !ok {
		t.Fatalf("池满时经中转的连接应放行,却被拒: %s", why)
	}
	if ok, why := p.AllowConnWhy("b", "2.2.2.2"); ok || why != RejectPool {
		t.Fatalf("池满(1 台):b 的真设备应被池拒,实际 ok=%v why=%q", ok, why)
	}
	if got := p.GroupState()["r1"].Devices; got != 1 {
		t.Fatalf("池的已用数应为 1(中转机的地址不算),实际 %d", got)
	}
}

// 名单撤掉(入口删了 / 暂停了)以后,那个地址又按普通来源算;名单没变时不重复重算
func TestRelaySourcesReplaced(t *testing.T) {
	const relay = "198.51.100.7"
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"alice": {DeviceLimit: 1}})
	l.SetRelaySources(relayAddrs(relay))
	if !l.AllowConn("alice", "1.1.1.1") || !l.AllowConn("alice", relay) {
		t.Fatal("前提:本人 + 中转都在线")
	}
	gen := l.deviceGen.Load()
	l.SetRelaySources(relayAddrs(relay))
	if l.deviceGen.Load() != gen {
		t.Fatal("名单没变不该让在线连接重新过一遍")
	}
	l.SetRelaySources(nil)
	if l.deviceGen.Load() == gen {
		t.Fatal("名单变了应让在线连接重新过一遍")
	}
	if l.AllowConn("alice", "6.6.6.6") {
		t.Fatal("名单撤掉以后:在线的已有 2 个来源(限 1 台),新设备应被拒")
	}
	// 没有名单时和以前完全一样
	n := NewLimiter()
	n.SetLimits(map[string]UserLimitSpec{"carol": {DeviceLimit: 1}})
	if !n.AllowConn("carol", "1.1.1.1") || n.AllowConn("carol", relay) {
		t.Fatal("没有中转名单时,第二个来源照旧按设备算、被拒")
	}
}
