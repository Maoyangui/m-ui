package ext

import (
	"strings"
	"testing"
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
	if len(it.Clash) != 2 || len(it.Links) != 2 {
		t.Fatalf("clash=%d links=%d, want 2/2: %v", len(it.Clash), len(it.Links), it.Links)
	}
	if !strings.HasPrefix(it.Links[0], "hysteria2://") || !strings.HasPrefix(it.Links[1], "vless://") {
		t.Fatalf("links: %v", it.Links)
	}
	pre := WithPrefix(it, "[tw] ")
	if !strings.Contains(pre.Links[0], "#%5Btw%5D%20") && !strings.Contains(pre.Links[0], "#[tw] ") {
		t.Fatalf("prefix missing in link: %s", pre.Links[0])
	}
}
