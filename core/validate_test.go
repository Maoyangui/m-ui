package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// 回归:一条坏上游(假 uuid)曾把整个数据面打掉。干跑校验必须在保存前拦住它。
func TestValidateOutboundRejectsBadUUID(t *testing.T) {
	bad := json.RawMessage(`{"type":"tuic","tag":"bad","server":"x.example","server_port":443,"uuid":"u","password":"p","congestion_control":"cubic","tls":{"enabled":true,"server_name":"x.example","alpn":["h3"]}}`)
	err := ValidateOutbound(bad)
	if err == nil {
		t.Fatal("假 uuid 的 tuic 出站应被拒绝")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "uuid") {
		t.Fatalf("错误应指明 uuid 问题,实际: %v", err)
	}
}

func TestValidateOutboundAcceptsRealShapes(t *testing.T) {
	cases := map[string]string{
		"tuic":        `{"type":"tuic","tag":"t","server":"x.example","server_port":443,"uuid":"11111111-2222-3333-4444-555555555555","password":"p","congestion_control":"cubic","udp_relay_mode":"native","tls":{"enabled":true,"server_name":"x.example","alpn":["h3"]}}`,
		"shadowsocks": `{"type":"shadowsocks","tag":"s","server":"1.2.3.4","server_port":8388,"method":"aes-256-gcm","password":"pw"}`,
		"socks-warp":  `{"type":"socks","tag":"warp","server":"127.0.0.1","server_port":40000,"version":"5"}`,
		"hysteria2":   `{"type":"hysteria2","tag":"h","server":"x.example","server_port":443,"password":"pw","tls":{"enabled":true,"server_name":"x.example","alpn":["h3"]}}`,
	}
	for name, raw := range cases {
		if err := ValidateOutbound(json.RawMessage(raw)); err != nil {
			t.Errorf("%s 应通过校验,实际: %v", name, err)
		}
	}
}

func TestValidateOutboundRejectsUnknownField(t *testing.T) {
	bad := json.RawMessage(`{"type":"socks","tag":"s","server":"1.2.3.4","server_port":1080,"version":"5","no_such_field":1}`)
	if err := ValidateOutbound(bad); err == nil {
		t.Fatal("未知字段应被拒绝")
	}
}

func TestValidateInbound(t *testing.T) {
	ok := json.RawMessage(`{"type":"anytls","tag":"a","listen":"::","listen_port":30688,"padding_scheme":["stop=8","0=30-30"]}`)
	if err := ValidateInbound(ok); err != nil {
		t.Fatalf("合法 anytls 参数应通过: %v", err)
	}
	// padding_scheme 是"字符串或列表"皆可的类型,单字符串合法;真正的非法是端口给字符串
	badType := json.RawMessage(`{"type":"anytls","tag":"a","listen":"::","listen_port":"abc"}`)
	if err := ValidateInbound(badType); err == nil {
		t.Fatal("端口类型错误应被拒绝")
	}
	unknown := json.RawMessage(`{"type":"anytls","tag":"a","listen":"::","listen_port":30688,"no_such_field":true}`)
	if err := ValidateInbound(unknown); err == nil {
		t.Fatal("未知字段应被拒绝")
	}
}
