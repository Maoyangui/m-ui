package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

func TestExternalNodesAppendedToSubscriptions(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)}
	lines := []model.Line{{Name: "本站", Protocol: "hysteria2", Port: 1, Enabled: true}}
	entries := []Entry{{Host: "1.2.3.4"}}
	external := []ExtItem{{
		Name:  "机场A",
		Links: []string{"trojan://pw@9.9.9.9:443?security=tls&sni=t.example.com#%E6%9C%BA%E5%9C%BAA-1"},
		Clash: []map[string]interface{}{
			{"name": "机场A-1", "type": "trojan", "server": "9.9.9.9", "port": 443, "password": "pw", "sni": "t.example.com"},
			{"name": "本站", "type": "ss", "server": "8.8.8.8", "port": 1, "cipher": "aes-128-gcm", "password": "x"}, // 与本站重名 → 自动改名
		},
	}}
	res := BuildLinkSub(user, lines, Options{Entries: entries, External: external})
	if !strings.Contains(res.Body, "hysteria2://") || !strings.Contains(res.Body, "trojan://pw@9.9.9.9:443") {
		t.Fatalf("链接订阅应含本站与外部链接:\n%s", res.Body)
	}
	out, err := BuildClashFull(user, lines, entries, "", "", external)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: 机场A-1", "name: 本站 2", "server: 9.9.9.9"} {
		if !strings.Contains(out, want) {
			t.Fatalf("clash 缺少 %q:\n%s", want, out)
		}
	}
	// 外部节点进 Auto 与 Proxy 组
	auto := out[strings.Index(out, "name: Auto"):]
	if !strings.Contains(auto, "- 机场A-1") || !strings.Contains(auto, "- 本站 2") {
		t.Fatalf("Auto 组应含外部节点:\n%s", auto)
	}
}
