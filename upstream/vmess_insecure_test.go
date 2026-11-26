package upstream

import (
	"encoding/base64"
	"fmt"
	"testing"
)

// vmess 链接(v2rayN 的 JSON 格式)里的「允许不安全」:insecure、allowInsecure 两个键都认,值是真才算。
// 以前只认 allowInsecure,而且有这个键就算:本站自签证书的 VMess 线路写的是 insecure(sub.vmessURI),
// 转成 sing-box 订阅后不带 tls.insecure,证书校验不过连不上(实机 t60 查出);别家链接写着 allowInsecure: false 的反倒被当成允许。
func TestParseVmessInsecureKeys(t *testing.T) {
	link := func(extra string) string {
		obj := fmt.Sprintf(`{"v":"2","ps":"n","add":"203.0.113.7","port":"443","id":"11111111-2222-3333-4444-555555555555","aid":"0","net":"tcp","tls":"tls","sni":"hk.example.com"%s}`, extra)
		return "vmess://" + base64.StdEncoding.EncodeToString([]byte(obj))
	}
	for _, tc := range []struct {
		extra string
		want  bool
	}{
		{``, false},
		{`,"insecure":"1"`, true}, // 本站自签线路的写法
		{`,"insecure":1`, true},
		{`,"insecure":true`, true},
		{`,"insecure":"0"`, false},
		{`,"allowInsecure":true`, true},
		{`,"allowInsecure":"1"`, true},
		{`,"allowInsecure":1`, true},
		{`,"allowInsecure":false`, false}, // 以前:有这个键就算
		{`,"allowInsecure":"0"`, false},
		{`,"allowInsecure":0`, false},
		{`,"allowInsecure":""`, false},
	} {
		p := mustParse(t, link(tc.extra))
		tls, _ := p.Options["tls"].(map[string]interface{})
		if tls == nil {
			t.Fatalf("%s:应有 TLS", tc.extra)
		}
		if got, _ := tls["insecure"].(bool); got != tc.want {
			t.Errorf("链接里写 %q:tls.insecure 应为 %v,实际 %v", tc.extra, tc.want, got)
		}
		if tls["server_name"] != "hk.example.com" {
			t.Errorf("%s:server_name 应取 sni: %v", tc.extra, tls)
		}
	}
}
