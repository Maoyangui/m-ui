package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database"
)

// 来源是「服务器上已有的证书」时,自签 / 签发不能往那两个文件里写(那是 certbot / nginx 的原件),
// 改写面板自己的固定路径;自动续期也只续面板自己签发的(审计 M2)。
func TestOwnCertNeverOverwritesExternal(t *testing.T) {
	dir := t.TempDir()
	r, err := New(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.DB())
	defer r.core.Stop()

	ext := filepath.Join(t.TempDir(), "live")
	extCert, extKey := filepath.Join(ext, "fullchain.pem"), filepath.Join(ext, "privkey.pem")
	if err := certutil.GenerateSelfSigned([]string{"ext.example"}, extCert, extKey, 30); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(extCert)
	if err := r.UseExternalCert(extCert, extKey, false, false); err != nil {
		t.Fatal(err)
	}
	r.SetSetting("acmeDomain", "old.example") // 以前签发过,换来源后还留着
	if c, _ := r.ownCertPaths("old.example"); c == extCert {
		t.Fatal("签发不能写到外部证书的原件路径")
	}
	if err := r.SelfSign([]string{"1.2.3.4"}, false, false); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(extCert); !bytes.Equal(now, orig) {
		t.Fatal("自签把外部证书的原件覆盖了")
	}
	if c := r.setting("certFile"); c == extCert || r.CertSource() != "selfsign" {
		t.Fatalf("自签应改用面板自己的路径并把来源换成自签: certFile=%s source=%s", c, r.CertSource())
	}
}
