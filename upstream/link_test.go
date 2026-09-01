package upstream

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func mustParse(t *testing.T, link string) *Parsed {
	t.Helper()
	p, err := ParseLink(link)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", link, err)
	}
	return p
}

// 与真实库中 tuic 上游形态一致:uuid/password/cubic/tls{sni,alpn h3}
func TestParseTuic(t *testing.T) {
	p := mustParse(t, "tuic://839d49de-2c75-41e8-af44-2a9736f2d332:839d49de-2c75-41e8-af44-2a9736f2d332@jphaa.629630.xyz:29840?congestion_control=cubic&udp_relay_mode=native&alpn=h3&sni=jphaa.629630.xyz#%5BHy2%5D%E6%97%A5%E6%9C%AC")
	if p.Type != "tuic" || p.Name != "[Hy2]日本" {
		t.Fatalf("type/name 不符: %s %s", p.Type, p.Name)
	}
	o := p.Options
	if o["server"] != "jphaa.629630.xyz" || o["server_port"] != 29840 || o["uuid"] != "839d49de-2c75-41e8-af44-2a9736f2d332" {
		t.Fatalf("server/port/uuid 不符: %+v", o)
	}
	if o["congestion_control"] != "cubic" || o["udp_relay_mode"] != "native" {
		t.Fatalf("cc/relay 不符: %+v", o)
	}
	tls := o["tls"].(map[string]interface{})
	if tls["server_name"] != "jphaa.629630.xyz" || tls["enabled"] != true {
		t.Fatalf("tls 不符: %+v", tls)
	}
	if alpn := tls["alpn"].([]string); len(alpn) != 1 || alpn[0] != "h3" {
		t.Fatalf("alpn 不符: %v", tls["alpn"])
	}
}

func TestParseTuicDefaults(t *testing.T) {
	p := mustParse(t, "tuic://u:p@host.example:443")
	if p.Options["congestion_control"] != "cubic" {
		t.Fatal("缺省拥塞控制应为 cubic")
	}
	tls := p.Options["tls"].(map[string]interface{})
	if tls["server_name"] != "host.example" {
		t.Fatal("sni 缺省应为主机名")
	}
	if p.Name != "host.example" {
		t.Fatal("无备注时名称应为主机名")
	}
}

func TestParseHysteria2(t *testing.T) {
	p := mustParse(t, "hy2://pw123@jp2.426624.xyz:30770?sni=jp2.426624.xyz&insecure=1&obfs=salamander&obfs-password=ob&upmbps=50&downmbps=200#jp")
	if p.Type != "hysteria2" || p.Options["password"] != "pw123" {
		t.Fatalf("hy2 基本字段不符: %+v", p)
	}
	tls := p.Options["tls"].(map[string]interface{})
	if tls["insecure"] != true {
		t.Fatal("insecure 应为 true")
	}
	if p.Options["up_mbps"] != 50 || p.Options["down_mbps"] != 200 {
		t.Fatalf("带宽字段不符: %+v", p.Options)
	}
	obfs := p.Options["obfs"].(map[string]interface{})
	if obfs["type"] != "salamander" || obfs["password"] != "ob" {
		t.Fatalf("obfs 不符: %+v", obfs)
	}
}

func TestParseShadowsocksBase64(t *testing.T) {
	// SIP002:userinfo = base64(method:password),与真实库中 es-au 上游一致
	userinfo := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:cj44mnrW5gcL+aC0T28cHzNytsl09IqpkycmMK57qQk="))
	p := mustParse(t, "ss://"+userinfo+"@68.221.25.1:17364#es-au")
	if p.Type != "shadowsocks" || p.Name != "es-au" {
		t.Fatalf("type/name 不符: %+v", p)
	}
	if p.Options["method"] != "aes-256-gcm" || p.Options["password"] != "cj44mnrW5gcL+aC0T28cHzNytsl09IqpkycmMK57qQk=" {
		t.Fatalf("method/password 不符: %+v", p.Options)
	}
	if p.Options["server"] != "68.221.25.1" || p.Options["server_port"] != 17364 {
		t.Fatalf("server/port 不符: %+v", p.Options)
	}
}

func TestParseShadowsocksPlain(t *testing.T) {
	p := mustParse(t, "ss://chacha20-ietf-poly1305:secret@1.2.3.4:8388#plain")
	if p.Options["method"] != "chacha20-ietf-poly1305" || p.Options["password"] != "secret" {
		t.Fatalf("plain ss 不符: %+v", p.Options)
	}
}

func TestParseSocks(t *testing.T) {
	p := mustParse(t, "socks5://user1:pass1@us.arxlabs.io:3010#%E5%AE%B6%E5%AE%BD")
	if p.Type != "socks" || p.Name != "家宽" || p.Options["username"] != "user1" || p.Options["password"] != "pass1" || p.Options["version"] != "5" {
		t.Fatalf("socks 不符: %+v", p)
	}
	// WARP 本地代理:无认证
	p = mustParse(t, "socks5://127.0.0.1:40000#warp")
	if _, has := p.Options["username"]; has {
		t.Fatal("无认证 socks 不应带 username")
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"", "vmess://abc", "tuic://@host:1", "ss://notbase64@h:1", "http://x"} {
		if _, err := ParseLink(bad); err == nil {
			t.Errorf("%q 应解析失败", bad)
		}
	}
}

func TestServerAddr(t *testing.T) {
	addr, err := ServerAddr(json.RawMessage(`{"server":"127.0.0.1","server_port":40000,"version":"5"}`))
	if err != nil || addr != "127.0.0.1:40000" {
		t.Fatalf("ServerAddr 不符: %s %v", addr, err)
	}
	if _, err := ServerAddr(json.RawMessage(`{"server":""}`)); err == nil {
		t.Fatal("缺 server 应报错")
	}
}
