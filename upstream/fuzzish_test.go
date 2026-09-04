package upstream

import (
	"strings"
	"testing"
)

// 分享链接来自用户粘贴、外部订阅抓取,内容完全不可控:
// 解析器可以报错,但绝不能 panic(它跑在面板协程与订阅刷新协程里)。
func TestParseLinkNeverPanics(t *testing.T) {
	schemes := []string{"ss", "ssr", "vmess", "vless", "trojan", "tuic", "hysteria2", "hy2", "socks5", "http", "anytls", "wireguard", "unknown"}
	bodies := []string{
		"", "://", "//", "@", ":@", "@:", "://@:", "://:@", "://a@", "://@b", "://a:b@c",
		"://user@host", "://user@host:", "://user@host:0", "://user@host:99999", "://user@host:-1",
		"://user@host:abc", "://[::1]", "://[::1]:443", "://[", "://]", "://%", "://%zz",
		"://" + strings.Repeat("a", 5000), "://a@b?x=" + strings.Repeat("y", 5000),
		"://eyJhZGQiOiIxLjIuMy40In0", "://!!!!", "://AAAA====", "://a#" + strings.Repeat("#", 100),
		"://a@b:1?alpn=&sni=&fp=&pbk=&sid=&type=&host=&path=&mode=&security=",
		"://a@b:1?plugin=", "://a@b:1?plugin=;", "://a@b:1?mport=", "://a@b:1?mport=-",
	}
	for _, s := range schemes {
		for _, b := range bodies {
			link := s + b
			func() {
				defer func() {
					if v := recover(); v != nil {
						t.Fatalf("ParseLink(%q) panic: %v", link, v)
					}
				}()
				_, _ = ParseLink(link)
			}()
		}
	}
	// 正常链接仍要解析成功,保证上面的用例没把功能测没了
	ok := "trojan://pass@example.com:443?security=tls&sni=example.com#node"
	if p, err := ParseLink(ok); err != nil || p.Type != "trojan" {
		t.Fatalf("正常链接应解析成功: %v %+v", err, p)
	}
}
