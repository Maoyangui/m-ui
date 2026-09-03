package certutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerify(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	if err := GenerateSelfSigned([]string{"example.com"}, crt, key, 30); err != nil {
		t.Fatal(err)
	}
	if err := Verify(crt, key); err != nil {
		t.Fatalf("有效证书应通过: %v", err)
	}
	if err := Verify(filepath.Join(dir, "nope.crt"), key); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("缺失文件应报错: %v", err)
	}
	if err := Verify(dir, key); err == nil || !strings.Contains(err.Error(), "目录") {
		t.Fatalf("目录路径应报错: %v", err)
	}
	// 另一对证书的私钥:不匹配
	crt2, key2 := filepath.Join(dir, "b.crt"), filepath.Join(dir, "b.key")
	if err := GenerateSelfSigned([]string{"other.com"}, crt2, key2, 30); err != nil {
		t.Fatal(err)
	}
	if err := Verify(crt, key2); err == nil {
		t.Fatal("证书与私钥不匹配时应报错")
	}
	// 非 PEM 内容
	bad := filepath.Join(dir, "bad.crt")
	os.WriteFile(bad, []byte("not a cert"), 0o600)
	if err := Verify(bad, key); err == nil {
		t.Fatal("非法证书应报错")
	}
}
