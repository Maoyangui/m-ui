package sub

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database/model"

	"gopkg.in/yaml.v3"
)

func fullUser() model.User {
	c := creds.Generate("alice")
	c["vless"]["uuid"] = "11111111-2222-3333-4444-555555555555"
	c["vmess"]["uuid"] = "11111111-2222-3333-4444-555555555555"
	c["tuic"]["uuid"] = "11111111-2222-3333-4444-555555555555"
	c["tuic"]["password"] = "tp"
	c["trojan"]["password"] = "trp"
	c["socks"]["password"] = "sp"
	c["http"]["password"] = "hp"
	b, _ := json.Marshal(c)
	return model.User{Name: "alice", Enabled: true, Credentials: b}
}

func raw(v interface{}) json.RawMessage { b, _ := json.Marshal(v); return b }

var realityTLS = raw(map[string]interface{}{"mode": "reality", "reality": map[string]interface{}{
	"private_key": "priv", "public_key": "PUBKEY", "short_ids": []string{"abcd1234"},
	"handshake_server": "www.microsoft.com", "handshake_port": 443}})

func TestVlessRealityVisionLink(t *testing.T) {
	line := model.Line{Name: "美国-vless", Protocol: "vless", Port: 443, Tls: realityTLS, Options: raw(map[string]interface{}{"vision": true})}
	got := GenerateLinks(fullUser(), []model.Line{line}, hkEntry)[0]
	want := "vless://11111111-2222-3333-4444-555555555555@hk.joinvip.vip:443?type=tcp&security=reality&pbk=PUBKEY&sid=abcd1234&fp=chrome&sni=www.microsoft.com&flow=xtls-rprx-vision#%E7%BE%8E%E5%9B%BD-vless"
	if got != want {
		t.Fatalf("\nwant %s\n got %s", want, got)
	}
}

func TestVlessWsCertNoVisionLink(t *testing.T) {
	// 有传输时 vision 必须被剥离
	line := model.Line{Name: "ws", Protocol: "vless", Port: 443,
		Tls:       raw(map[string]interface{}{"mode": "cert"}),
		Transport: raw(map[string]interface{}{"type": "ws", "path": "/chat", "headers": map[string]interface{}{"Host": "cdn.example"}}),
		Options:   raw(map[string]interface{}{"vision": true})}
	got := GenerateLinks(fullUser(), []model.Line{line}, hkEntry)[0]
	if !strings.Contains(got, "type=ws&path=%2Fchat&host=cdn.example&security=tls&sni=hk.joinvip.vip") || strings.Contains(got, "flow=") {
		t.Fatalf("ws+cert 链接不符: %s", got)
	}
}

func TestTrojanGrpcLink(t *testing.T) {
	line := model.Line{Name: "tj", Protocol: "trojan", Port: 8443, Transport: raw(map[string]interface{}{"type": "grpc", "service_name": "svc"})}
	got := GenerateLinks(fullUser(), []model.Line{line}, hkEntry)[0]
	if got != "trojan://trp@hk.joinvip.vip:8443?type=grpc&serviceName=svc&security=tls&sni=hk.joinvip.vip#tj" {
		t.Fatalf("trojan 链接不符: %s", got)
	}
}

func TestTuicLink(t *testing.T) {
	line := model.Line{Name: "tu", Protocol: "tuic", Port: 9443, Options: raw(map[string]interface{}{"congestion_control": "bbr"})}
	got := GenerateLinks(fullUser(), []model.Line{line}, hkEntry)[0]
	if got != "tuic://11111111-2222-3333-4444-555555555555:tp@hk.joinvip.vip:9443?security=tls&sni=hk.joinvip.vip&congestion_control=bbr&udp_relay_mode=native&alpn=h3#tu" {
		t.Fatalf("tuic 链接不符: %s", got)
	}
}

func TestVmessWsLink(t *testing.T) {
	line := model.Line{Name: "vm", Protocol: "vmess", Port: 80, Transport: raw(map[string]interface{}{"type": "ws", "path": "/v", "headers": map[string]interface{}{"Host": "h.example"}})}
	got := GenerateLinks(fullUser(), []model.Line{line}, hkEntry)[0]
	if !strings.HasPrefix(got, "vmess://") {
		t.Fatal(got)
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, "vmess://"))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(dec, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["net"] != "ws" || obj["path"] != "/v" || obj["host"] != "h.example" || obj["tls"] != "none" || obj["port"] != "80" || obj["ps"] != "vm" {
		t.Fatalf("vmess 字段不符: %v", obj)
	}
}

func TestMixedProducesTwoLinks(t *testing.T) {
	line := model.Line{Name: "mx", Protocol: "mixed", Port: 1080}
	got := GenerateLinks(fullUser(), []model.Line{line}, hkEntry)
	if len(got) != 2 || !strings.HasPrefix(got[0], "socks5://alice:sp@hk.joinvip.vip:1080") || !strings.HasPrefix(got[1], "http://alice:hp@hk.joinvip.vip:1080") {
		t.Fatalf("mixed 应出 socks5+http 两条: %v", got)
	}
}

func TestClashNewProtocols(t *testing.T) {
	lines := []model.Line{
		{Name: "vr", Protocol: "vless", Port: 443, Tls: realityTLS, Options: raw(map[string]interface{}{"vision": true})},
		{Name: "tj", Protocol: "trojan", Port: 8443, Transport: raw(map[string]interface{}{"type": "ws", "path": "/w"})},
		{Name: "tu", Protocol: "tuic", Port: 9443},
		{Name: "vm", Protocol: "vmess", Port: 80},
		{Name: "mx", Protocol: "mixed", Port: 1080},
	}
	out, err := BuildClash(fullUser(), lines, hkEntry, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(out), &root); err != nil {
		t.Fatal(err)
	}
	proxies := root["proxies"].([]interface{})
	if len(proxies) != 6 { // 4 + mixed 的 2
		t.Fatalf("代理数应为 6,实际 %d", len(proxies))
	}
	vr := findProxy(proxies, "vr")
	ro, _ := vr["reality-opts"].(map[string]interface{})
	if vr["type"] != "vless" || vr["flow"] != "xtls-rprx-vision" || vr["servername"] != "www.microsoft.com" || ro["public-key"] != "PUBKEY" || ro["short-id"] != "abcd1234" || vr["client-fingerprint"] != "chrome" {
		t.Fatalf("vless reality 代理不符: %v", vr)
	}
	tj := findProxy(proxies, "tj")
	wso, _ := tj["ws-opts"].(map[string]interface{})
	if tj["type"] != "trojan" || tj["network"] != "ws" || wso["path"] != "/w" || tj["tls"] != true {
		t.Fatalf("trojan ws 代理不符: %v", tj)
	}
	tu := findProxy(proxies, "tu")
	if tu["type"] != "tuic" || tu["password"] != "tp" || tu["congestion-controller"] != "cubic" {
		t.Fatalf("tuic 代理不符: %v", tu)
	}
	vm := findProxy(proxies, "vm")
	if vm["type"] != "vmess" || vm["cipher"] != "auto" || vm["tls"] == true {
		t.Fatalf("vmess 代理不符: %v", vm)
	}
	if findProxy(proxies, "mx-socks")["type"] != "socks5" || findProxy(proxies, "mx-http")["type"] != "http" {
		t.Fatal("mixed 应出 socks5 与 http 两个代理")
	}
}
