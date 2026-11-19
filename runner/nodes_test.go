package runner

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database/model"
)

func rawJSON(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 这台(id 2)上的节点表:直连节点、转到这台的中转入口各算各的;别的服务器上的、关着的、暂停的不进表。
func TestNodeTable(t *testing.T) {
	nodes := []model.Node{
		{Id: 1, Name: "主机", PublicIP: "198.51.100.1"},
		{Id: 2, Name: "出口", PublicIP: "198.51.100.2"},
		{Id: 3, Name: "中转", PublicIP: "203.0.113.3", PublicIP6: "2001:db8::3", Addr: "10.0.0.3"},
	}
	forwards := []model.LineForward{
		{Id: 1, Via: 3, Port: 30001, To: 2},                                                            // 集群中转 → 这台,单独设了倍率
		{Id: 2, Via: 0, Host: "203.0.113.50", Port: 30002, To: 2},                                      // 别处的中转 → 这台,没单独设
		{Id: 3, Via: 3, Port: 30003, To: 1},                                                            // 转到别的服务器:和这台无关
		{Id: 4, Via: 0, Host: "relay.example.com", Port: 30004, To: 2, Src: "192.0.2.77,2001:db8::77"}, // 域名认不出,手填了来源
		{Id: 5, Via: 0, Host: "203.0.113.60", Port: 30005, To: 2, Off: true},                           // 暂停的
	}
	opts := map[string]model.NodeOpt{
		"s2": {Ratio: 2, Speed: 100},
		"s1": {Ratio: 9},
		"f1": {Ratio: 5, Cap: 30},
		"f4": {UserSpeed: 20},
		"f5": {Ratio: 7},
	}
	lines := []model.Line{
		{Id: 10, Name: "a", Enabled: true, Forwards: rawJSON(t, forwards), NodeOpts: rawJSON(t, opts)},
		{Id: 11, Name: "只关了输出", Enabled: true, NodeOpts: rawJSON(t, map[string]model.NodeOpt{"s2": {Hidden: true}})},
		{Id: 12, Name: "停用的线路", Enabled: false, NodeOpts: rawJSON(t, map[string]model.NodeOpt{"s2": {Ratio: 3}})},
		{Id: 13, Name: "不在这台", Enabled: true, NodeIds: rawJSON(t, []uint{1}), NodeOpts: rawJSON(t, map[string]model.NodeOpt{"s1": {Ratio: 3}})},
		{Id: 14, Name: "没设置", Enabled: true},
	}
	tb := nodeTable(lines, nodes, 2)
	if len(tb) != 1 {
		t.Fatalf("只有线路 a 在这台上有单独设置的节点,实际表里有 %d 个入站:%+v", len(tb), tb)
	}
	at := func(src string) core.NodeSpec { return tb.Lookup("a", netip.MustParseAddr(src)) }
	if got := at("8.8.8.8"); got != (core.NodeSpec{Key: "10:s2", Ratio: 2, Speed: 100}) {
		t.Errorf("直连:%+v", got)
	}
	for _, src := range []string{"203.0.113.3", "2001:db8::3", "10.0.0.3"} {
		if got := at(src); got != (core.NodeSpec{Key: "10:f1", Ratio: 5, Cap: 30}) {
			t.Errorf("集群中转的来源 %s 应认成入口 1:%+v", src, got)
		}
	}
	if got := at("203.0.113.50"); got != (core.NodeSpec{}) {
		t.Errorf("没单独设的入口:跟随服务器,不能套直连节点的倍率:%+v", got)
	}
	for _, src := range []string{"192.0.2.77", "2001:db8::77"} {
		if got := at(src); got != (core.NodeSpec{Key: "10:f4", UserSpeed: 20}) {
			t.Errorf("手填的来源 %s 应认成入口 4:%+v", src, got)
		}
	}
	if got := at("203.0.113.60"); got.Key != "10:s2" {
		t.Errorf("暂停的入口不进表,它的来源按直连算:%+v", got)
	}
	// 在主机(id 1)上看:线路 a 的直连 s1 和转到主机的入口 3
	tb1 := nodeTable(lines, nodes, 1)
	if got := tb1.Lookup("a", netip.MustParseAddr("8.8.8.8")); got.Key != "10:s1" || got.Ratio != 9 {
		t.Errorf("主机上的直连:%+v", got)
	}
	if got := tb1.Lookup("a", netip.MustParseAddr("203.0.113.3")); got != (core.NodeSpec{}) {
		t.Errorf("主机上经入口 3 进来的(没单独设):%+v", got)
	}
	if got := tb1.Lookup("不在这台", netip.MustParseAddr("8.8.8.8")); got.Ratio != 3 {
		t.Errorf("线路「不在这台」部署在主机上:%+v", got)
	}
	// 没有任何节点单独设置:空表
	if tb := nodeTable([]model.Line{{Id: 1, Name: "x", Enabled: true}}, nodes, 2); len(tb) != 0 {
		t.Errorf("应是空表:%+v", tb)
	}
}

func TestNormForwardSrc(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{" 192.0.2.1 ", "192.0.2.1"},
		{"192.0.2.1,192.0.2.1 2001:db8::1", "192.0.2.1,2001:db8::1"},
		{"::ffff:192.0.2.9、[2001:db8::2]", "192.0.2.9,2001:db8::2"},
	} {
		got, err := model.NormForwardSrc(c.in)
		if err != nil || got != c.want {
			t.Errorf("NormForwardSrc(%q) = %q, %v;应是 %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"relay.example.com", "192.0.2.1/24", "1,2,3", "192.0.2.1,192.0.2.2,192.0.2.3,192.0.2.4,192.0.2.5,192.0.2.6,192.0.2.7,192.0.2.8,192.0.2.9"} {
		if _, err := model.NormForwardSrc(bad); err == nil {
			t.Errorf("NormForwardSrc(%q) 应该报错", bad)
		}
	}
}

// 全集群中转入口的来源地址(设备数不算这些):手填的来源、别处的中转填的地址、集群里当中转的那台的公网地址和连接地址;
// 不分转到哪一台;停用的线路、暂停的入口不算;域名不算。
func TestRelaySources(t *testing.T) {
	nodes := []model.Node{
		{Id: 1, PublicIP: "203.0.113.1"},
		{Id: 2, PublicIP: "198.51.100.2", PublicIP6: "2001:db8:2::1", Addr: "relay.example.net"},
		{Id: 3, PublicIP: "198.51.100.3", Addr: "[2001:db8:3::9]"},
		{Id: 4, PublicIP: "198.51.100.4"},
	}
	fw := func(v interface{}) json.RawMessage { b, _ := json.Marshal(v); return b }
	lines := []model.Line{
		{Id: 1, Name: "A", Enabled: true, Forwards: fw([]model.LineForward{
			{Id: 1, Via: 2, Port: 30001, To: 1},
			{Id: 2, Via: 0, Host: "192.0.2.50", Port: 30002, To: 1, Src: "192.0.2.51, 192.0.2.52"},
			{Id: 3, Via: 4, Port: 30003, To: 1, Off: true},                 // 暂停的
			{Id: 4, Via: 0, Host: "relay.example.org", Port: 30004, To: 1}, // 域名:认不出来源
		})},
		{Id: 2, Name: "B", Enabled: true, Forwards: fw([]model.LineForward{{Id: 1, Via: 3, Port: 30005, To: 2}})},  // 转到别的机器的也算
		{Id: 3, Name: "C", Enabled: false, Forwards: fw([]model.LineForward{{Id: 1, Via: 4, Port: 30006, To: 1}})}, // 停用的线路
	}
	got := map[string]bool{}
	for _, a := range relaySources(lines, nodes) {
		got[a.String()] = true
	}
	want := []string{"198.51.100.2", "2001:db8:2::1", "192.0.2.50", "192.0.2.51", "192.0.2.52", "198.51.100.3", "2001:db8:3::9"}
	if len(got) != len(want) {
		t.Fatalf("应有 %d 个来源,实际 %v", len(want), got)
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("少了 %s: %v", w, got)
		}
	}
	if got["198.51.100.4"] {
		t.Fatalf("暂停的入口、停用的线路上的中转机不该算: %v", got)
	}
}
