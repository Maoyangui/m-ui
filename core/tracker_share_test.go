package core

import (
	"net"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 临时共享的凭据在数据面里叫 "alice#share":记账(连接数、在线 IP、踢下线)归到 alice,
// 但取消共享时只断 "alice#share" 的连接,alice 自己的连接不动。
func TestShareConnectionsAccountedToOwnerButClosedSeparately(t *testing.T) {
	c := NewConnTracker(nil)
	mk := func(id, user, ip string) net.Conn {
		a, b := net.Pipe()
		go func() { _, _ = b.Read(make([]byte, 1)) }()
		c.trackConnection(id, &ConnectionInfo{ID: id, Conn: a, Inbound: "hy2", User: user, SourceIP: ip, Type: "tcp"})
		return a
	}
	mk("1", "alice", "10.0.0.1")
	mk("2", "alice", "10.0.0.2")
	mk("3", "alice"+model.ShareSuffix, "10.0.0.9")
	mk("4", "bob", "10.0.0.3")

	if got := c.ConnCountByUser(); got["alice"] != 3 || got["bob"] != 1 || got["alice"+model.ShareSuffix] != 0 {
		t.Fatalf("连接数应归到本人:%v", got)
	}
	if ips := c.IPLinesByUser(); len(ips["alice"]) != 3 || ips["alice"+model.ShareSuffix] != nil {
		t.Fatalf("在线 IP 应归到本人:%v", ips)
	}

	// 取消共享:只断借用者
	if n := c.CloseConnByDataPlaneName("alice" + model.ShareSuffix); n != 1 {
		t.Fatalf("应只断 1 条共享连接,实际 %d", n)
	}
	if got := c.ConnCountByUser(); got["alice"] != 2 || got["bob"] != 1 {
		t.Fatalf("本人连接不该受影响:%v", got)
	}

	// 热更新用户表:表里只剩 alice 与 bob 时,残留的共享连接也会被自动断掉
	mk("5", "alice"+model.ShareSuffix, "10.0.0.9")
	if n := c.CloseConnsNotIn(map[string]map[string]struct{}{"hy2": {"alice": {}, "bob": {}}}); n != 1 {
		t.Fatalf("按用户表断连应只断共享连接,实际 %d", n)
	}

	// 面板踢下线:本人和借用者一起断
	mk("6", "alice"+model.ShareSuffix, "10.0.0.9")
	if n := c.CloseConnByUser("alice"); n != 3 {
		t.Fatalf("踢下线应断本人 2 条 + 共享 1 条,实际 %d", n)
	}
	if got := c.ConnCountByUser(); got["alice"] != 0 || got["bob"] != 1 {
		t.Fatalf("踢完只剩 bob:%v", got)
	}
}

func TestOwnerStripsShareSuffixOnly(t *testing.T) {
	for in, want := range map[string]string{"alice": "alice", "alice#share": "alice", "#share": "#share", "bob#shared": "bob#shared", "": ""} {
		if got := model.Owner(in); got != want {
			t.Fatalf("Owner(%q) = %q, want %q", in, got, want)
		}
	}
}
