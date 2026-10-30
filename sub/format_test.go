package sub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"gopkg.in/yaml.v3"
)

// 真实客户端的 UA:把通用地址粘进去时要拿到它能用的格式。
func TestFormatForUA(t *testing.T) {
	cases := []struct {
		ua     string
		format string
		legacy bool
	}{
		{"clash-verge/v2.3.1", "clash", false},
		{"clash.meta/v1.19.0", "clash", false},
		{"ClashMetaForAndroid/2.11.5.Meta", "clash", false},
		{"ClashX Meta/v1.4.0 (com.metacubex.ClashX; build:1)", "clash", false},
		{"mihomo.party/v1.7.6 (clash.meta)", "clash", false},
		{"FlClash/v0.8.90 clash-verge Platform/android", "clash", false},
		{"Stash/2.7.0 Clash/1.9.0", "clash", false},
		{"clash-nyanpasu/v1.6.1", "clash", false},
		{"ClashforWindows/0.20.39", "clash", true},
		{"ClashForAndroid/2.5.12", "clash", true},
		{"ClashX/1.118.0 (com.west2online.ClashX; build:1)", "clash", true},
		{"SFA/1.14.2 (34; sing-box 1.14.2; language zh_CN)", "json", false},
		{"SFI/1.14.2 (26.0; sing-box 1.14.2; language zh_CN)", "json", false},
		{"sing-box 1.14.2", "json", false},
		{"Shadowrocket/2070 CFNetwork/1568.100.1 Darwin/24.0.0", "", false},
		{"Nextin/1.2 CFNetwork", "", false},
		{"v2rayN/7.8.3", "", false},
		{"v2rayNG/1.9.30", "", false},
		{"NekoBox/Android/1.3.3 (Prefer ClashMeta Format)", "", false},
		{"HiddifyNext/2.5.7 (android) like ClashMeta v2ray sing-box", "", false},
		{"karing/1.0.30 (android) like ClashMeta v2rayNG", "", false},
		{"Quantumult%20X/1.5.0", "", false},
		{"Loon/800 CFNetwork", "", false},
		{"curl/8.5.0", "", false},
		{"", "", false},
		{"Go-http-client/1.1", "", false},
		{"godusevpn/0.7.15", "", false},
	}
	for _, c := range cases {
		f, legacy := FormatForUA(c.ua)
		if f != c.format || legacy != c.legacy {
			t.Errorf("%q: 实际 (%q, %v),应为 (%q, %v)", c.ua, f, legacy, c.format, c.legacy)
		}
	}
}

// 老内核 Clash 不认识的协议不发:一个不认识的协议就会让整份配置导入失败。
func TestLegacyClashDropsUnknownProtocols(t *testing.T) {
	lines := []model.Line{
		{Id: 1, Name: "hy2", Protocol: "hysteria2", Port: 1, Enabled: true},
		{Id: 2, Name: "tj", Protocol: "trojan", Port: 2, Enabled: true},
		{Id: 3, Name: "vl", Protocol: "vless", Port: 3, Enabled: true},
	}
	user := model.User{Name: "a", Credentials: []byte(`{"hysteria2":{"password":"p"},"trojan":{"password":"p"},"vless":{"uuid":"u"}}`)}
	opt := Options{Entries: []Entry{{Host: "1.2.3.4"}}, LegacyClash: true, External: []ExtItem{{Clash: []map[string]interface{}{
		{"name": "ext-hy2", "type": "hysteria2", "server": "x", "port": 1},
		{"name": "ext-ss", "type": "ss", "server": "x", "port": 2, "cipher": "aes-128-gcm", "password": "p"},
	}}}}
	res, err := BuildClashSub(user, lines, opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"hysteria2", "vless", "ext-hy2"} {
		if strings.Contains(res.Body, bad) {
			t.Fatalf("老内核 Clash 不该拿到 %s:\n%s", bad, res.Body)
		}
	}
	for _, good := range []string{"tj", "ext-ss"} {
		if !strings.Contains(res.Body, good) {
			t.Fatalf("%s 不该被过滤:\n%s", good, res.Body)
		}
	}
	opt.LegacyClash = false
	res, _ = BuildClashSub(user, lines, opt)
	if !strings.Contains(res.Body, "hysteria2") {
		t.Fatalf("新内核要拿到全部协议:\n%s", res.Body)
	}
}

// Clash 模板:模板写的规则、自定义策略组、自己的节点都留着;节点和 Proxy / Auto 组由面板填进去。
func TestClashTemplateMerge(t *testing.T) {
	tmpl := `mixed-port: 7777
proxies:
  - {name: 模板节点, type: ss, server: 1.1.1.1, port: 1, cipher: aes-128-gcm, password: x}
proxy-groups:
  - {name: 流媒体, type: select, proxies: [Proxy, DIRECT]}
  - {name: Auto, type: fallback, proxies: [模板节点]}
rules:
  - DOMAIN-SUFFIX,netflix.com,流媒体
  - MATCH,Proxy
`
	lines := []model.Line{{Id: 1, Name: "tj", Protocol: "trojan", Port: 2, Enabled: true}}
	user := model.User{Name: "a", Credentials: []byte(`{"trojan":{"password":"p"}}`)}
	body, err := BuildClash(user, lines, []Entry{{Host: "1.2.3.4"}}, tmpl, "")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MixedPort int                      `yaml:"mixed-port"`
		Proxies   []map[string]interface{} `yaml:"proxies"`
		Groups    []map[string]interface{} `yaml:"proxy-groups"`
		Rules     []string                 `yaml:"rules"`
	}
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("不是合法 YAML: %v\n%s", err, body)
	}
	if doc.MixedPort != 7777 || len(doc.Rules) != 2 || doc.Rules[0] != "DOMAIN-SUFFIX,netflix.com,流媒体" {
		t.Fatalf("模板内容丢了:\n%s", body)
	}
	if len(doc.Proxies) != 2 || doc.Proxies[0]["name"] != "模板节点" || doc.Proxies[1]["name"] != "tj" {
		t.Fatalf("节点不对: %v", doc.Proxies)
	}
	names := map[string]map[string]interface{}{}
	for _, g := range doc.Groups {
		names[g["name"].(string)] = g
	}
	if names["流媒体"] == nil || names["Proxy"] == nil {
		t.Fatalf("策略组缺了: %v", doc.Groups)
	}
	if names["Auto"]["type"] != "fallback" {
		t.Fatalf("同名策略组要用模板的: %v", names["Auto"])
	}
	// 模板没写规则也要走代理,不能全部直连
	body, err = BuildClash(user, lines, []Entry{{Host: "1.2.3.4"}}, "mixed-port: 1\n", "")
	if err != nil || !strings.Contains(body, "MATCH,Proxy") {
		t.Fatalf("模板没写规则时要补默认: %v\n%s", err, body)
	}
}

// sing-box 模板:模板写的段原样用、{all} 展开成全部节点、没写的 proxy / auto / direct 补上。
// 模板只改分流、没写 default_domain_resolver:sing-box 1.14 起整份配置起不来,要从 dns 段补上。
func TestSingBoxTemplateResolver(t *testing.T) {
	user := model.User{Name: "u"}
	resolver := func(tmpl string) interface{} {
		t.Helper()
		res, err := BuildSingBoxSub(user, nil, Options{SingBoxTmpl: tmpl, External: []ExtItem{{Links: []string{"trojan://p@198.51.100.1:443?sni=a.example.com#n1"}}}})
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Route map[string]interface{} `json:"route"`
		}
		if err := json.Unmarshal([]byte(res.Body), &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Route["default_domain_resolver"]
	}
	if got := resolver(`{"route":{"final":"proxy"}}`); got != "local" {
		t.Fatalf("没写 dns 段时用内置的 local,实际 %v", got)
	}
	if got := resolver(`{"dns":{"servers":[{"type":"https","tag":"r","server":"1.1.1.1","detour":"proxy"},{"type":"udp","tag":"cn","server":"223.5.5.5"}]},"route":{"final":"proxy"}}`); got != "cn" {
		t.Fatalf("模板自己的 dns:挑不走代理的,实际 %v", got)
	}
	if got := resolver(`{"dns":{"servers":[{"type":"https","tag":"r","server":"1.1.1.1","detour":"proxy"}]},"route":{"final":"proxy"}}`); got != nil {
		t.Fatalf("只有走代理的 dns:不瞎填,实际 %v", got)
	}
	if got := resolver(`{"route":{"final":"proxy","default_domain_resolver":"mine"}}`); got != "mine" {
		t.Fatalf("模板写了就听模板的,实际 %v", got)
	}
}

func TestSingBoxTemplate(t *testing.T) {
	tmpl := `{"route":{"final":"流媒体","rules":[{"action":"sniff"}]},"outbounds":[{"type":"selector","tag":"流媒体","outbounds":["proxy","{all}"]}]}`
	lines := []model.Line{{Id: 1, Name: "tj", Protocol: "trojan", Port: 2, Enabled: true}}
	user := model.User{Name: "a", Credentials: []byte(`{"trojan":{"password":"p"}}`)}
	res, err := BuildSingBoxSub(user, lines, Options{Entries: []Entry{{Host: "1.2.3.4"}}, SingBoxTmpl: tmpl})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(res.Body), &doc); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	if doc["route"].(map[string]interface{})["final"] != "流媒体" {
		t.Fatalf("模板的 route 丢了: %v", doc["route"])
	}
	if doc["dns"] == nil || doc["inbounds"] == nil {
		t.Fatalf("模板没写的段要用内置的: %v", doc)
	}
	byTag := map[string]map[string]interface{}{}
	for _, o := range doc["outbounds"].([]interface{}) {
		m := o.(map[string]interface{})
		byTag[m["tag"].(string)] = m
	}
	for _, tag := range []string{"流媒体", "proxy", "auto", "direct", "tj"} {
		if byTag[tag] == nil {
			t.Fatalf("缺出站 %s: %v", tag, doc["outbounds"])
		}
	}
	list := byTag["流媒体"]["outbounds"].([]interface{})
	if len(list) != 2 || list[0] != "proxy" || list[1] != "tj" {
		t.Fatalf("{all} 没展开: %v", list)
	}
	// 模板是坏的:报错,不能吐一份半截配置
	if _, err := BuildSingBoxSub(user, lines, Options{Entries: []Entry{{Host: "1.2.3.4"}}, SingBoxTmpl: "{坏"}); err == nil {
		t.Fatal("坏模板应该报错")
	}
	// 默认骨架本身要是合法 JSON
	if err := json.Unmarshal([]byte(DefaultSingBoxTemplate()), &doc); err != nil {
		t.Fatalf("内置骨架不是合法 JSON: %v", err)
	}
}
