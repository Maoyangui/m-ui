package ext

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseLinksPlainAndBase64AndYAML(t *testing.T) {
	links := "vless://11111111-1111-1111-1111-111111111111@1.2.3.4:443?security=reality&pbk=PUBKEY&sid=ab12&fp=chrome&sni=www.apple.com&type=tcp&flow=xtls-rprx-vision#节点A\n" +
		"hysteria2://pw@5.6.7.8:8443?sni=h.example.com&insecure=1#节点B\n" +
		"ss://" + base64.StdEncoding.EncodeToString([]byte("aes-128-gcm:secret")) + "@9.9.9.9:8388#节点C\n"
	it := Parse(links)
	if len(it.Links) != 3 || len(it.Clash) != 3 {
		t.Fatalf("明文链接应解析 3/3,实际 %d/%d", len(it.Links), len(it.Clash))
	}
	a := it.Clash[0]
	if a["type"] != "vless" || a["server"] != "1.2.3.4" || a["port"] != 443 || a["flow"] != "xtls-rprx-vision" || a["servername"] != "www.apple.com" {
		t.Fatalf("vless 转换错误: %v", a)
	}
	if ro, _ := a["reality-opts"].(map[string]interface{}); ro == nil || ro["public-key"] != "PUBKEY" || ro["short-id"] != "ab12" {
		t.Fatalf("reality 参数丢失: %v", a)
	}
	b := it.Clash[1]
	if b["type"] != "hysteria2" || b["password"] != "pw" || b["sni"] != "h.example.com" || b["skip-cert-verify"] != true {
		t.Fatalf("hy2 转换错误: %v", b)
	}
	c := it.Clash[2]
	if c["type"] != "ss" || c["cipher"] != "aes-128-gcm" || c["password"] != "secret" {
		t.Fatalf("ss 转换错误: %v", c)
	}

	b64 := base64.StdEncoding.EncodeToString([]byte(links))
	if it2 := Parse(b64); len(it2.Links) != 3 {
		t.Fatalf("base64 链接列表应解析 3 条,实际 %d", len(it2.Links))
	}

	yamlDoc := "port: 7890\nproxies:\n  - name: 外部A\n    type: ss\n    server: 1.1.1.1\n    port: 8388\n    cipher: aes-128-gcm\n    password: x\n  - name: 外部B\n    type: trojan\n    server: 2.2.2.2\n    port: 443\n    password: y\n    sni: t.example.com\nproxy-groups: []\n"
	it3 := Parse(yamlDoc)
	if len(it3.Clash) != 2 || it3.Clash[1]["sni"] != "t.example.com" || len(it3.Links) != 0 {
		t.Fatalf("YAML 应解析 2 个 clash 代理: %+v", it3)
	}

	pre := WithPrefix(it, "[外] ")
	if !strings.Contains(pre.Links[0], "#%5B%E5%A4%96%5D%20") || pre.Clash[0]["name"] != "[外] 节点A" {
		t.Fatalf("前缀未生效: %s / %v", pre.Links[0], pre.Clash[0]["name"])
	}
	if it.Clash[0]["name"] != "节点A" {
		t.Fatal("WithPrefix 不应改动原数据")
	}
}

func TestParseGarbage(t *testing.T) {
	if it := Parse("hello world"); len(it.Links) != 0 || len(it.Clash) != 0 {
		t.Fatal("垃圾内容应为空")
	}
	if _, ok := LinkToClash("foo://bar"); ok {
		t.Fatal("不支持的链接应返回 false")
	}
}
