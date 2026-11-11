package sub

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// Azure 真机:带证书的 HTTP 线路(https 代理)在自签证书时,sing-box 订阅里不带"允许不安全",
// 客户端报 x509 unknown authority 连不上;同一份订阅里 trojan、hy2 都带了,Clash 订阅也带了 skip-cert-verify。
// 判定和其它 TLS 协议一致:自签 / 设置了允许不安全才带;分享链接同样补上(allowInsecure=1),SNI 也照 Clash 那样带上。
func TestHTTPSLineInsecureLikeOtherTLS(t *testing.T) {
	user := model.User{Name: "alice", Credentials: []byte(`{"http":{"password":"pw"},"trojan":{"password":"tp"}}`)}
	lines := []model.Line{
		{Id: 1, Name: "hp", Protocol: "http", Port: 8443, Enabled: true, Tls: []byte(`{"mode":"cert"}`)},
		{Id: 2, Name: "tj", Protocol: "trojan", Port: 9443, Enabled: true, Tls: []byte(`{"mode":"cert"}`)},
		{Id: 3, Name: "plain", Protocol: "http", Port: 8080, Enabled: true, Tls: []byte(`{"mode":"none"}`)},
	}
	for _, tc := range []struct {
		name     string
		insecure bool
	}{{"自签", true}, {"正式证书", false}} {
		t.Run(tc.name, func(t *testing.T) {
			entries := []Entry{{Host: "203.0.113.7", SNI: "hk.example.com", Insecure: tc.insecure}}
			// 分享链接
			var httpsLink, plainLink string
			for _, l := range GenerateLinksFor(user, lines, entries, nil) {
				switch {
				case strings.HasPrefix(l, "https://"):
					httpsLink = l
				case strings.HasPrefix(l, "http://"):
					plainLink = l
				}
			}
			u, err := url.Parse(httpsLink)
			if err != nil || httpsLink == "" {
				t.Fatalf("https 链接: %q %v", httpsLink, err)
			}
			if got := u.Query().Get("allowInsecure") == "1"; got != tc.insecure {
				t.Fatalf("https 链接的 allowInsecure 应为 %v: %s", tc.insecure, httpsLink)
			}
			if u.Query().Get("sni") != "hk.example.com" {
				t.Fatalf("https 链接应带 SNI: %s", httpsLink)
			}
			if strings.Contains(plainLink, "?") {
				t.Fatalf("不加密的 http 线路不该带 TLS 参数: %s", plainLink)
			}
			// sing-box 订阅
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
			hp, tj := tlsOf["hp"], tlsOf["tj"]
			if hp == nil || tj == nil {
				t.Fatalf("hp / tj 都应有 TLS: %v", tlsOf)
			}
			if got, _ := hp["insecure"].(bool); got != tc.insecure {
				t.Fatalf("https 线路的 tls.insecure 应为 %v(trojan 是 %v): %v", tc.insecure, tj["insecure"], hp)
			}
			if got, _ := tj["insecure"].(bool); got != tc.insecure {
				t.Fatalf("trojan 的 tls.insecure 应为 %v: %v", tc.insecure, tj)
			}
			if hp["server_name"] != "hk.example.com" {
				t.Fatalf("https 线路的 server_name 应是 SNI 而不是连接地址: %v", hp)
			}
			if tlsOf["plain"] != nil {
				t.Fatalf("不加密的 http 线路不该有 TLS: %v", tlsOf["plain"])
			}
		})
	}
}

// sing-box 订阅里的外部 https 节点:分享链接上常见的几种"允许不安全"写法与 sni / peer 都认。
func TestHTTPLinkParsesTLSParams(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://u:p@1.2.3.4:443?sni=a.example&allowInsecure=1#x":  true,
		"https://u:p@1.2.3.4:443?peer=a.example&insecure=1#x":      true,
		"https://u:p@1.2.3.4:443?sni=a.example&allow_insecure=1#x": true,
		"https://u:p@1.2.3.4:443?sni=a.example#x":                  false,
	} {
		p := httpLink(raw)
		if p == nil {
			t.Fatalf("解析不了: %s", raw)
		}
		tl, _ := p.Options["tls"].(map[string]interface{})
		if got, _ := tl["insecure"].(bool); got != want || tl["server_name"] != "a.example" {
			t.Fatalf("%s: tls=%v", raw, tl)
		}
	}
	if p := httpLink("https://u:p@a.example:443#x"); p.Options["tls"].(map[string]interface{})["server_name"] != "a.example" {
		t.Fatalf("没有 sni 时用主机名: %v", p.Options)
	}
}
