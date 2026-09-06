package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

func TestPasswordEscapedInURIs(t *testing.T) {
	u := model.User{Name: "x", Credentials: []byte(`{"hysteria2":{"password":"p@ss/w?d#1"},"anytls":{"password":"a@b"}}`)}
	a := addr{server: "1.2.3.4", port: 443}
	h := hysteria2URI(model.Line{Protocol: "hysteria2"}, u, a, "r")
	if !strings.HasPrefix(h, "hysteria2://p%40ss%2Fw%3Fd%231@1.2.3.4:443?") {
		t.Fatalf("hysteria2 密码应按 userinfo 规则转义: %s", h)
	}
	if got := anytlsURI(u, a, "r"); !strings.HasPrefix(got, "anytls://a%40b@1.2.3.4:443?") {
		t.Fatalf("anytls 密码应转义: %s", got)
	}
	// 字母数字原样不动(与旧面板字节级一致)
	if escapeUserinfo("qJIq2Qzhug") != "qJIq2Qzhug" {
		t.Fatal("普通密码不该被改动")
	}
}

func TestCustomAddrsHonourNodeNarrowing(t *testing.T) {
	line := model.Line{Id: 1, Port: 443, NodeIds: []byte(`[1,2]`),
		Addrs: []byte(`[{"server":"a.example","node_id":1},{"server":"b.example","node_id":2},{"server":"any.example"}]`)}
	entries := []Entry{{Name: "A", Host: "1.1.1.1", NodeId: 1}, {Name: "B", Host: "2.2.2.2", NodeId: 2}}
	all := resolveAddrs(line, entries, nil)
	if len(all) != 3 {
		t.Fatalf("没有收窄时三条地址都给: %+v", all)
	}
	only2 := resolveAddrs(line, entries, map[uint]bool{2: true})
	if len(only2) != 2 || only2[0].server != "b.example" || only2[1].server != "any.example" {
		t.Fatalf("收窄到 2 号服务器后应只剩它的地址和不属于任何服务器的地址: %+v", only2)
	}
}
