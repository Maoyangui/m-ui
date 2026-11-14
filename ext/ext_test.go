package ext

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/upstream"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestParseLinksPlainAndBase64AndYAML(t *testing.T) {
	links := "vless://11111111-1111-1111-1111-111111111111@1.2.3.4:443?security=reality&pbk=PUBKEY&sid=ab12&fp=chrome&sni=www.apple.com&type=tcp&flow=xtls-rprx-vision#节点A\n" +
		"hysteria2://pw@5.6.7.8:8443?sni=h.example.com&insecure=1#节点B\n" +
		"ss://" + base64.StdEncoding.EncodeToString([]byte("aes-128-gcm:secret")) + "@9.9.9.9:8388#节点C\n"
	it := Parse(links)
	if len(it.Links()) != 3 || len(it.Clash()) != 3 {
		t.Fatalf("明文链接应解析 3/3,实际 %d/%d", len(it.Links()), len(it.Clash()))
	}
	a := it.Clash()[0]
	if a["type"] != "vless" || a["server"] != "1.2.3.4" || a["port"] != 443 || a["flow"] != "xtls-rprx-vision" || a["servername"] != "www.apple.com" {
		t.Fatalf("vless 转换错误: %v", a)
	}
	if ro, _ := a["reality-opts"].(map[string]interface{}); ro == nil || ro["public-key"] != "PUBKEY" || ro["short-id"] != "ab12" {
		t.Fatalf("reality 参数丢失: %v", a)
	}
	b := it.Clash()[1]
	if b["type"] != "hysteria2" || b["password"] != "pw" || b["sni"] != "h.example.com" || b["skip-cert-verify"] != true {
		t.Fatalf("hy2 转换错误: %v", b)
	}
	c := it.Clash()[2]
	if c["type"] != "ss" || c["cipher"] != "aes-128-gcm" || c["password"] != "secret" {
		t.Fatalf("ss 转换错误: %v", c)
	}

	if it2 := Parse(b64(links)); len(it2.Links()) != 3 {
		t.Fatalf("base64 链接列表应解析 3 条,实际 %d", len(it2.Links()))
	}

	yamlDoc := "port: 7890\nproxies:\n  - name: 外部A\n    type: ss\n    server: 1.1.1.1\n    port: 8388\n    cipher: aes-128-gcm\n    password: x\n  - name: 外部B\n    type: trojan\n    server: 2.2.2.2\n    port: 443\n    password: y\n    sni: t.example.com\nproxy-groups: []\n"
	it3 := Parse(yamlDoc)
	if len(it3.Clash()) != 2 || it3.Clash()[1]["sni"] != "t.example.com" || len(it3.Links()) != 2 {
		t.Fatalf("YAML 应解析 2 个 clash 代理: %+v", it3)
	}

	pre := WithPrefix(it, "[外] ")
	if !strings.Contains(pre.Links()[0], "#%5B%E5%A4%96%5D%20") || pre.Clash()[0]["name"] != "[外] 节点A" {
		t.Fatalf("前缀未生效: %s / %v", pre.Links()[0], pre.Clash()[0]["name"])
	}
	if it.Clash()[0]["name"] != "节点A" {
		t.Fatal("WithPrefix 不应改动原数据")
	}
}

func TestParseGarbage(t *testing.T) {
	if it := Parse("hello world"); len(it.Nodes) != 0 {
		t.Fatal("垃圾内容应为空")
	}
	if _, ok := LinkToClash("foo://bar"); ok {
		t.Fatal("不支持的链接应返回 false")
	}
}

// 审计 I5:以前给 vmess 链接追加 "#前缀",链接不再是合法 base64,sing-box 订阅整条丢掉、通用链接订阅里前缀也不生效。
// 现在改 base64 里的 ps;没名字的链接加前缀后和 Clash 侧同名(前缀 + 服务器地址)。
func TestWithPrefixVmessStillParses(t *testing.T) {
	vm := `vmess://eyJ2IjoiMiIsInBzIjoi6aaZ5rivIiwiYWRkIjoiMS4xLjEuMSIsInBvcnQiOiI0NDMiLCJpZCI6ImEifQ==`
	it := WithPrefix(Parse(vm+"\ntrojan://pw@2.2.2.2:443"), "[中转] ")
	p, err := upstream.ParseLink(it.Links()[0])
	if err != nil || p.Name != "[中转] 香港" {
		t.Fatalf("加前缀后的 vmess 应能解析、名字带前缀: %v %+v (%s)", err, p, it.Links()[0])
	}
	if it.Clash()[0]["name"] != "[中转] 香港" {
		t.Fatalf("Clash 侧名字不对: %v", it.Clash()[0]["name"])
	}
	if LinkName(it.Links()[1]) != "[中转] 2.2.2.2" || it.Clash()[1]["name"] != "[中转] 2.2.2.2" {
		t.Fatalf("没名字的链接加前缀后两侧应一致: %q %v", LinkName(it.Links()[1]), it.Clash()[1]["name"])
	}
}

// 指纹不随名字、顺序变化;同一个节点从 Clash YAML 和分享链接解析出来指纹一致(抓取身份不同拿到两种格式时还认得)。
func TestNodeKeyStableAcrossFormats(t *testing.T) {
	y := "proxies:\n" +
		"  - {name: 'A 名字一', type: vless, server: v.example.com, port: 443, uuid: 8F0C6A3E-0D7D-4D3A-9D0C-7C2A2A1B1C1D, tls: true, network: ws, ws-opts: {path: /ws, headers: {Host: cdn.example.com}}}\n" +
		"  - {name: 'B', type: trojan, server: t.example.com, port: 443, password: pw}\n" +
		"  - {name: 'C', type: hysteria2, server: h.example.com, port: 443, password: pw, sni: h.example.com}\n"
	fromYAML := Parse(y)
	var links []string
	for _, n := range fromYAML.Nodes {
		links = append(links, n.Link)
	}
	fromLinks := Parse(strings.Join(links, "\n"))
	for i := range fromYAML.Nodes {
		if fromYAML.Nodes[i].Key != fromLinks.Nodes[i].Key {
			t.Errorf("节点 %d 两种格式指纹不同: %s vs %s (%s)", i, fromYAML.Nodes[i].Key, fromLinks.Nodes[i].Key, links[i])
		}
	}
	renamed := Parse(strings.Replace(y, "A 名字一", "A 名字二", 1))
	if renamed.Nodes[0].Key != fromYAML.Nodes[0].Key {
		t.Fatal("改名不该改变指纹")
	}
	path := Parse(strings.Replace(y, "path: /ws", "path: /other", 1))
	if path.Nodes[0].Key == fromYAML.Nodes[0].Key {
		t.Fatal("同一入口不同路径应是不同节点")
	}
}
