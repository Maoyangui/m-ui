package render

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/fangjunsheng555/m-ui/certutil"
	"github.com/fangjunsheng555/m-ui/core"
	"github.com/fangjunsheng555/m-ui/creds"
	"github.com/fangjunsheng555/m-ui/database"
	"github.com/fangjunsheng555/m-ui/database/model"
)

func raw(v interface{}) json.RawMessage { b, _ := json.Marshal(v); return b }

// 每种协议 × TLS 模式 × 传输 各建一条线路,渲染后交给 sing-box 干跑构造(不监听):
// 全部通过即证明渲染器产出的每一种入站都是 sing-box 能真实创建的。
func TestEveryProtocolRendersAndConstructs(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "main.crt"), filepath.Join(dir, "main.key")
	if err := certutil.GenerateSelfSigned([]string{"hk.test"}, crt, key, 7); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db) // Windows 上不关句柄 TempDir 清理会失败
	priv, pub, err := creds.RealityKeypair()
	if err != nil {
		t.Fatal(err)
	}
	reality := raw(map[string]interface{}{"mode": "reality", "reality": map[string]interface{}{
		"private_key": priv, "public_key": pub, "short_ids": []string{creds.ShortID()},
		"handshake_server": "www.microsoft.com", "handshake_port": 443}})
	cert := raw(map[string]interface{}{"mode": "cert"})
	none := raw(map[string]interface{}{"mode": "none"})

	up := model.Upstream{Name: "warp", Type: "socks", Options: raw(map[string]interface{}{"server": "127.0.0.1", "server_port": 40000, "version": "5"})}
	db.Create(&up)
	lines := []model.Line{
		{Name: "hy2", Protocol: "hysteria2", Port: 20001, UpstreamId: up.Id},
		{Name: "anytls", Protocol: "anytls", Port: 20002, Options: raw(map[string]interface{}{"padding_scheme": []string{"stop=8", "0=30-30"}})},
		{Name: "tuic", Protocol: "tuic", Port: 20003, Options: raw(map[string]interface{}{"congestion_control": "bbr"})},
		{Name: "trojan-ws", Protocol: "trojan", Port: 20004, Transport: raw(map[string]interface{}{"type": "ws", "path": "/t"})},
		{Name: "vless-reality", Protocol: "vless", Port: 20005, Tls: reality, Options: raw(map[string]interface{}{"vision": true})},
		{Name: "vless-grpc", Protocol: "vless", Port: 20006, Tls: cert, Transport: raw(map[string]interface{}{"type": "grpc", "service_name": "g"})},
		{Name: "vmess-ws", Protocol: "vmess", Port: 20007, Tls: none, Transport: raw(map[string]interface{}{"type": "ws", "path": "/v", "headers": map[string]interface{}{"Host": "h"}})},
		{Name: "vmess-httpupgrade", Protocol: "vmess", Port: 20008, Tls: cert, Transport: raw(map[string]interface{}{"type": "httpupgrade", "path": "/u"})},
		{Name: "ss", Protocol: "shadowsocks", Port: 20009, Options: raw(map[string]interface{}{"method": "aes-256-gcm", "password": creds.Base64Key(32)})},
		{Name: "ss2022", Protocol: "shadowsocks", Port: 20010, Options: raw(map[string]interface{}{"method": "2022-blake3-aes-128-gcm", "password": creds.Base64Key(16)})},
		{Name: "socks", Protocol: "socks", Port: 20011},
		{Name: "http-tls", Protocol: "http", Port: 20012},
		{Name: "mixed", Protocol: "mixed", Port: 20013},
	}
	for i := range lines {
		lines[i].Enabled = true
		lines[i].Sort = i + 1
		if err := db.Create(&lines[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	u := model.User{Name: "alice", Enabled: true, Credentials: raw(creds.Generate("alice"))}
	db.Create(&u)
	for _, l := range lines {
		db.Create(&model.UserLine{UserId: u.Id, LineId: l.Id})
	}

	cfg, err := BuildConfig(db, NodeCert{ServerName: "hk.test", CertPath: crt, KeyPath: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.ValidateConfig(cfg); err != nil {
		t.Fatalf("sing-box 拒绝渲染结果: %v\n%s", err, cfg)
	}

	// 抽查关键字段
	var parsed struct {
		Inbounds []map[string]interface{} `json:"inbounds"`
	}
	json.Unmarshal(cfg, &parsed)
	byTag := map[string]map[string]interface{}{}
	for _, in := range parsed.Inbounds {
		byTag[in["tag"].(string)] = in
	}
	vr := byTag["vless-reality"]
	if vr["tls"].(map[string]interface{})["reality"] == nil {
		t.Fatal("vless-reality 应带 reality 块")
	}
	if vr["users"].([]interface{})[0].(map[string]interface{})["flow"] != "xtls-rprx-vision" {
		t.Fatal("vless-reality 用户应带 vision flow")
	}
	if byTag["vless-grpc"]["users"].([]interface{})[0].(map[string]interface{})["flow"] != nil {
		t.Fatal("有传输的 vless 不应带 flow")
	}
	if byTag["vmess-ws"]["tls"] != nil {
		t.Fatal("vmess none 模式不应有 tls")
	}
	if byTag["mixed"]["users"].([]interface{})[0].(map[string]interface{})["username"] != "alice" {
		t.Fatal("mixed 用户应为 username/password 形态")
	}
	if byTag["ss2022"]["users"].([]interface{})[0].(map[string]interface{})["password"] == byTag["ss"]["users"].([]interface{})[0].(map[string]interface{})["password"] {
		t.Fatal("ss2022 应使用 shadowsocks16 凭据")
	}
}

func TestTLSRequiredProtocolRejectsNone(t *testing.T) {
	line := model.Line{Name: "x", Protocol: "hysteria2", Port: 1, Tls: raw(map[string]interface{}{"mode": "none"})}
	if _, err := InboundJSON(line, NodeCert{}); err == nil {
		t.Fatal("hysteria2 关闭 TLS 应被拒绝")
	}
}

func TestRealityRequiresKeyAndServer(t *testing.T) {
	line := model.Line{Name: "x", Protocol: "vless", Port: 1, Tls: raw(map[string]interface{}{"mode": "reality"})}
	if _, err := InboundJSON(line, NodeCert{}); err == nil {
		t.Fatal("reality 缺参数应被拒绝")
	}
}
