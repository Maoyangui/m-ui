package ext

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/core"
)

// outboundJSON 转出的出站加上 type / tag,和 render.OutboundJSON 落库后渲染的一样。
func outboundJSON(t *testing.T, typ string, opts map[string]interface{}) json.RawMessage {
	t.Helper()
	m := map[string]interface{}{"type": typ, "tag": "x"}
	for k, v := range opts {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 审计 I5:测速与"添加为上游"用的出站以前经"Clash → 分享链接 → 解析"绕一圈,ss 插件、vmess 的跳过证书校验、
// httpupgrade 都丢了:这些节点测速误报不通,加成上游后线路不通。现在直接转,字段齐全,并且 sing-box 干跑能过。
func TestClashToOutboundLossless(t *testing.T) {
	cases := []struct {
		name string
		p    map[string]interface{}
		typ  string
		want []string // 出站 JSON 里必须有的片段
	}{
		{"ss+obfs", map[string]interface{}{"type": "ss", "server": "1.2.3.4", "port": 8388, "cipher": "aes-128-gcm", "password": "x",
			"plugin": "obfs", "plugin-opts": map[string]interface{}{"mode": "http", "host": "bing.com"}},
			"shadowsocks", []string{`"plugin":"obfs-local"`, `"plugin_opts":"obfs=http;obfs-host=bing.com"`}},
		{"ss+v2ray-plugin", map[string]interface{}{"type": "ss", "server": "1.2.3.4", "port": 443, "cipher": "aes-128-gcm", "password": "x",
			"plugin": "v2ray-plugin", "plugin-opts": map[string]interface{}{"mode": "websocket", "tls": true, "host": "a.example.com", "path": "/v"}},
			"shadowsocks", []string{`"plugin":"v2ray-plugin"`, `mode=websocket;tls;host=a.example.com;path=/v;mux=0`}},
		{"vmess+tls+skip-cert-verify", map[string]interface{}{"type": "vmess", "server": "1.2.3.4", "port": 443, "uuid": "00000000-0000-0000-0000-000000000000",
			"alterId": 0, "cipher": "auto", "tls": true, "skip-cert-verify": true, "servername": "x.example.com", "client-fingerprint": "chrome"},
			"vmess", []string{`"insecure":true`, `"server_name":"x.example.com"`, `"fingerprint":"chrome"`}},
		{"vmess+ws 用 Host 当 SNI", map[string]interface{}{"type": "vmess", "server": "1.2.3.4", "port": 443, "uuid": "00000000-0000-0000-0000-000000000000",
			"cipher": "auto", "tls": true, "network": "ws", "ws-opts": map[string]interface{}{"path": "/ws", "headers": map[string]interface{}{"Host": "cdn.example.com"}, "max-early-data": 2048}},
			"vmess", []string{`"server_name":"cdn.example.com"`, `"type":"ws"`, `"Host":"cdn.example.com"`, `"max_early_data":2048`}},
		{"vless+httpupgrade", map[string]interface{}{"type": "vless", "server": "1.2.3.4", "port": 443, "uuid": "00000000-0000-0000-0000-000000000000",
			"tls": true, "servername": "a.example.com", "network": "ws", "ws-opts": map[string]interface{}{"path": "/up", "v2ray-http-upgrade": true, "headers": map[string]interface{}{"Host": "a.example.com"}}},
			"vless", []string{`"type":"httpupgrade"`, `"host":"a.example.com"`, `"path":"/up"`}},
		{"vless+reality", map[string]interface{}{"type": "vless", "server": "1.2.3.4", "port": 443, "uuid": "00000000-0000-0000-0000-000000000000",
			"tls": true, "servername": "www.apple.com", "flow": "xtls-rprx-vision", "reality-opts": map[string]interface{}{"public-key": "MJnakD6uWVj6xdd5BTOIo8yP6WRiAciOtTcF_23k50A", "short-id": "5a21"}},
			"vless", []string{`"flow":"xtls-rprx-vision"`, `"public_key":"MJnakD6uWVj6xdd5BTOIo8yP6WRiAciOtTcF_23k50A"`, `"short_id":"5a21"`, `"fingerprint":"chrome"`}},
		{"vless+h2", map[string]interface{}{"type": "vless", "server": "1.2.3.4", "port": 443, "uuid": "00000000-0000-0000-0000-000000000000",
			"tls": true, "network": "h2", "h2-opts": map[string]interface{}{"host": []interface{}{"h.example.com"}, "path": "/h2"}},
			"vless", []string{`"type":"http"`, `"host":["h.example.com"]`, `"server_name":"h.example.com"`}},
		{"trojan+grpc", map[string]interface{}{"type": "trojan", "server": "1.2.3.4", "port": 443, "password": "pw", "sni": "t.example.com",
			"network": "grpc", "grpc-opts": map[string]interface{}{"grpc-service-name": "svc"}},
			"trojan", []string{`"service_name":"svc"`, `"server_name":"t.example.com"`}},
		{"hysteria2 端口跳跃 + obfs", map[string]interface{}{"type": "hysteria2", "server": "1.2.3.4", "port": 443, "password": "pw", "sni": "h.example.com",
			"ports": "443,20000-30000", "hop-interval": 30, "obfs": "salamander", "obfs-password": "op", "up": "100 Mbps", "down": "1 Gbps"},
			"hysteria2", []string{`"server_ports":["443:443","20000:30000"]`, `"hop_interval":"30s"`, `"type":"salamander"`, `"up_mbps":100`, `"down_mbps":1000`, `"alpn":["h3"]`}},
		{"tuic", map[string]interface{}{"type": "tuic", "server": "1.2.3.4", "port": 443, "uuid": "00000000-0000-0000-0000-000000000000", "password": "pw",
			"sni": "t.example.com", "congestion-controller": "bbr", "udp-relay-mode": "quic", "reduce-rtt": true, "heartbeat-interval": 10000},
			"tuic", []string{`"congestion_control":"bbr"`, `"udp_relay_mode":"quic"`, `"zero_rtt_handshake":true`, `"heartbeat":"10000ms"`}},
		{"anytls", map[string]interface{}{"type": "anytls", "server": "1.2.3.4", "port": 443, "password": "pw", "sni": "a.example.com", "skip-cert-verify": true},
			"anytls", []string{`"password":"pw"`, `"insecure":true`}},
		{"socks5", map[string]interface{}{"type": "socks5", "server": "1.2.3.4", "port": 1080, "username": "u", "password": "p"},
			"socks", []string{`"version":"5"`, `"username":"u"`}},
		{"http+tls", map[string]interface{}{"type": "http", "server": "1.2.3.4", "port": 443, "username": "u", "password": "p", "tls": true, "sni": "p.example.com"},
			"http", []string{`"server_name":"p.example.com"`}},
	}
	for _, c := range cases {
		c.p["name"] = c.name
		typ, opts, err := ClashToOutbound(c.p)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if typ != c.typ {
			t.Fatalf("%s: 类型 %s,应为 %s", c.name, typ, c.typ)
		}
		raw := outboundJSON(t, typ, opts)
		for _, w := range c.want {
			if !strings.Contains(string(raw), w) {
				t.Errorf("%s: 出站里没有 %s:%s", c.name, w, raw)
			}
		}
		if err := core.ValidateOutbound(raw); err != nil { // 和"添加为上游"同一道干跑
			t.Errorf("%s: sing-box 不认: %v\n%s", c.name, err, raw)
		}
	}
}

// 转不全的不硬转:标成不能测 / 不能作上游并说明原因,而不是拿一个残缺的出站去测、去建上游。
func TestClashToOutboundRefusesLossy(t *testing.T) {
	base := func(extra map[string]interface{}) map[string]interface{} {
		m := map[string]interface{}{"name": "n", "server": "1.2.3.4", "port": 443}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for name, p := range map[string]map[string]interface{}{
		"shadow-tls 插件":     base(map[string]interface{}{"type": "ss", "cipher": "aes-128-gcm", "password": "x", "plugin": "shadow-tls"}),
		"v2ray-plugin 跳过校验": base(map[string]interface{}{"type": "ss", "cipher": "aes-128-gcm", "password": "x", "plugin": "v2ray-plugin", "plugin-opts": map[string]interface{}{"tls": true, "skip-cert-verify": true}}),
		"TUIC v4":           base(map[string]interface{}{"type": "tuic", "token": "t"}),
		"trojan-go ss":      base(map[string]interface{}{"type": "trojan", "password": "p", "ss-opts": map[string]interface{}{"enabled": true}}),
		"vless 加密":          base(map[string]interface{}{"type": "vless", "uuid": "u", "encryption": "mlkem768x25519plus"}),
		"xhttp 传输":          base(map[string]interface{}{"type": "vless", "uuid": "u", "network": "xhttp"}),
		"dialer-proxy":      base(map[string]interface{}{"type": "trojan", "password": "p", "dialer-proxy": "other"}),
		"wireguard":         base(map[string]interface{}{"type": "wireguard"}),
		"hysteria v1":       base(map[string]interface{}{"type": "hysteria", "auth-str": "x", "up": 10, "down": 10}), // 本站数据面没有注册这个出站
		"没有端口":              {"name": "n", "type": "trojan", "server": "1.2.3.4", "password": "p"},
	} {
		if _, _, err := ClashToOutbound(p); err == nil {
			t.Errorf("%s: 应拒绝转换", name)
		}
	}
}

// sing-box JSON 订阅:测速 / 添加为上游直接用原始出站;依赖别的出站(detour)的拒绝。
func TestParseSingBoxJSON(t *testing.T) {
	src := `{"outbounds":[
		{"type":"selector","tag":"proxy","outbounds":["h"]},
		{"type":"hysteria2","tag":"h","server":"1.2.3.4","server_port":443,"password":"p","tls":{"enabled":true,"server_name":"x.example.com","insecure":true}},
		{"type":"vless","tag":"chained","server":"1.2.3.5","server_port":443,"uuid":"00000000-0000-0000-0000-000000000000","detour":"h"},
		{"type":"direct","tag":"direct"}]}`
	it := Parse(src)
	if len(it.Nodes) != 2 || len(it.Links()) != 2 {
		t.Fatalf("应解析出 2 个节点,得到 %d / %d", len(it.Nodes), len(it.Links()))
	}
	p := it.Nodes[0].Clash
	if p["name"] != "h" || p["type"] != "hysteria2" || p["server"] != "1.2.3.4" || p["port"] != 443 || p["sni"] != "x.example.com" {
		t.Fatalf("字段不对: %v", p)
	}
	if !strings.HasPrefix(it.Links()[0], "hysteria2://") {
		t.Fatalf("链接不对: %s", it.Links()[0])
	}
	typ, opts, err := Outbound(it.Nodes[0])
	if err != nil || typ != "hysteria2" || opts["tag"] != nil || opts["type"] != nil {
		t.Fatalf("应直接用原始出站(去掉 type/tag): %s %v %v", typ, opts, err)
	}
	if tls, _ := opts["tls"].(map[string]interface{}); tls["insecure"] != true {
		t.Fatalf("原始出站的参数应原样保留: %v", opts)
	}
	if _, _, err := Outbound(it.Nodes[1]); err == nil {
		t.Fatal("依赖别的出站的应拒绝")
	}
}
