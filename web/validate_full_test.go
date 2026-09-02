package web

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
)

// 复现真机事故:ss2022 线路没有服务端 PSK 时,parse 能过、初始化报 "missing psk",
// 保存前的整配置干跑必须把它拦下来;validateLine 会自动补 PSK 后则通过。
func TestFullConfigDryRunCatchesMissingPSKAndValidateLineFillsIt(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	crt, key := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	if err := certutil.GenerateSelfSigned([]string{"t.example.com"}, crt, key, 10); err != nil {
		t.Fatal(err)
	}
	cert := render.NodeCert{ServerName: "t.example.com", CertPath: crt, KeyPath: key}
	db.Create(&model.User{Name: "u", Enabled: true, Credentials: []byte(`{"shadowsocks":{"password":"` + randomBase64(32) + `"},"shadowsocks16":{"password":"` + randomBase64(16) + `"}}`)})

	// 直接写库一条缺 PSK 的 ss2022 线路 → 整体干跑必须报错
	bad := model.Line{Name: "ss2022", Protocol: "shadowsocks", Port: 30445, Enabled: true, Options: []byte(`{"method":"2022-blake3-aes-128-gcm"}`)}
	db.Create(&bad)
	db.Create(&model.UserLine{UserId: 1, LineId: bad.Id})
	err = validateFullConfig(db, cert)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "psk") {
		t.Fatalf("应报 missing psk: %v", err)
	}

	// validateLine 自动补 PSK(aes-128 → 16 字节)
	s := &Server{db: db}
	fixed := model.Line{Name: "ss2022-ok", Protocol: "shadowsocks", Port: 30446, Enabled: true, Options: []byte(`{"method":"2022-blake3-aes-128-gcm"}`)}
	if err := s.validateLine(&fixed); err != nil {
		t.Fatal(err)
	}
	var opts map[string]interface{}
	json.Unmarshal(fixed.Options, &opts)
	psk, _ := opts["password"].(string)
	if len(psk) != 24 { // base64(16 字节) = 24 字符
		t.Fatalf("应自动生成 16 字节 PSK,实际 %q", psk)
	}
	db.Delete(&bad)
	db.Create(&fixed)
	db.Create(&model.UserLine{UserId: 1, LineId: fixed.Id})
	if err := validateFullConfig(db, cert); err != nil {
		t.Fatalf("补 PSK 后应通过: %v", err)
	}
	// aes-256 → 32 字节
	big := model.Line{Name: "ss2022-256", Protocol: "shadowsocks", Port: 30447, Options: []byte(`{"method":"2022-blake3-aes-256-gcm"}`)}
	s.validateLine(&big)
	json.Unmarshal(big.Options, &opts)
	if p, _ := opts["password"].(string); len(p) != 44 {
		t.Fatalf("aes-256 应生成 32 字节 PSK,实际 %q", p)
	}
}
