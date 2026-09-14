package ext

import "testing"

// 展开外部订阅要能看到每个节点的全部参数:常见字段按固定顺序在前、嵌套结构给紧凑 JSON,
// 能转成链接并解析回来的才标成"可作上游"。
func TestDetailsFieldsAndUpstreamFlag(t *testing.T) {
	src := `proxies:
  - {name: a, type: hysteria2, server: 1.2.3.4, port: 443, password: p, sni: x.example.com, skip-cert-verify: true, alpn: [h3], up: 100}
  - {name: b, type: vless, server: 1.2.3.4, port: 8443, uuid: 00000000-0000-0000-0000-000000000000, tls: true, servername: x.example.com, network: ws, ws-opts: {path: /w, headers: {Host: x.example.com}}}
  - {name: c, type: snell, server: 1.2.3.4, port: 9000, psk: k}
`
	infos := Details(Parse(src))
	if len(infos) != 3 {
		t.Fatalf("应有 3 个节点,得到 %d", len(infos))
	}
	a := infos[0]
	if a.Index != 0 || a.Name != "a" || a.Type != "hysteria2" || a.Server != "1.2.3.4" || a.Port != 443 || !a.Upstream || a.Link == "" {
		t.Fatalf("节点 a 基本信息不对: %+v", a)
	}
	// 顺序:name type server port password sni alpn skip-cert-verify up
	want := []string{"name", "type", "server", "port", "password", "sni", "alpn", "skip-cert-verify", "up"}
	for i, k := range want {
		if i >= len(a.Fields) || a.Fields[i].Key != k {
			t.Fatalf("字段顺序不对,第 %d 个应是 %s: %+v", i, k, a.Fields)
		}
	}
	got := map[string]string{}
	for _, f := range a.Fields {
		got[f.Key] = f.Value
	}
	if got["port"] != "443" || got["skip-cert-verify"] != "true" || got["alpn"] != `["h3"]` || got["up"] != "100" {
		t.Fatalf("字段值不对: %v", got)
	}
	b := infos[1]
	bm := map[string]string{}
	for _, f := range b.Fields {
		bm[f.Key] = f.Value
	}
	if bm["ws-opts"] != `{"headers":{"Host":"x.example.com"},"path":"/w"}` {
		t.Fatalf("嵌套结构应给紧凑 JSON: %q", bm["ws-opts"])
	}
	if !b.Upstream {
		t.Fatalf("vless 应可作上游: %+v", b)
	}
	if c := infos[2]; c.Upstream || c.Link != "" {
		t.Fatalf("不支持的协议不该标成可作上游: %+v", c)
	}
}

func TestDetailsEmpty(t *testing.T) {
	if got := Details(Items{}); len(got) != 0 {
		t.Fatalf("空输入应得空: %v", got)
	}
}
