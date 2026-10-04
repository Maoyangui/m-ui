package sub

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gopkg.in/yaml.v3"
)

// 地址族选项:双栈默认 IPv4;选 IPv6 用 v6;选了 IPv6 却没有就用 IPv4;纯 IPv6 机器只能用 v6;手填照旧优先。
// v6 地址进分享链接要加方括号,Clash 配置要能被解析回原地址。
func TestEntriesAddrFamily(t *testing.T) {
	db, _ := database.Open(filepath.Join(t.TempDir(), "x.db"))
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", Domain: "a.example.com", IsLocal: true, Enabled: true, Sort: 1, AddrFamily: "v6"})
	db.Create(&model.Node{Name: "双栈", Domain: "b.example.com", PublicIP: "203.0.113.2", PublicIP6: "2001:db8::2", Enabled: true, Sort: 2})
	db.Create(&model.Node{Name: "选v6", Domain: "c.example.com", PublicIP: "203.0.113.3", PublicIP6: "2001:db8::3", AddrFamily: "v6", Enabled: true, Sort: 3})
	db.Create(&model.Node{Name: "选v6没有", Domain: "d.example.com", PublicIP: "203.0.113.4", AddrFamily: "v6", Enabled: true, Sort: 4})
	db.Create(&model.Node{Name: "纯v6", Domain: "e.example.com", PublicIP: "2001:db8::5", PublicIP6: "2001:db8::5", Enabled: true, Sort: 5})
	db.Create(&model.Node{Name: "手填", Domain: "f.example.com", Addr: "198.51.100.6", PublicIP: "203.0.113.6", PublicIP6: "2001:db8::6", AddrFamily: "v6", Enabled: true, Sort: 6})
	db.Create(&model.Node{Name: "手填v6", Domain: "g.example.com", Addr: "[2001:db8::7]", Enabled: true, Sort: 7})

	e := EntriesFromNodes(db, "a.example.com", "203.0.113.1", "2001:db8::1", true)
	want := []string{"2001:db8::1", "203.0.113.2", "2001:db8::3", "203.0.113.4", "2001:db8::5", "198.51.100.6", "2001:db8::7"}
	if len(e) != len(want) {
		t.Fatalf("应有 %d 个入口: %+v", len(want), e)
	}
	for i, w := range want {
		if e[i].Host != w {
			t.Errorf("入口 %s 的地址应为 %s,得到 %s", e[i].Name, w, e[i].Host)
		}
		if !strings.HasSuffix(e[i].SNI, ".example.com") {
			t.Errorf("入口 %s 的 SNI 应仍是域名: %q", e[i].Name, e[i].SNI)
		}
	}

	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"},"trojan":{"password":"p"}}`)}
	lines := []model.Line{
		{Name: "hy", Protocol: "hysteria2", Port: 443, Enabled: true},
		{Name: "tj", Protocol: "trojan", Port: 8443, Enabled: true},
	}
	links := strings.Join(GenerateLinks(user, lines, e), "\n")
	for _, w := range []string{"@[2001:db8::3]:443", "@[2001:db8::3]:8443", "@203.0.113.4:443", "@[2001:db8::7]:443"} {
		if !strings.Contains(links, w) {
			t.Errorf("分享链接里应有 %s:\n%s", w, links)
		}
	}
	out, err := BuildClash(user, lines, e, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Proxies []struct {
			Name   string `yaml:"name"`
			Server string `yaml:"server"`
		} `yaml:"proxies"`
	}
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("Clash 配置解析失败: %v", err)
	}
	got := map[string]string{}
	for _, p := range cfg.Proxies {
		got[p.Name] = p.Server
	}
	if got["hy-选v6"] != "2001:db8::3" || got["tj-纯v6"] != "2001:db8::5" || got["hy-双栈"] != "203.0.113.2" {
		t.Fatalf("Clash 里的 server 不对: %v", got)
	}
}
