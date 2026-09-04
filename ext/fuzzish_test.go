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
	}
	for _, c := range cases {
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("Parse(%.40q) panic: %v", c, v)
				}
			}()
			it := Parse(c)
			for _, p := range it.Clash {
				func() {
					defer func() {
						if v := recover(); v != nil {
							t.Fatalf("ClashToLink(%v) panic: %v", p, v)
						}
					}()
					_, _ = ClashToLink(p)
				}()
			}
			_ = WithPrefix(it, "[前缀] ")
		}()
	}
}
