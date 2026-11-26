package sub

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 实机(73 个组合逐个真连,tests/rs/t60-combos.mjs):VMess 配证书、证书是自签的,sing-box 订阅里的节点连不上
// (tls: failed to verify certificate),同一张证书的 trojan、vless 都通。原因:本站的 vmess 链接把「允许不安全」写成
// insecure(v2rayN 系认这个键),而 sing-box 订阅是把链接再解析成出站,解析那头只认 allowInsecure —— 自己写的自己读不出来。
// 现在两个键都认:自签时 sing-box 订阅里的 VMess 节点带 tls.insecure,和 trojan 一致;正式证书都不带。
func TestVMessCertLineInsecureInSingBoxSub(t *testing.T) {
	user := model.User{Name: "alice", Credentials: []byte(`{"vmess":{"uuid":"5b0e2a9c-3f1d-4c6b-8a7e-2d4c6e8f0a1b"},"trojan":{"password":"tp"}}`)}
	lines := []model.Line{
		{Id: 1, Name: "vm", Protocol: "vmess", Port: 8443, Enabled: true, Tls: []byte(`{"mode":"cert"}`)},
		{Id: 2, Name: "vmws", Protocol: "vmess", Port: 8444, Enabled: true, Tls: []byte(`{"mode":"cert"}`), Transport: []byte(`{"type":"ws","path":"/w"}`)},
		{Id: 3, Name: "tj", Protocol: "trojan", Port: 9443, Enabled: true, Tls: []byte(`{"mode":"cert"}`)},
		{Id: 4, Name: "vmplain", Protocol: "vmess", Port: 8445, Enabled: true, Tls: []byte(`{"mode":"none"}`)},
	}
	for _, tc := range []struct {
		name     string
		insecure bool
	}{{"自签", true}, {"正式证书", false}} {
		t.Run(tc.name, func(t *testing.T) {
			entries := []Entry{{Host: "203.0.113.7", SNI: "hk.example.com", Insecure: tc.insecure}}
			// 链接里的写法不变(v2rayN 系认 insecure)
			for _, l := range GenerateLinksFor(user, lines[:1], entries, nil) {
				b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(l, "vmess://"))
				if err != nil {
					t.Fatalf("vmess 链接: %v", err)
				}
				var obj map[string]interface{}
				if err := json.Unmarshal(b, &obj); err != nil {
					t.Fatal(err)
				}
				if got := obj["insecure"] == "1"; got != tc.insecure {
					t.Fatalf("vmess 链接的 insecure 应为 %v: %s", tc.insecure, b)
				}
			}
			res, err := BuildSingBoxSub(user, lines, Options{Entries: entries})
			if err != nil {
				t.Fatal(err)
			}
			var sb struct {
				Outbounds []map[string]interface{} `json:"outbounds"`
			}
			if err := json.Unmarshal([]byte(res.Body), &sb); err != nil {
				t.Fatal(err)
			}
			tlsOf := map[string]map[string]interface{}{}
			for _, o := range sb.Outbounds {
				tag, _ := o["tag"].(string)
				tl, _ := o["tls"].(map[string]interface{})
				tlsOf[tag] = tl
			}
			for _, tag := range []string{"vm", "vmws", "tj"} {
				tl := tlsOf[tag]
				if tl == nil {
					t.Fatalf("%s 应有 TLS: %v", tag, tlsOf)
				}
				if got, _ := tl["insecure"].(bool); got != tc.insecure {
					t.Fatalf("%s 的 tls.insecure 应为 %v(trojan 是 %v): %v", tag, tc.insecure, tlsOf["tj"]["insecure"], tl)
				}
				if tl["server_name"] != "hk.example.com" {
					t.Fatalf("%s 的 server_name 应是 SNI: %v", tag, tl)
				}
			}
			if tlsOf["vmplain"] != nil {
				t.Fatalf("不加密的 VMess 不该有 TLS: %v", tlsOf["vmplain"])
			}
		})
	}
}
