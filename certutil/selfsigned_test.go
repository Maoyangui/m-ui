package certutil

import (
	"crypto/tls"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerateSelfSigned(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "main.crt"), filepath.Join(dir, "main.key")
	if err := GenerateSelfSigned([]string{"hk.example.test", "127.0.0.1"}, crt, key, 30); err != nil {
		t.Fatal(err)
	}
	// 能被 Go TLS 栈加载即为有效证书/私钥对
	if _, err := tls.LoadX509KeyPair(crt, key); err != nil {
		t.Fatalf("证书/私钥不匹配或格式错误: %v", err)
	}
	exp, err := Expiry(crt)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(exp); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("有效期应约 30 天,实际 %v", d)
	}
}

func TestGenerateSelfSignedNoHosts(t *testing.T) {
	if err := GenerateSelfSigned(nil, "a", "b", 1); err == nil {
		t.Fatal("无 host 应报错")
	}
}
