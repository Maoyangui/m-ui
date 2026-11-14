package sub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"gopkg.in/yaml.v3"
)

// 线路自定义地址把 IPv6 写成 [v6](保存规整之前的老数据):分享链接、Clash、sing-box 三种格式都要是同一个地址,
// 不能出现 [[v6]],sing-box 订阅里也不能把这个节点丢掉(以前解析失败被静默跳过)。
func TestCustomV6AddrLinksClashSingBox(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"trojan":{"password":"p"},"hysteria2":{"password":"p"}}`)}
	lines := []model.Line{
		{Id: 1, Name: "old6", Protocol: "trojan", Port: 13443, Enabled: true, Tls: []byte(`{"mode":"cert"}`), Addrs: []byte(`[{"server":" [2001:db8::99] "}]`)},
		{Id: 2, Name: "new6", Protocol: "hysteria2", Port: 13444, Enabled: true, Addrs: []byte(`[{"server":"2001:db8::98","server_port":23444}]`)},
	}
	entries := []Entry{{Name: "a", Host: "203.0.113.1", SNI: "a.example.com", IP4: "203.0.113.1", NodeId: 1}}

	links := strings.Join(GenerateLinks(user, lines, entries), "\n")
	if strings.Contains(links, "[[") || !strings.Contains(links, "@[2001:db8::99]:13443") || !strings.Contains(links, "@[2001:db8::98]:23444") {
		t.Fatalf("分享链接:\n%s", links)
	}

	y, err := BuildClash(user, lines, entries, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		IPv6    bool `yaml:"ipv6"`
		Proxies []struct {
			Name   string `yaml:"name"`
			Server string `yaml:"server"`
			Port   int    `yaml:"port"`
		} `yaml:"proxies"`
	}
	if err := yaml.Unmarshal([]byte(y), &cfg); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range cfg.Proxies {
		got[p.Name] = p.Server
	}
	if got["old6"] != "2001:db8::99" || got["new6"] != "2001:db8::98" || !cfg.IPv6 {
		t.Fatalf("Clash 的 server 要是裸 IPv6 并打开 ipv6: %v ipv6=%v", got, cfg.IPv6)
	}

	res, err := BuildSingBoxSub(user, lines, Options{Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	var sb struct {
		Outbounds []map[string]interface{} `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(res.Body), &sb); err != nil {
		t.Fatal(err)
	}
	servers := map[string]interface{}{}
	for _, o := range sb.Outbounds {
		if tag, _ := o["tag"].(string); o["server"] != nil {
			servers[tag] = o["server"]
		}
	}
	if servers["old6"] != "2001:db8::99" || servers["new6"] != "2001:db8::98" {
		t.Fatalf("sing-box 里两个节点都要在、地址是裸 IPv6: %v", servers)
	}
}

// 本站线路的链接 sing-box 解析不了时(自定义地址写坏的老数据)要打警告,不再静默丢节点;日志里不能带链接(里面有密码),
// 同一个节点一小时只记一次。
func TestSingBoxWarnsBadOwnLink(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"trojan":{"password":"secret-pw"}}`)}
	lines := []model.Line{{Id: 7, Name: "坏地址", Protocol: "trojan", Port: 443, Enabled: true, Addrs: []byte(`[{"server":"bad%zz"}]`)}}
	badLinkMu.Lock()
	badLinkWarned = map[string]time.Time{}
	badLinkMu.Unlock()
	res, err := BuildSingBoxSub(user, lines, Options{Entries: hkEntry, External: []ExtItem{{Name: "ext", Links: []string{"bogus://x#ext-bad"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Body, "坏地址") {
		t.Fatalf("解析不了的节点写不进配置: %s", res.Body)
	}
	badLinkMu.Lock()
	defer badLinkMu.Unlock()
	if _, ok := badLinkWarned["trojan|坏地址"]; !ok || len(badLinkWarned) != 1 {
		t.Fatalf("本站节点要记一次警告、外部节点不记: %v", badLinkWarned)
	}
}
