package runner

import "testing"

// 两份配置只在入站 users 上不同 → 只热换用户,不重启数据面;别的地方也动了 → 必须重启。
func TestOnlyUsersDiffer(t *testing.T) {
	base := `{"log":{"level":"info"},"inbounds":[{"type":"hysteria2","tag":"hk","listen_port":30443,"users":[{"name":"a","password":"1"}]},{"type":"anytls","tag":"jp","listen_port":30444,"users":[{"name":"a","password":"1"}]}],"outbounds":[{"type":"direct","tag":"direct"}]}`
	usersOnly := `{"log":{"level":"info"},"inbounds":[{"type":"hysteria2","tag":"hk","listen_port":30443,"users":[{"name":"a","password":"1"},{"name":"a#share","password":"2"}]},{"type":"anytls","tag":"jp","listen_port":30444,"users":[]}],"outbounds":[{"type":"direct","tag":"direct"}]}`
	portChanged := `{"log":{"level":"info"},"inbounds":[{"type":"hysteria2","tag":"hk","listen_port":30445,"users":[{"name":"a","password":"1"}]},{"type":"anytls","tag":"jp","listen_port":30444,"users":[{"name":"a","password":"1"}]}],"outbounds":[{"type":"direct","tag":"direct"}]}`
	outboundChanged := `{"log":{"level":"info"},"inbounds":[{"type":"hysteria2","tag":"hk","listen_port":30443,"users":[{"name":"a","password":"1"}]},{"type":"anytls","tag":"jp","listen_port":30444,"users":[{"name":"a","password":"1"}]}],"outbounds":[{"type":"direct","tag":"direct"},{"type":"socks","tag":"warp","server":"127.0.0.1","server_port":40000}]}`
	inboundAdded := `{"log":{"level":"info"},"inbounds":[{"type":"hysteria2","tag":"hk","listen_port":30443,"users":[{"name":"a","password":"1"}]}],"outbounds":[{"type":"direct","tag":"direct"}]}`

	if !onlyUsersDiffer([]byte(base), []byte(usersOnly)) {
		t.Fatal("只有用户表不同应判为 true")
	}
	if onlyUsersDiffer([]byte(base), []byte(base)) {
		t.Fatal("完全相同不算\"只有用户不同\"(调用方另有无变化分支)")
	}
	for name, next := range map[string]string{"端口变了": portChanged, "出站变了": outboundChanged, "少了一个入站": inboundAdded, "不是 JSON": "{"} {
		if onlyUsersDiffer([]byte(base), []byte(next)) {
			t.Fatalf("%s:不该判为只有用户不同", name)
		}
	}
}

// 上游热更新能兜住的范围:只差出站和 / 或入站用户表;线路、路由一动就不行。
func TestSameExceptOutboundsUsers(t *testing.T) {
	base := `{"inbounds":[{"tag":"a","type":"socks","listen_port":1,"users":[{"username":"x","password":"1"}]}],"outbounds":[{"type":"direct","tag":"direct"},{"type":"socks","tag":"up","server_port":1}],"route":{"rules":[{"inbound":["a"],"outbound":"up"}]}}`
	both := `{"inbounds":[{"tag":"a","type":"socks","listen_port":1,"users":[]}],"outbounds":[{"type":"direct","tag":"direct"},{"type":"socks","tag":"up","server_port":2}],"route":{"rules":[{"inbound":["a"],"outbound":"up"}]}}`
	port := `{"inbounds":[{"tag":"a","type":"socks","listen_port":9,"users":[{"username":"x","password":"1"}]}],"outbounds":[{"type":"direct","tag":"direct"},{"type":"socks","tag":"up","server_port":2}],"route":{"rules":[{"inbound":["a"],"outbound":"up"}]}}`
	route := `{"inbounds":[{"tag":"a","type":"socks","listen_port":1,"users":[{"username":"x","password":"1"}]}],"outbounds":[{"type":"direct","tag":"direct"},{"type":"socks","tag":"up","server_port":1}],"route":{"rules":[{"inbound":["a"],"outbound":"direct"}]}}`
	if !sameExceptOutboundsUsers([]byte(base), []byte(both)) || !sameExceptOutboundsUsers([]byte(base), []byte(base)) {
		t.Fatal("只差出站与用户表(或完全相同)应判为 true")
	}
	for name, next := range map[string]string{"线路端口变了": port, "路由变了": route, "不是 JSON": "{"} {
		if sameExceptOutboundsUsers([]byte(base), []byte(next)) {
			t.Fatalf("%s:热换出站兜不住,不该判为 true", name)
		}
	}
	// 热换出站之后、换用户表之前在跑的样子:旧配置 + 新出站
	running := withOutboundsOf([]byte(base), []byte(both))
	if !sameExceptOutboundsUsers(running, []byte(base)) || !onlyUsersDiffer(running, []byte(both)) {
		t.Fatalf("应是旧配置换上新出站(用户表仍是旧的): %s", running)
	}
}
