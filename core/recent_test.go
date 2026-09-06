package core

import "testing"

func TestRecentConnsAggregate(t *testing.T) {
	c := NewConnTracker(nil)
	c.noteRecent("39.144.90.180", "LMEC", "美国4", "anytls")
	c.noteRecent("1.2.3.4", "alice", "香港1", "hysteria2")
	c.noteRecent("1.2.3.4", "alice", "香港1", "hysteria2") // 同 IP 同线路 → 计数累加
	c.noteRecent("5.6.7.8", "", "台湾2", "tuic")           // 没认出用户
	got := map[string]RecentConn{}
	for _, r := range c.Recent(50) {
		got[r.IP+"|"+r.Inbound] = r
	}
	if len(got) != 3 {
		t.Fatalf("应聚合成 3 条: %+v", got)
	}
	if r := got["1.2.3.4|香港1"]; r.Count != 2 || r.User != "alice" || r.Protocol != "hysteria2" {
		t.Fatalf("香港1: %+v", r)
	}
	if r := got["5.6.7.8|台湾2"]; r.User != "" {
		t.Fatalf("没有用户时不该乱认: %+v", r)
	}
	if c.Recent(1)[0].IP != "5.6.7.8" {
		t.Fatal("应按最近在前")
	}
	for i := 0; i < recentCap+20; i++ {
		c.noteRecent("10.0.0."+string(rune('a'+i%26))+string(rune('a'+i/26)), "", "x", "vless")
	}
	if n := len(c.Recent(0)); n > recentCap {
		t.Fatalf("应有上限,实际 %d", n)
	}
}
