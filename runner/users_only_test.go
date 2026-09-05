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
