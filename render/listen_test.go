package render

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func inboundListens(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var cfg struct {
		Inbounds []map[string]interface{} `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, in := range cfg.Inbounds {
		tag, _ := in["tag"].(string)
		out[tag], _ = in["listen"].(string)
	}
	return out
}

// 回归:所有协议的入站都监听 ::(双栈,IPv4 也收),和线路选 IPv4 还是 IPv6 无关 —— 线路的 IP 版本只影响订阅。
// 内核关了 IPv6(没有 /proc/net/if_inet6)时退回 0.0.0.0,不然整份配置的入站一起起不来;除了 listen 别的一个字节都不变。
func TestAllInboundsListenDualStack(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true, PublicIP: "203.0.113.1", PublicIP6: "2001:db8::1"})
	protocols := []string{"hysteria2", "anytls", "tuic", "trojan", "vless", "vmess", "shadowsocks", "socks", "http", "mixed"}
	for i, p := range protocols {
		l := model.Line{Name: p, Protocol: p, Port: 30400 + i, Enabled: true}
		if i%2 == 0 {
			l.NodeFamily = json.RawMessage(`{"1":"v6"}`)
		} else if i%3 == 0 {
			l.NodeFamily = json.RawMessage(`{"1":"v4"}`)
		}
		switch p {
		case "shadowsocks":
			l.Options = []byte(`{"method":"aes-256-gcm","password":"x"}`)
		case "vless":
			l.Tls = []byte(`{"mode":"reality","reality":{"private_key":"k","handshake_server":"www.apple.com","short_ids":["ab"]}}`)
		}
		db.Create(&l)
	}
	old := noIPv6Stack
	defer func() { noIPv6Stack = old }()
	noIPv6Stack = func() bool { return false } // 常规机器(不看跑测试这台的内核)
	cert := NodeCert{ServerName: "a.example.com", CertPath: "/c", KeyPath: "/k"}
	raw, err := BuildConfig(db, cert)
	if err != nil {
		t.Fatal(err)
	}
	got := inboundListens(t, raw)
	if len(got) != len(protocols) {
		t.Fatalf("入站数不对: %v", got)
	}
	for tag, l := range got {
		if l != "::" {
			t.Errorf("%s 的 listen = %q,应为 ::", tag, l)
		}
	}

	noIPv6Stack = func() bool { return true }
	raw4, err := BuildConfig(db, cert)
	if err != nil {
		t.Fatal(err)
	}
	for tag, l := range inboundListens(t, raw4) {
		if l != "0.0.0.0" {
			t.Errorf("没有 IPv6 协议栈时 %s 的 listen = %q,应为 0.0.0.0", tag, l)
		}
	}
	if !bytes.Equal(bytes.ReplaceAll(raw4, []byte(`"listen": "0.0.0.0"`), []byte(`"listen": "::"`)), raw) {
		t.Fatal("除了 listen,配置别的地方不该变")
	}
}

// 退回 0.0.0.0 的配置要能过 sing-box 干跑(只收 IPv4)。
func TestListenFallbackWithoutIPv6Stack(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Line{Name: "ss", Protocol: "shadowsocks", Port: 30451, Enabled: true, Options: []byte(`{"method":"aes-256-gcm","password":"x"}`)})
	old := noIPv6Stack
	noIPv6Stack = func() bool { return true }
	defer func() { noIPv6Stack = old }()
	raw, err := BuildConfig(db, NodeCert{})
	if err != nil {
		t.Fatal(err)
	}
	if got := inboundListens(t, raw); got["ss"] != "0.0.0.0" {
		t.Fatalf("listen = %v", got)
	}
	if err := core.ValidateConfig(raw); err != nil {
		t.Fatalf("0.0.0.0 的配置应通过干跑: %v", err)
	}
	if s := listenAddr(); s != "0.0.0.0" {
		t.Fatalf("listenAddr = %q", s)
	}
}
