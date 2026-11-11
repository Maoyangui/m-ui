package sub

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 订阅端口按 SNI 改用证书库里的别的证书,只在本机迁过主机身份(接任 / 被设为主机)之后:
// 没开容灾、没换过主机的,和以前一样只用订阅证书。
func TestSubSNIExtrasOnlyAfterIdentityMoved(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "own.crt"), filepath.Join(dir, "own.key")
	if err := certutil.GenerateSelfSigned([]string{"own.example.com"}, certFile, keyFile, 30); err != nil {
		t.Fatal(err)
	}
	db.Create(&model.Cert{Name: "own.example.com", Source: "acme", CertFile: certFile, KeyFile: keyFile})
	s := NewServer(db)
	if got := s.sniCerts(filepath.Join(dir, "sub.crt")); len(got) != 0 {
		t.Fatalf("没迁过主机身份:订阅端口只用订阅证书,不该有备选: %d", len(got))
	}
	db.Create(&model.Setting{Key: "haBecame", Value: `{"how":"takeover","epoch":2}`})
	if got := s.sniCerts(filepath.Join(dir, "sub.crt")); len(got) != 1 {
		t.Fatalf("迁过主机身份:本机自己的证书留给本机自己的域名: %d", len(got))
	}
	if got := s.sniCerts(certFile); len(got) != 0 {
		t.Fatal("订阅证书本身不重复算")
	}
}
