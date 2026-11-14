package certutil

import (
	"crypto/tls"
	"crypto/x509"
	"path/filepath"
	"testing"
)

// 订阅端口按 SNI 挑证书:哪张写着客户端要的域名就用哪张;都没写、没带域名都用第一张(默认)。
func TestSNI(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, host string) *Reloader {
		t.Helper()
		crt, key := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
		if err := GenerateSelfSigned([]string{host}, crt, key, 30); err != nil {
			t.Fatal(err)
		}
		r, err := NewReloader(crt, key)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	own, carried := mk("own", "tw.example.test"), mk("carried", "sub.example.test")
	s := NewSNI(carried, own)
	host := func(name string) string {
		c, err := s.GetCertificate(&tls.ClientHelloInfo{ServerName: name})
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(c.Certificate[0])
		return leaf.DNSNames[0]
	}
	if got := host("sub.example.test"); got != "sub.example.test" {
		t.Fatalf("订阅域名要拿随主机身份带来的证书: %s", got)
	}
	if got := host("tw.example.test"); got != "tw.example.test" {
		t.Fatalf("本机自己的域名用本机的证书: %s", got)
	}
	if got := host("other.example.test"); got != "sub.example.test" {
		t.Fatalf("都没写这个域名:用第一张: %s", got)
	}
	if got := host(""); got != "sub.example.test" {
		t.Fatalf("没带域名(按 IP 连):用第一张: %s", got)
	}
}
