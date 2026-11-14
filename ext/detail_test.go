package ext

import "testing"

// 展开外部订阅要能看到每个节点的全部参数:常见字段按固定顺序在前、嵌套结构给紧凑 JSON,
// 能无损转成 sing-box 出站的才标成"可测速 / 可作上游",不能的给出原因。
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
	if a.Index != 0 || a.Name != "a" || a.Type != "hysteria2" || a.Server != "1.2.3.4" || a.Port != 443 || !a.Upstream || a.Link == "" || len(a.Key) != 16 {
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
	if c := infos[2]; c.Upstream || c.Link != "" || c.Reason == "" {
		t.Fatalf("不支持的协议不该标成可作上游,且要说明原因: %+v", c)
	}
}

func TestDetailsEmpty(t *testing.T) {
	if got := Details(Items{}); len(got) != 0 {
		t.Fatalf("空输入应得空: %v", got)
	}
}

// 审计 I5:名字里有不合法的百分号转义("流量 100% 可用")的链接,以前整条解析失败、只进链接订阅,面板看不到、没法隐藏;
// 协议本站不认识的链接也一样。现在都在面板列出、能隐藏(后者标成不能测、不能作上游)。
func TestLinkOnlyNodesListedInDetails(t *testing.T) {
	raw := "trojan://pw@1.2.3.4:443#%E9%A6%99%E6%B8%AF%2001\n" +
		"trojan://pw@5.6.7.8:443#流量 100% 可用\n" +
		"snell://psk@9.9.9.9:8000#Snell 节点\n"
	it := Parse(raw)
	infos := Details(it)
	if len(infos) != 3 {
		t.Fatalf("三条都应列出: %+v", infos)
	}
	if infos[1].Name != "流量 100% 可用" || !infos[1].Upstream {
		t.Fatalf("名字里有非法转义的节点应能正常解析: %+v", infos[1])
	}
	if s := infos[2]; s.Name != "Snell 节点" || s.Type != "snell" || s.Server != "9.9.9.9" || s.Port != 8000 || s.Upstream || s.Reason == "" {
		t.Fatalf("只有链接的节点应列出且标成不能测: %+v", s)
	}
	f := newFilter("")
	r := RosterOf(it)
	f.Hide(r, infos[1].Name, infos[1].Key)
	f.Hide(r, infos[2].Name, infos[2].Key)
	got := f.Apply(it)
	if len(got.Links()) != 1 || len(got.Clash()) != 1 || LinkName(got.Links()[0]) != "香港 01" {
		t.Fatalf("隐藏后链接订阅里也应只剩 香港 01: links=%v clash=%v", linkNames(got), clashNames(got))
	}
	Annotate(infos, f)
	if !infos[1].Hidden || !infos[2].Hidden || infos[0].Hidden {
		t.Fatalf("面板状态不对: %+v", infos)
	}
}
