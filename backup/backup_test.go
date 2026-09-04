package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func TestCreateInspectRestore(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	db, err := database.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	db.Create(&model.User{Name: "alice", Enabled: true})
	db.Create(&model.User{Name: "bob", Enabled: true})
	db.Create(&model.Line{Name: "l1", Protocol: "hysteria2", Port: 1234})
	db.Create(&model.Setting{Key: "webDomain", Value: "hk.example.com"})
	certPath := filepath.Join(dir, "new", "cert", "hk.crt") // 与还原目标同一数据目录
	os.MkdirAll(filepath.Dir(certPath), 0o755)
	os.WriteFile(certPath, []byte("CERT"), 0o644)

	var buf bytes.Buffer
	if err := Create(db, []string{certPath, "", certPath, filepath.Join(dir, "missing.key")}, "1.2.3", &buf); err != nil {
		t.Fatal(err)
	}
	database.Close(db)

	zipPath := filepath.Join(dir, "b.zip")
	os.WriteFile(zipPath, buf.Bytes(), 0o600)
	s, err := Inspect(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsZip || s.Users != 2 || s.Lines != 1 || s.Meta.Version != "1.2.3" || len(s.Meta.Certs) != 1 {
		t.Fatalf("摘要不符: %+v", s)
	}

	// 还原到新路径;数据目录内的证书应写回原绝对路径
	os.Remove(certPath)
	dst := filepath.Join(dir, "new", "m-ui.db")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(dst, []byte("old"), 0o600)
	if err := Restore(dst, zipPath); err != nil {
		t.Fatal(err)
	}
	db2, err := database.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	db2.Model(&model.User{}).Count(&n)
	database.Close(db2)
	if n != 2 {
		t.Fatalf("还原后用户数 %d", n)
	}
	if b, _ := os.ReadFile(certPath); string(b) != "CERT" {
		t.Fatal("证书未写回原路径")
	}
	// 数据目录之外的路径不写(不可信的备份不能借此往任意位置落文件)
	outside := filepath.Join(dir, "outside.crt")
	os.WriteFile(outside, []byte("OLD"), 0o644)
	var buf2 bytes.Buffer
	db3, _ := database.Open(src)
	if err := Create(db3, []string{outside}, "1.2.3", &buf2); err != nil {
		t.Fatal(err)
	}
	database.Close(db3)
	zip2 := filepath.Join(dir, "b2.zip")
	os.WriteFile(zip2, buf2.Bytes(), 0o600)
	os.WriteFile(outside, []byte("KEEP"), 0o644)
	if err := Restore(dst, zip2); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "KEEP" {
		t.Fatal("数据目录外的证书路径不应被备份覆盖")
	}

	baks, _ := filepath.Glob(dst + ".bak-*")
	if len(baks) == 0 {
		t.Fatal("旧库应保留为 .bak-*")
	}

	// Stage + ApplyPending
	if err := Stage(dst, zipPath); err != nil {
		t.Fatal(err)
	}
	if PendingPath(dst) == "" {
		t.Fatal("应存在待还原文件")
	}
	applied, err := ApplyPending(dst)
	if err != nil || !applied || PendingPath(dst) != "" {
		t.Fatalf("ApplyPending: %v %v", applied, err)
	}

	// 裸 db 也可作为备份
	raw := filepath.Join(dir, "raw.db")
	os.Rename(src, raw)
	s2, err := Inspect(raw)
	if err != nil || s2.IsZip || s2.Users != 2 {
		t.Fatalf("裸 db 检查: %v %+v", err, s2)
	}
	// 垃圾文件被拒
	junk := filepath.Join(dir, "junk.bin")
	os.WriteFile(junk, []byte("hello"), 0o600)
	if _, err := Inspect(junk); err == nil {
		t.Fatal("垃圾文件应被拒")
	}
}
