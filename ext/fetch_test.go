package ext

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const clashTwo = `proxies:
  - {name: a, type: hysteria2, server: 1.2.3.4, port: 443, password: p, sni: x.example.com, skip-cert-verify: true}
  - {name: b, type: tuic, server: 1.2.3.4, port: 444, uuid: 00000000-0000-0000-0000-000000000000, password: p, sni: x.example.com}
`

// 有的面板按客户端身份发不同内容:Clash 系给全量,通用身份只给一两种协议。抓取要先以 Clash Meta 身份要。
func TestFetchPrefersClashMetaAgent(t *testing.T) {
	var agents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		agents = append(agents, ua)
		if strings.Contains(ua, "ClashMeta") {
			w.Header().Set("Content-Type", "text/yaml")
			w.Write([]byte(clashTwo))
			return
		}
		w.Write([]byte(base64.StdEncoding.EncodeToString([]byte("hysteria2://p@1.2.3.4:443?sni=x.example.com#only\n"))))
	}))
	defer srv.Close()
	b, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	it := Parse(string(b))
	if len(it.Clash) != 2 {
		t.Fatalf("应拿到 Clash 身份那份的 2 个节点,得到 %d", len(it.Clash))
	}
	if len(agents) != 1 || !strings.Contains(agents[0], "ClashMeta") {
		t.Fatalf("第一份就解析出节点时不该再请求第二次: %v", agents)
	}
}

// Clash 身份拿到的解析不出节点(比如面板只对它发一段说明),要退回通用身份。
func TestFetchFallsBackToGenericAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("User-Agent"), "ClashMeta") {
			w.Write([]byte("please use another client"))
			return
		}
		w.Write([]byte(base64.StdEncoding.EncodeToString([]byte("hysteria2://p@1.2.3.4:443?sni=x.example.com#only\n"))))
	}))
	defer srv.Close()
	b, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if it := Parse(string(b)); len(it.Clash) != 1 {
		t.Fatalf("应退回通用身份拿到 1 个节点,得到 %d", len(it.Clash))
	}
}

// 两种身份都解析不出:把第一份交出去,调用方据此报"没有可识别的节点";不能当抓取失败。
func TestFetchReturnsFirstBodyWhenNothingParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("nothing here " + r.Header.Get("User-Agent")))
	}))
	defer srv.Close()
	b, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ClashMeta") {
		t.Fatalf("应返回第一份(Clash 身份)的原文,得到 %q", b)
	}
}

// sing-box JSON:取 outbounds 里带 server 的,选择组 / direct 略过。
func TestParseSingBoxJSON(t *testing.T) {
	src := `{"outbounds":[
		{"type":"selector","tag":"proxy","outbounds":["h"]},
		{"type":"hysteria2","tag":"h","server":"1.2.3.4","server_port":443,"password":"p","tls":{"enabled":true,"server_name":"x.example.com","insecure":true}},
		{"type":"direct","tag":"direct"}]}`
	it := Parse(src)
	if len(it.Clash) != 1 || len(it.Links) != 1 {
		t.Fatalf("应解析出 1 个节点 1 条链接,得到 %d / %d", len(it.Clash), len(it.Links))
	}
	p := it.Clash[0]
	if p["name"] != "h" || p["type"] != "hysteria2" || p["server"] != "1.2.3.4" || p["port"] != 443 || p["sni"] != "x.example.com" {
		t.Fatalf("字段不对: %v", p)
	}
	if !strings.HasPrefix(it.Links[0], "hysteria2://") {
		t.Fatalf("链接不对: %s", it.Links[0])
	}
}
