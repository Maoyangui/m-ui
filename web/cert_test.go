package web

import "testing"

// 域名会进证书文件名与 ACME 请求,奇怪的输入要在入口就挡掉。
func TestValidDomain(t *testing.T) {
	ok := []string{"example.com", "hk.joinvip.vip", "*.example.com", "a-b.c-d.io", "xn--fiqs8s.com"}
	bad := []string{"", "localhost", "example", ".com", "example.", "-a.com", "a-.com",
		"../../etc/passwd", "a/b.com", "a b.com", "例子.com", "a..com", "http://a.com"}
	for _, d := range ok {
		if !validDomain(d) {
			t.Errorf("应通过: %q", d)
		}
	}
	for _, d := range bad {
		if validDomain(d) {
			t.Errorf("应拒绝: %q", d)
		}
	}
}
