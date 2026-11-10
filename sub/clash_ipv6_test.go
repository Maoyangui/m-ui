package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"

	"gopkg.in/yaml.v3"
)

// 自定义 Clash 模板写了 ipv6: false(照抄内置骨架时常见):有 IPv6 节点时要改成 true,不然 mihomo 连不上这些节点;
// 没有 IPv6 节点时模板写什么就是什么。设置页据 ClashTemplateDisablesIPv6 提示。
func TestClashTemplateIPv6FalseWithV6Nodes(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"trojan":{"password":"p"}}`)}
	v6 := model.Line{Id: 1, Name: "l", Protocol: "trojan", Port: 443, Tls: []byte(`{"mode":"cert"}`), NodeFamily: []byte(`{"1":"v6"}`)}
	entries := []Entry{{Name: "a", Host: "203.0.113.1", IP4: "203.0.113.1", IP6: "2001:db8::a", NodeId: 1}}
	tmpl := "mixed-port: 7890\nipv6: false\nmode: rule\nrules:\n  - MATCH,Proxy\n"
	ipv6Of := func(y string) interface{} {
		t.Helper()
		var root map[string]interface{}
		if err := yaml.Unmarshal([]byte(y), &root); err != nil {
			t.Fatal(err)
		}
		return root["ipv6"]
	}
	y, err := BuildClashFor(user, []model.Line{v6}, entries, tmpl, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ipv6Of(y) != true || !strings.Contains(y, "2001:db8::a") {
		t.Fatalf("有 IPv6 节点时模板里的 ipv6: false 要改成 true:\n%s", y)
	}
	v4 := v6
	v4.NodeFamily = nil
	if y, _ = BuildClashFor(user, []model.Line{v4}, entries, tmpl, "", nil, nil); ipv6Of(y) != false {
		t.Fatalf("没有 IPv6 节点时照模板:\n%s", y)
	}
	// 外部节点里的 IPv6 地址同样算
	ext := []ExtItem{{Name: "e", Clash: []map[string]interface{}{{"name": "ext6", "type": "ss", "server": "2001:db8::e", "port": 8388, "cipher": "aes-128-gcm", "password": "x"}}}}
	if y, _ = BuildClashFor(user, []model.Line{v4}, entries, tmpl, "", ext, nil); ipv6Of(y) != true {
		t.Fatalf("外部节点是 IPv6 也要打开:\n%s", y)
	}

	for in, want := range map[string]bool{
		tmpl: true, "ipv6: true\n": false, "mode: rule\n": false, "": false, "dns:\n  ipv6: false\n": false, "ipv6: [": false,
	} {
		if got := ClashTemplateDisablesIPv6(in); got != want {
			t.Errorf("ClashTemplateDisablesIPv6(%q) = %v,应为 %v", in, got, want)
		}
	}
}
