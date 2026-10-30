package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
)

func certTestRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.core.Stop()
		database.Close(r.DB())
	})
	return r
}

// genCert 在临时目录生成一张证书(自签,但带指定的域名),返回路径。
func genCert(t *testing.T, name string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	c, k := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	if err := certutil.GenerateSelfSigned([]string{name}, c, k, 30); err != nil {
		t.Fatal(err)
	}
	return c, k
}

// 来源是「服务器上已有的证书」时,自签 / 签发不能往那两个文件里写(那是 certbot / nginx 的原件),
// 改写面板自己的固定路径(审计 M2)。
func TestOwnCertNeverOverwritesExternal(t *testing.T) {
	r := certTestRunner(t)
	extCert, extKey := genCert(t, "ext.example")
	orig, _ := os.ReadFile(extCert)
	if err := r.UseExternalCert(extCert, extKey, CertTargets{Lines: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.SelfSign([]string{"1.2.3.4"}, CertTargets{Lines: true}); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(extCert); !bytes.Equal(now, orig) {
		t.Fatal("自签把外部证书的原件覆盖了")
	}
	if c := r.setting("certFile"); c == extCert || r.CertSource() != "selfsign" {
		t.Fatalf("自签应改用面板自己的路径并把来源换成自签: certFile=%s source=%s", c, r.CertSource())
	}
	// 同名的外部证书:签发也不能写到它的路径
	r.saveCertRow(model.Cert{Name: "ext.example", Source: "external", CertFile: extCert, KeyFile: extKey})
	if c, _ := r.storedPaths("ext.example"); c == extCert {
		t.Fatal("签发不能写到外部证书的原件路径")
	}
}

// 证书库:多张证书并存,各个用途各选各的,没勾的用途不动。
func TestCertStoreKeepsSeveral(t *testing.T) {
	r := certTestRunner(t)
	aCert, aKey := genCert(t, "a.example")
	bCert, bKey := genCert(t, "b.example")
	// 第一张:没有默认证书时它就是默认的
	if err := r.UseExternalCert(aCert, aKey, CertTargets{}); err != nil {
		t.Fatal(err)
	}
	if r.setting("certFile") != aCert {
		t.Fatalf("第一张证书应该成为默认: %s", r.setting("certFile"))
	}
	if err := r.UseCert("fullchain", CertForPanel); err != nil {
		t.Fatal(err)
	}
	// 第二张:只进证书库,默认证书、面板都不动
	if err := r.UseExternalCert(bCert, bKey, CertTargets{}); err == nil {
		// 名字都是文件名 fullchain,同名就是同一张:路径换成新的
		if got := r.Certs(); len(got) != 1 || got[0].CertFile != bCert {
			t.Fatalf("同名证书应更新路径: %+v", got)
		}
	} else {
		t.Fatal(err)
	}
}

// 证书库按证书里的域名起名:真实 CA 的证书名字是域名,自签的统一叫 self-signed。
func TestCertNameOf(t *testing.T) {
	r := certTestRunner(t)
	if err := r.SelfSign([]string{"1.2.3.4"}, CertTargets{Lines: true}); err != nil {
		t.Fatal(err)
	}
	got := r.Certs()
	if len(got) != 1 || got[0].Name != SelfSignedName || !got[0].ForLines {
		t.Fatalf("自签应叫 %s 且是默认: %+v", SelfSignedName, got)
	}
	// 重新生成是覆盖,不是多一张
	if err := r.SelfSign([]string{"5.6.7.8"}, CertTargets{}); err != nil {
		t.Fatal(err)
	}
	if got := r.Certs(); len(got) != 1 {
		t.Fatalf("自签只留一张: %+v", got)
	}
}

// 还在用的证书不让删;删掉没在用的:面板自己存的文件一并删,服务器上已有的原件不动。
func TestDeleteCert(t *testing.T) {
	r := certTestRunner(t)
	if err := r.SelfSign([]string{"1.2.3.4"}, CertTargets{Lines: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteCert(SelfSignedName); err == nil || !strings.Contains(err.Error(), "默认") {
		t.Fatalf("默认证书不该能删: %v", err)
	}
	extCert, extKey := genCert(t, "ext.example")
	if err := r.saveCertRow(model.Cert{Name: "ext", Source: "external", CertFile: extCert, KeyFile: extKey}); err != nil {
		t.Fatal(err)
	}
	// 线路单独选了它,不让删
	tls, _ := json.Marshal(map[string]string{"mode": "cert", "cert": "ext"})
	r.db.Create(&model.Line{Name: "l1", Protocol: "trojan", Port: 40001, Enabled: true, Tls: tls})
	if err := r.DeleteCert("ext"); err == nil || !strings.Contains(err.Error(), "l1") {
		t.Fatalf("线路还在用不该能删: %v", err)
	}
	r.db.Where("name = ?", "l1").Delete(&model.Line{})
	if err := r.DeleteCert("ext"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(extCert); err != nil {
		t.Fatal("服务器上已有的证书,原件不能删")
	}
	// 面板自己存的:先换掉默认,再删,文件一起没
	ownCert, _ := r.storedPaths(SelfSignedName)
	if err := r.saveCertRow(model.Cert{Name: "ext", Source: "external", CertFile: extCert, KeyFile: extKey}); err != nil {
		t.Fatal(err)
	}
	if err := r.UseCert("ext", CertForLines); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteCert(SelfSignedName); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ownCert); err == nil {
		t.Fatal("面板自己存的证书文件应一并删掉")
	}
}

// 老版本升上来:设置里引用的、数据目录 cert/ 下成对的证书都要收进库;文件名和内容对不上时按内容起名。
func TestEnsureCertStoreImportsOldFiles(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "cert")
	// 老版本把后签的域名写进了旧文件名
	old, oldKey := filepath.Join(certDir, "old.example.crt"), filepath.Join(certDir, "old.example.key")
	if err := certutil.GenerateSelfSigned([]string{"new.example"}, old, oldKey, 30); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Create(&model.Setting{Key: "certFile", Value: old})
	db.Create(&model.Setting{Key: "keyFile", Value: oldKey})
	db.Create(&model.Setting{Key: "certSource", Value: "selfsign"})
	database.Close(db)

	r, err := New(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.core.Stop(); database.Close(r.DB()) }()
	got := r.Certs()
	if len(got) != 1 || got[0].CertFile != old || !got[0].ForLines {
		t.Fatalf("老证书要收进库并标成默认: %+v", got)
	}
	// 再启动一次不重复
	r.ensureCertStore()
	if got := r.Certs(); len(got) != 1 {
		t.Fatalf("不能重复收录: %+v", got)
	}
	// 本机记录里有清单,线路编辑框和订阅用
	var local model.Node
	r.db.Where("is_local = ?", true).First(&local)
	var briefs []model.CertBrief
	if err := json.Unmarshal(local.Certs, &briefs); err != nil || len(briefs) != 1 || !briefs[0].Default {
		t.Fatalf("本机证书清单不对: %s %v", local.Certs, err)
	}
}

// 线路单独选了证书:入站用那一张;这台机器没有那张就用默认的,不让整份配置起不来。
func TestLineCertInRender(t *testing.T) {
	r := certTestRunner(t)
	defCert, defKey := genCert(t, "def.example")
	if err := r.UseExternalCert(defCert, defKey, CertTargets{Lines: true}); err != nil {
		t.Fatal(err)
	}
	other, otherKey := genCert(t, "other.example")
	if err := r.saveCertRow(model.Cert{Name: "*.other.example", Source: "external", CertFile: other, KeyFile: otherKey}); err != nil {
		t.Fatal(err)
	}
	cert := r.NodeCert()
	inboundCert := func(tlsJSON string) (string, string) {
		t.Helper()
		ib, err := render.InboundJSON(model.Line{Name: "l", Protocol: "trojan", Port: 1, Tls: []byte(tlsJSON)}, cert)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			TLS struct {
				Path string `json:"certificate_path"`
				Name string `json:"server_name"`
			} `json:"tls"`
		}
		json.Unmarshal(ib, &doc)
		return doc.TLS.Path, doc.TLS.Name
	}
	if p, _ := inboundCert(`{"mode":"cert"}`); p != defCert {
		t.Fatalf("没选证书要用默认的: %s", p)
	}
	p, name := inboundCert(`{"mode":"cert","cert":"*.other.example"}`)
	if p != other || name != "other.example" {
		t.Fatalf("选了证书要用它,server_name 不带 *: %s %s", p, name)
	}
	if p, _ := inboundCert(`{"mode":"cert","cert":"这台没有"}`); p != defCert {
		t.Fatalf("这台没有那张证书要退回默认: %s", p)
	}
	// 文件被删了也算没有
	os.Remove(other)
	cert = r.NodeCert()
	if p, _ := inboundCert(`{"mode":"cert","cert":"*.other.example"}`); p != defCert {
		t.Fatalf("文件没了要退回默认: %s", p)
	}
}
