package ext

import (
	"encoding/base64"
	"strings"
	"testing"
)

// 外部订阅的内容来自第三方服务器,可能是任意字节:
// Parse 与 ClashToLink 只能忽略坏数据,不能 panic(它们跑在后台刷新协程里,panic 会带走进程)。
func TestParseNeverPanics(t *testing.T) {
	cases := []string{
		"", " ", "\n\n\n", "\x00\x01\x02", strings.Repeat("A", 100000),
		"not a subscription", "ss://", "vmess://", "://", "#", "#####",
		base64.StdEncoding.EncodeToString([]byte("ss://\nvmess://\n#\n")),
		"proxies:", "proxies: []", "proxies:\n  - {}", "proxies:\n  - name: x",
		"proxies:\n  - name: x\n    type: ss", "proxies:\n  - name: x\n    type: ss\n    server: 1.2.3.4",
		"proxies:\n  - name: x\n    type: unknown-type\n    server: h\n    port: 1",
		"proxies:\n  - name: x\n    type: vmess\n    server: h\n    port: notaport",
		"proxies:\n  - [1,2,3]", "proxies: 42", "proxies:\n  - name: [x]\n    type: ss",
		"{", "}", "[]", "{\"proxies\":[{}]}", "\xff\xfe\xfd",
		"vmess://#", "vmess://%%%#x", "trojan://#%", "ss://x@h:1/?plugin=%#a", "hysteria2://p@h:1?mport=a-b#x",
		"proxies:\n  - {name: x, type: ss, server: h, port: 1, cipher: c, plugin: obfs, plugin-opts: 5}",
		"proxies:\n  - {name: x, type: vless, server: h, port: 1, uuid: u, network: ws, ws-opts: {headers: 5}}",
		"proxies:\n  - {name: x, type: vless, server: h, port: 1, uuid: u, network: h2, h2-opts: {host: 5}}",
		"proxies:\n  - {name: x, type: hysteria2, server: h, port: 1, ports: 'a-b', up: [1]}",
		"proxies:\n  - {name: x, type: vless, server: h, port: 1, uuid: u, reality-opts: {short-id: [1]}, smux: {enabled: true, brutal-opts: 3}}",
		`{"outbounds":[{"type":"shadowsocks","tag":"s","server":"h","server_port":"x","plugin":"obfs-local","plugin_opts":"\\"}]}`,
	}
	for _, c := range cases {
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("Parse(%.40q) panic: %v", c, v)
				}
			}()
			it := Parse(c)
			for _, p := range it.Clash() {
				func() {
					defer func() {
						if v := recover(); v != nil {
							t.Fatalf("ClashToLink(%v) panic: %v", p, v)
						}
					}()
					_, _ = ClashToLink(p)
					_, _, _ = ClashToOutbound(p)
				}()
			}
			_ = WithPrefix(it, "[前缀] ")
			// 面板展开、隐藏改名、测速取出站也都跑在这份数据上
			infos := Details(it)
			f := ParseFilter(nil, nil, nil, nil, "x")
			r := RosterOf(it)
			for _, n := range it.Nodes {
				f.Hide(r, n.Name, n.Key)
				f.Rename(r, n.Name, n.Key, "改")
				_, _, _ = Outbound(n)
			}
			Annotate(infos, f)
			_ = f.Apply(it)
			_ = ContentSig(it)
		}()
	}
}
