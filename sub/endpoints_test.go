package sub

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

func epStr(eps []Endpoint) []string {
	out := make([]string, 0, len(eps))
	for _, e := range eps {
		relay := ""
		if e.Relay {
			relay = " relay"
		}
		out = append(out, fmt.Sprintf("%s|%s|%s:%d|n%d%s", e.Key, e.Label, e.Server, e.Port, e.NodeId, relay))
	}
	return out
}

func sameStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 「测大陆连通」测的就是订阅里给用户的那些地址:部署到的每台服务器一个(线路单独选了 IP 版本的按选的那个地址),
// 接着是中转入口(地址是中转的、端口是入口的);没部署到的服务器、暂停的入口、线路上关了输出的节点都不在里面。
func TestLineEndpointsFollowSubscription(t *testing.T) {
	entries := []Entry{
		{Name: "东京", Host: "203.0.113.10", NodeId: 1, Suffix: "-东京", IP4: "203.0.113.10", IP6: "2001:db8::10"},
		{Name: "香港", Host: "198.51.100.20", NodeId: 2, Suffix: "-香港 x2", Ratio: 2, IP4: "198.51.100.20"},
		{Name: "新加坡", Host: "192.0.2.30", NodeId: 3, Suffix: "-新加坡"},
	}
	line := model.Line{Id: 7, Name: "L", Protocol: "vless", Port: 443, Enabled: true,
		NodeIds:    json.RawMessage(`[1,2]`),
		NodeFamily: json.RawMessage(`{"1":"v6"}`),
		Forwards: json.RawMessage(`[{"id":1,"via":3,"port":20443,"to":2},{"id":2,"name":"广州入口","host":"gz.example.com","port":30443,"to":1},
			{"id":3,"host":"off.example.com","port":40443,"to":1,"off":true}]`),
	}
	got := epStr(lineEndpoints(line, entries))
	want := []string{
		"s1|东京|2001:db8::10:443|n1",              // 这条线路在东京上选了 IPv6
		"s2|香港 x2|198.51.100.20:443|n2",          // 节点名后缀里的倍率照订阅里的样子带着
		"f1|香港-新加坡 x2|192.0.2.30:20443|n0 relay", // 集群里的中转:地址是中转那台的,端口是入口的
		"f2|东京-广州入口|gz.example.com:30443|n0 relay",
	}
	if !sameStrs(got, want) {
		t.Fatalf("地址不对:\n got %q\nwant %q", got, want)
	}

	// 线路上关了输出的节点不测(订阅里没有它)
	line.NodeOpts = json.RawMessage(`{"s2":{"hidden":true},"f2":{"hidden":true}}`)
	got = epStr(lineEndpoints(line, entries))
	want = []string{"s1|东京|2001:db8::10:443|n1", "f1|香港-新加坡 x2|192.0.2.30:20443|n0 relay"}
	if !sameStrs(got, want) {
		t.Fatalf("关了输出的节点不该在里面:\n got %q\nwant %q", got, want)
	}
}

// 只有一台服务器时订阅里的节点名没有后缀:这里用服务器名;自定义地址用它的备注,没写备注的就用地址本身;端口没填的用线路端口。
func TestLineEndpointsLabels(t *testing.T) {
	one := []Entry{{Name: "本机", Host: "203.0.113.10", NodeId: 1}}
	line := model.Line{Id: 1, Name: "L", Protocol: "trojan", Port: 8443, Enabled: true}
	if got, want := epStr(lineEndpoints(line, one)), []string{"s1|本机|203.0.113.10:8443|n1"}; !sameStrs(got, want) {
		t.Fatalf("单台服务器: got %q want %q", got, want)
	}
	line.Addrs = json.RawMessage(`[{"server":"cdn.example.com","server_port":2053,"remark":"-优选"},{"server":"[2001:db8::7]"},{"server":"198.51.100.9","node_id":1}]`)
	got := epStr(lineEndpoints(line, one))
	want := []string{"|优选|cdn.example.com:2053|n0", "|2001:db8::7|2001:db8::7:8443|n0", "s1|本机|198.51.100.9:8443|n1"}
	if !sameStrs(got, want) {
		t.Fatalf("自定义地址:\n got %q\nwant %q", got, want)
	}
}
