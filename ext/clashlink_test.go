package ext

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/upstream"
)

// clash YAML 外部订阅 → 链接订阅:每种协议都要能转成分享链接,且能被本项目的链接解析器读回同样的服务器/端口。
func TestClashToLinkRoundTrip(t *testing.T) {
	cases := []map[string]interface{}{
		{"name": "hy", "type": "hysteria2", "server": "1.2.3.4", "port": 443, "password": "p w", "sni": "a.example.com", "alpn": []interface{}{"h3"}},
		{"name": "any", "type": "anytls", "server": "1.2.3.4", "port": 8443, "password": "pw", "sni": "a.example.com", "skip-cert-verify": true},
		{"name": "vl", "type": "vless", "server": "1.2.3.4", "port": 8888, "uuid": "8f0c6a3e-0d7d-4d3a-9d0c-7c2a2a1b1c1d", "tls": true, "servername": "www.booking.com", "flow": "xtls-rprx-vision", "client-fingerprint": "chrome", "reality-opts": map[string]interface{}{"public-key": "MJnakD6uWVj6xdd5BTOIo8yP6WRiAciOtTcF_23k50A", "short-id": "5a21"}},
		{"name": "tr", "type": "trojan", "server": "h.example.com", "port": 443, "password": "pw", "sni": "h.example.com", "network": "ws", "ws-opts": map[string]interface{}{"path": "/x", "headers": map[string]interface{}{"Host": "h.example.com"}}},
		{"name": "ss", "type": "ss", "server": "1.2.3.4", "port": 8388, "cipher": "aes-256-gcm", "password": "pw"},
		{"name": "vm", "type": "vmess", "server": "1.2.3.4", "port": 80, "uuid": "8f0c6a3e-0d7d-4d3a-9d0c-7c2a2a1b1c1d", "alterId": 0, "network": "ws", "ws-opts": map[string]interface{}{"path": "/ws"}},
		{"name": "tu", "type": "tuic", "server": "1.2.3.4", "port": 8844, "uuid": "8f0c6a3e-0d7d-4d3a-9d0c-7c2a2a1b1c1d", "password": "pw", "sni": "a.example.com", "congestion-controller": "bbr"},
	}
	for _, c := range cases {
		link, ok := ClashToLink(c)
		if !ok {
			t.Fatalf("%s: 转换失败", c["name"])
		}
		back, ok := LinkToClash(link)
		if !ok {
			t.Fatalf("%s: 链接无法读回: %s", c["name"], link)
		}
		if back["server"] != c["server"] || back["port"] != c["port"] {
			t.Fatalf("%s: 读回的 server/port 不一致: %v vs %v (%s)", c["name"], back, c, link)
		}
		if c["type"] != "vmess" && !strings.HasSuffix(link, "#"+c["name"].(string)) { // vmess 的名称在 JSON 的 ps 字段
			t.Fatalf("%s: 名称未进 fragment: %s", c["name"], link)
		}
	}
	if _, ok := ClashToLink(map[string]interface{}{"name": "x", "type": "wireguard", "server": "1.1.1.1", "port": 1}); ok {
		t.Fatal("不支持的类型应返回 false")
	}
}

// 外部订阅是 clash YAML 时,Parse 也要产出链接,链接订阅才看得到这些节点。
func TestParseClashYAMLProducesLinks(t *testing.T) {
	y := "proxies:\n  - name: 台湾warp1\n    type: hysteria2\n    server: 34.81.174.123\n    port: 443\n    password: secret\n    sni: aron.joinvip.vip\n    alpn:\n      - h3\n  - name: 台湾warp3\n    type: vless\n    server: 34.81.174.123\n    port: 8888\n    uuid: 8f0c6a3e-0d7d-4d3a-9d0c-7c2a2a1b1c1d\n    tls: true\n    servername: www.booking.com\n    flow: xtls-rprx-vision\n    reality-opts:\n      public-key: MJnakD6uWVj6xdd5BTOIo8yP6WRiAciOtTcF_23k50A\n      short-id: 5a21\n"
	it := Parse(y)
	if len(it.Clash()) != 2 || len(it.Links()) != 2 {
		t.Fatalf("clash=%d links=%d, want 2/2: %v", len(it.Clash()), len(it.Links()), it.Links())
	}
	if !strings.HasPrefix(it.Links()[0], "hysteria2://") || !strings.HasPrefix(it.Links()[1], "vless://") {
		t.Fatalf("links: %v", it.Links())
	}
	pre := WithPrefix(it, "[tw] ")
	if !strings.Contains(pre.Links()[0], "#%5Btw%5D%20") {
		t.Fatalf("prefix missing in link: %s", pre.Links()[0])
	}
}

// 审计 I5:Clash → 分享链接以前丢参数,链接订阅里这些节点连不上:ss 的插件、vmess 的跳过证书校验、
// ws 上的 v2ray-http-upgrade(其实是 httpupgrade)、h2 的 Host。现在都写进链接,读回来还在。
func TestClashToLinkKeepsPluginInsecureAndTransport(t *testing.T) {
	ss := map[string]interface{}{"name": "ss", "type": "ss", "server": "1.2.3.4", "port": 8388, "cipher": "aes-128-gcm", "password": "x",
		"plugin": "obfs", "plugin-opts": map[string]interface{}{"mode": "http", "host": "bing.com"}}
	link, ok := ClashToLink(ss)
	p, err := upstream.ParseLink(link)
	if !ok || err != nil || p.Options["plugin"] != "obfs-local" || p.Options["plugin_opts"] != "obfs=http;obfs-host=bing.com" {
		t.Fatalf("ss 插件没进链接: %s %v %+v", link, err, p)
	}
	vm := map[string]interface{}{"name": "vm", "type": "vmess", "server": "1.2.3.4", "port": 443, "uuid": "00000000-0000-0000-0000-000000000000",
		"alterId": 0, "cipher": "auto", "tls": true, "skip-cert-verify": true, "servername": "x.example.com",
		"network": "ws", "ws-opts": map[string]interface{}{"path": "/up", "v2ray-http-upgrade": true, "headers": map[string]interface{}{"Host": "cdn.example.com"}}}
	link, _ = ClashToLink(vm)
	p, err = upstream.ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	opt := string(p.OptionsJSON())
	if !strings.Contains(opt, `"insecure":true`) || !strings.Contains(opt, `"type":"httpupgrade"`) || !strings.Contains(opt, `"host":"cdn.example.com"`) {
		t.Fatalf("vmess 的跳过证书校验 / httpupgrade 丢了: %s", opt)
	}
	h2 := map[string]interface{}{"name": "h2", "type": "vless", "server": "1.2.3.4", "port": 443, "uuid": "00000000-0000-0000-0000-000000000000",
		"tls": true, "network": "h2", "h2-opts": map[string]interface{}{"host": []interface{}{"a.example.com"}, "path": "/h2"}}
	link, _ = ClashToLink(h2)
	p, _ = upstream.ParseLink(link)
	if opt := string(p.OptionsJSON()); !strings.Contains(opt, `"host":["a.example.com"]`) || !strings.Contains(opt, `"path":"/h2"`) {
		t.Fatalf("h2 的 Host 丢了: %s (%s)", opt, link)
	}
	// 链接写不出的插件:不给链接,别给一条缺插件、连不上的
	if _, ok := ClashToLink(map[string]interface{}{"name": "x", "type": "ss", "server": "1.2.3.4", "port": 1, "cipher": "aes-128-gcm", "password": "x", "plugin": "shadow-tls"}); ok {
		t.Fatal("shadow-tls 插件不该转成链接")
	}
	// vmess 链接里的 allowInsecure 读回来是跳过证书校验
	raw, _ := json.Marshal(map[string]interface{}{"v": "2", "ps": "a", "add": "1.2.3.4", "port": "443", "id": "x", "tls": "tls", "allowInsecure": true})
	if c, ok := LinkToClash("vmess://" + base64.StdEncoding.EncodeToString(raw)); !ok || c["skip-cert-verify"] != true {
		t.Fatalf("vmess allowInsecure 应读回: %v", c)
	}
}

// sing-box 订阅里的 ss 插件转成 Clash 时要是 map(以前原样塞了 SIP003 字符串,Clash 不认,整份配置加载失败)。
func TestSingBoxPluginToClash(t *testing.T) {
	src := `{"outbounds":[{"type":"shadowsocks","tag":"s","server":"1.2.3.4","server_port":8388,"method":"aes-128-gcm","password":"x","plugin":"obfs-local","plugin_opts":"obfs=tls;obfs-host=a.example.com"}]}`
	it := Parse(src)
	if len(it.Nodes) != 1 {
		t.Fatalf("应解析出 1 个: %+v", it)
	}
	c := it.Nodes[0].Clash
	po, _ := c["plugin-opts"].(map[string]interface{})
	if c["plugin"] != "obfs" || po["mode"] != "tls" || po["host"] != "a.example.com" {
		t.Fatalf("插件参数应转成 Clash 的 map: %v", c)
	}
	if kv := parseSIP003(`mode=websocket;tls;host=a\;b;path=/p`); kv["mode"] != "websocket" || kv["host"] != "a;b" || kv["path"] != "/p" {
		t.Fatalf("SIP003 解析不对: %v", kv)
	} else if _, ok := kv["tls"]; !ok {
		t.Fatalf("开关参数应保留: %v", kv)
	}
}
