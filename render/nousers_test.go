package render

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 线路一个用户都没有(新建还没分配、用户全停了、停用 / 删除副机时主机推的空用户表)也要渲染得出来、被内核接受:
// 副机应用快照时数据面起不来,会被当成应用失败,下线通知就发不到。
func TestEveryProtocolConstructsWithoutUsers(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "main.crt"), filepath.Join(dir, "main.key")
	if err := certutil.GenerateSelfSigned([]string{"hk.test"}, crt, key, 7); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	priv, pub, err := creds.RealityKeypair()
	if err != nil {
		t.Fatal(err)
	}
	reality := raw(map[string]interface{}{"mode": "reality", "reality": map[string]interface{}{
		"private_key": priv, "public_key": pub, "short_ids": []string{creds.ShortID()},
		"handshake_server": "www.microsoft.com", "handshake_port": 443}})
	cert := raw(map[string]interface{}{"mode": "cert"})
	lines := []model.Line{
		{Name: "hy2", Protocol: "hysteria2", Port: 21001},
		{Name: "anytls", Protocol: "anytls", Port: 21002},
		{Name: "tuic", Protocol: "tuic", Port: 21003},
		{Name: "trojan-ws", Protocol: "trojan", Port: 21004, Transport: raw(map[string]interface{}{"type": "ws", "path": "/t"})},
		{Name: "vless-reality", Protocol: "vless", Port: 21005, Tls: reality, Options: raw(map[string]interface{}{"vision": true})},
		{Name: "vmess-grpc", Protocol: "vmess", Port: 21006, Tls: cert, Transport: raw(map[string]interface{}{"type": "grpc", "service_name": "g"})},
		{Name: "ss2022", Protocol: "shadowsocks", Port: 21007, Options: raw(map[string]interface{}{"method": "2022-blake3-aes-128-gcm", "password": creds.Base64Key(16)})},
		{Name: "ss", Protocol: "shadowsocks", Port: 21008, Options: raw(map[string]interface{}{"method": "aes-256-gcm", "password": creds.Base64Key(32)})},
		{Name: "socks", Protocol: "socks", Port: 21009},
		{Name: "http", Protocol: "http", Port: 21010},
		{Name: "mixed", Protocol: "mixed", Port: 21011},
	}
	for i := range lines {
		lines[i].Enabled, lines[i].Sort = true, i+1
		if err := db.Create(&lines[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := BuildConfig(db, NodeCert{ServerName: "hk.test", CertPath: crt, KeyPath: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.ValidateConfig(cfg); err != nil {
		t.Fatalf("没有用户时 sing-box 拒绝渲染结果: %v\n%s", err, cfg)
	}
}
