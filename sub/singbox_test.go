package sub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database/model"
)

// sing-box JSON 订阅:各协议线路 + 外部节点都要进 outbounds,分组齐全,并且整份配置能被内嵌 sing-box 解析构造。
func TestSingBoxSubValidates(t *testing.T) {
	lines := []model.Line{
		{Name: "hy", Protocol: "hysteria2", Port: 443, Tls: raw(map[string]interface{}{"mode": "cert"})},
		{Name: "any", Protocol: "anytls", Port: 8443, Tls: raw(map[string]interface{}{"mode": "cert"})},
		{Name: "vl", Protocol: "vless", Port: 8888, Tls: raw(map[string]interface{}{"mode": "reality", "reality": map[string]interface{}{
			"private_key": "priv", "public_key": "MJnakD6uWVj6xdd5BTOIo8yP6WRiAciOtTcF_23k50A", "short_ids": []string{"abcd1234"},
			"handshake_server": "www.microsoft.com", "handshake_port": 443}}), Options: raw(map[string]interface{}{"vision": true})},
		{Name: "tu", Protocol: "tuic", Port: 8844, Tls: raw(map[string]interface{}{"mode": "cert"}), Options: raw(map[string]interface{}{"congestion_control": "bbr"})},
		{Name: "tj", Protocol: "trojan", Port: 9443, Transport: raw(map[string]interface{}{"type": "grpc", "service_name": "svc"})},
	}
	opt := Options{Entries: hkEntry, External: []ExtItem{{Name: "ext", Links: []string{
		"hysteria2://secret@34.81.174.123:443?sni=aron.joinvip.vip#%E5%8F%B0%E6%B9%BEwarp1",
		"http://user:pw@1.2.3.4:8080#skipped", // sing-box 订阅不收 http 代理,应被跳过而不是报错
	}}}}
	res, err := BuildSingBoxSub(fullUser(), lines, opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Headers["Content-Type"] != "application/json; charset=utf-8" || !strings.HasPrefix(res.Headers["Subscription-Userinfo"], "upload=") {
		t.Fatalf("headers: %v", res.Headers)
	}
	var cfg struct {
		Outbounds []map[string]interface{} `json:"outbounds"`
		Inbounds  []map[string]interface{} `json:"inbounds"`
		Route     map[string]interface{}   `json:"route"`
	}
	if err := json.Unmarshal([]byte(res.Body), &cfg); err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	tags := map[string]bool{}
	for _, o := range cfg.Outbounds {
		types[o["type"].(string)]++
		tags[o["tag"].(string)] = true
	}
	for _, want := range []string{"selector", "urltest", "hysteria2", "anytls", "vless", "tuic", "trojan", "direct"} {
		if types[want] == 0 {
			t.Fatalf("缺少 %s 出站: %v", want, types)
		}
	}
	if types["hysteria2"] != 2 || types["http"] != 0 {
		t.Fatalf("外部节点应并入(hysteria2×2)、http 应跳过: %v", types)
	}
	if !tags["proxy"] || !tags["auto"] || !tags["台湾warp1"] {
		t.Fatalf("分组或外部节点 tag 缺失: %v", tags)
	}
	if cfg.Route["final"] != "proxy" || len(cfg.Inbounds) != 2 {
		t.Fatalf("route/inbounds 不符: %v %d", cfg.Route["final"], len(cfg.Inbounds))
	}
	// 内嵌 sing-box 干跑:字段名、类型、废弃项都会在这里暴露
	// 服务端构建没有注册 tun 入站(客户端才有),干跑前去掉它
	var full map[string]interface{}
	_ = json.Unmarshal([]byte(res.Body), &full)
	var keep []interface{}
	for _, in := range full["inbounds"].([]interface{}) {
		if in.(map[string]interface{})["type"] != "tun" {
			keep = append(keep, in)
		}
	}
	full["inbounds"] = keep
	body, _ := json.Marshal(full)
	if err := core.ValidateConfig(body); err != nil {
		t.Fatalf("sing-box 拒绝生成的客户端配置: %v\n%s", err, res.Body)
	}
}

func TestSingBoxSubNoNodes(t *testing.T) {
	if _, err := BuildSingBoxSub(fullUser(), nil, Options{Entries: hkEntry}); err == nil {
		t.Fatal("没有节点应报错")
	}
}
