package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// m-ui render 和运行时用同一套证书取法(runner.NodeCertOf):线路默认证书先取 certFile / keyFile(证书页设的),
// 没设才回落到面板证书 webCertFile;线路单独选的证书库证书按名字找。以前 render 只看 webCertFile,
// 两个设置不一致时校验的和实际跑的不是同一份配置。
func TestRenderUsesRuntimeCertPriority(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.db")
	db, err := database.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true, PublicIP: "203.0.113.1"})
	db.Create(&model.Line{Name: "hy2", Protocol: "hysteria2", Port: 30443, Enabled: true})
	sc, sk := filepath.Join(dir, "store.crt"), filepath.Join(dir, "store.key")
	os.WriteFile(sc, []byte("x"), 0o600)
	os.WriteFile(sk, []byte("x"), 0o600)
	db.Create(&model.Cert{Name: "s.example.com", Source: "external", CertFile: sc, KeyFile: sk})
	db.Create(&model.Line{Name: "named", Protocol: "trojan", Port: 30444, Enabled: true, Tls: []byte(`{"mode":"cert","cert":"s.example.com"}`)})
	for k, v := range map[string]string{"webDomain": "a.example.com", "certFile": "/line/a.crt", "keyFile": "/line/a.key",
		"webCertFile": "/panel/p.crt", "webKeyFile": "/panel/p.key"} {
		db.Create(&model.Setting{Key: k, Value: v})
	}
	database.Close(db)

	out := filepath.Join(dir, "cfg.json")
	if err := runRender(p, out, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	cfg := string(raw)
	if !strings.Contains(cfg, `"/line/a.crt"`) || strings.Contains(cfg, "/panel/p.crt") {
		t.Fatalf("render 应和运行时一样先用 certFile,不该用面板证书:\n%s", cfg)
	}
	if !strings.Contains(cfg, filepath.ToSlash(sc)) && !strings.Contains(cfg, strings.ReplaceAll(sc, `\`, `\`)) {
		t.Fatalf("线路单独选的证书库证书应按名字用上:\n%s", cfg)
	}

	// 没设 certFile:两边都回落到面板证书
	db, err = database.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	db.Where("key IN ?", []string{"certFile", "keyFile"}).Delete(&model.Setting{})
	database.Close(db)
	if err := runRender(p, out, false); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(out); !strings.Contains(string(raw), `"/panel/p.crt"`) {
		t.Fatalf("没设 certFile 时应回落到面板证书:\n%s", raw)
	}
}
