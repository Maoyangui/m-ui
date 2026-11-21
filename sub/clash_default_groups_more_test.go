package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"

	"gopkg.in/yaml.v3"
)

// 「自动生成默认策略组」(subClashDefaultGroups)的补充测试:线路测速组跟着开关走、
// 模板在规则以外的地方用到 Proxy / Auto、设置页保存时的判断、警告只记一次。

// twoLines 两条 trojan 线路,都部署在 twoServers 的两台上:开关开着时各出一个线路测速组(线一、线二)。
func twoLines() []model.Line {
	cert := raw(map[string]interface{}{"mode": "cert"})
	return []model.Line{
		{Id: 1, Name: "线一", Protocol: "trojan", Port: 8443, Tls: cert, Enabled: true},
		{Id: 2, Name: "线二", Protocol: "trojan", Port: 9443, Tls: cert, Enabled: true},
	}
}

func clashProxyNames(t *testing.T, out string) []string {
	t.Helper()
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(out), &root); err != nil {
		t.Fatalf("输出不是合法 YAML: %v", err)
	}
	names := []string{}
	ps, _ := root["proxies"].([]interface{})
	for _, p := range ps {
		m, _ := p.(map[string]interface{})
		n, _ := m["name"].(string)
		names = append(names, n)
	}
	return names
}

// clashDangling 策略组的成员、规则的目标里指向不存在的名字的(mihomo 遇到就整份拒收)。
func clashDangling(t *testing.T, out string) []string {
	t.Helper()
	known := map[string]bool{}
	for _, n := range clashBuiltins {
		known[n] = true
	}
	for _, n := range clashProxyNames(t, out) {
		known[n] = true
	}
	for _, n := range clashGroupNames(t, out) {
		known[n] = true
	}
	var bad []string
	for _, g := range clashGroupsOf(t, out) {
		m, _ := g.(map[string]interface{})
		for _, member := range stringList(m["proxies"]) {
			if !known[member] {
				bad = append(bad, member)
			}
		}
	}
	for _, r := range clashRulesOf(t, out) {
		if strings.HasPrefix(r, "SUB-RULE,") { // 目标是 sub-rules 里的一段,不是策略组
			continue
		}
		f := strings.Split(r, ",")
		target := f[len(f)-1]
		if target == "no-resolve" || target == "src" {
			target = f[len(f)-2]
		}
		if !known[strings.TrimSpace(target)] {
			bad = append(bad, target)
		}
	}
	return bad
}

func sameNames(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

// 开关开着(默认):和以前一样,Proxy、Auto、两个线路测速组都在,Proxy 的选单列着线路组。
func TestDefaultGroupsOnKeepsLineGroups(t *testing.T) {
	out, err := BuildClashFor(fullUser(), twoLines(), twoServers, "", "", nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if names := clashGroupNames(t, out); !sameNames(names, []string{"Proxy", "Auto", "线一", "线二"}) {
		t.Fatalf("开关开着应有 Proxy、Auto、线一、线二,实际 %v", names)
	}
	if m := stringList(findGroup(clashGroupsOf(t, out), "Proxy")["proxies"]); !sameNames(m, []string{"Auto", "线一", "线二"}) {
		t.Fatalf("Proxy 应列 Auto 和两个线路组,实际 %v", m)
	}
	if bad := clashDangling(t, out); len(bad) > 0 {
		t.Fatalf("有悬空的名字: %v", bad)
	}
}

// 开关关着、模板不用它们:线路测速组也不生成,节点照样都在(模板 include-all 的组选得到)。
func TestDefaultGroupsOffDropsLineGroups(t *testing.T) {
	tmpl := "rules:\n  - MATCH,我的组\nproxy-groups:\n  - name: 我的组\n    type: select\n    include-all: true\n"
	out, err := BuildClashFor(fullUser(), twoLines(), twoServers, tmpl, "", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if names := clashGroupNames(t, out); !sameNames(names, []string{"我的组"}) {
		t.Fatalf("开关关着、模板不用时只该有模板自己的组,实际 %v", names)
	}
	if ps := clashProxyNames(t, out); !sameNames(ps, []string{"线一-A", "线一-B", "线二-A", "线二-B"}) {
		t.Fatalf("节点应照样都在,实际 %v", ps)
	}
	if bad := clashDangling(t, out); len(bad) > 0 {
		t.Fatalf("有悬空的名字: %v", bad)
	}
}

// 开关关着、模板按名字用到某条线路的测速组(规则的目标,或策略组的成员):只留用到的那个,Proxy / Auto 不生成。
func TestDefaultGroupsOffKeepsReferencedLineGroup(t *testing.T) {
	cases := []struct {
		name, tmpl string
		want       []string
	}{
		{"规则的目标", "rules:\n  - DOMAIN,a.example,线一\n  - MATCH,我的组\nproxy-groups:\n  - name: 我的组\n    type: select\n    include-all: true\n",
			[]string{"线一", "我的组"}},
		{"策略组的成员", "rules:\n  - MATCH,我的组\nproxy-groups:\n  - name: 我的组\n    type: select\n    proxies: [线二, DIRECT]\n",
			[]string{"线二", "我的组"}},
	}
	for _, c := range cases {
		out, err := BuildClashFor(fullUser(), twoLines(), twoServers, c.tmpl, "", nil, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if names := clashGroupNames(t, out); !sameNames(names, c.want) {
			t.Errorf("%s:应只有 %v,实际 %v", c.name, c.want, names)
		}
		if bad := clashDangling(t, out); len(bad) > 0 {
			t.Errorf("%s:有悬空的名字: %v", c.name, bad)
		}
	}
}

// 开关关着、模板还用着 Proxy:Proxy、Auto 和全部线路测速组都生成 —— Proxy 的选单列着它们,少一个就悬空。
func TestDefaultGroupsOffProxyKeepsLineGroups(t *testing.T) {
	out, err := BuildClashFor(fullUser(), twoLines(), twoServers, "rules:\n  - MATCH,Proxy\n", "", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if names := clashGroupNames(t, out); !sameNames(names, []string{"Proxy", "Auto", "线一", "线二"}) {
		t.Fatalf("模板用着 Proxy 时应有 Proxy、Auto、线一、线二,实际 %v", names)
	}
	if bad := clashDangling(t, out); len(bad) > 0 {
		t.Fatalf("有悬空的名字: %v", bad)
	}
}

// 模板在规则以外的地方用 Proxy / Auto 也算:DNS 服务器的 #出站、sub-rules、入站 listeners 的 proxy、
// proxy-providers 的 override.dialer-proxy。漏认了订阅里就没有这个组,客户端拒收或 DNS 解析失败。
// 模板写了 rules 键但是空的:订阅不补默认规则,也就不算用了 Proxy。
func TestDefaultGroupsOffOtherRefs(t *testing.T) {
	cases := []struct {
		name, tmpl  string
		proxy, auto bool
	}{
		{"DNS 服务器的 #出站", "dns:\n  enable: true\n  nameserver:\n    - https://1.1.1.1/dns-query#Proxy&h3=true\nrules:\n  - MATCH,DIRECT\n", true, true},
		{"sub-rules", "sub-rules:\n  r1:\n    - MATCH,Auto\nrules:\n  - SUB-RULE,(NETWORK,tcp),r1\n  - MATCH,DIRECT\n", false, true},
		{"入站 listeners", "listeners:\n  - name: in1\n    type: socks\n    port: 10808\n    proxy: Auto\nrules:\n  - MATCH,DIRECT\n", false, true},
		{"proxy-providers 的 override", "proxy-providers:\n  p1:\n    type: http\n    url: https://example.com/p.yaml\n    path: ./p1.yaml\n    override:\n      dialer-proxy: Proxy\nrules:\n  - MATCH,DIRECT\n", true, true},
		{"都没用", "rules:\n  - MATCH,DIRECT\n", false, false},
		{"rules 写了但是空的", "rules: []\n", false, false},
	}
	for _, c := range cases {
		out, err := BuildClashFor(fullUser(), twoLines(), twoServers, c.tmpl, "", nil, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		names := clashGroupNames(t, out)
		if hasName(names, "Proxy") != c.proxy || hasName(names, "Auto") != c.auto {
			t.Errorf("%s:Proxy 应 %v、Auto 应 %v,实际组 %v", c.name, c.proxy, c.auto, names)
		}
		// 线路组只跟着 Proxy 走(这些模板都没按名字用线路组)
		if hasName(names, "线一") != c.proxy || hasName(names, "线二") != c.proxy {
			t.Errorf("%s:线路组应跟着 Proxy(%v),实际组 %v", c.name, c.proxy, names)
		}
		if bad := clashDangling(t, out); len(bad) > 0 {
			t.Errorf("%s:有悬空的名字: %v", c.name, bad)
		}
		if got := ClashTemplateNeedsDefaultGroups(c.tmpl); hasName(got, "Proxy") != c.proxy || hasName(got, "Auto") != c.auto {
			t.Errorf("%s:保存时的判断应和订阅一致(Proxy %v、Auto %v),实际 %v", c.name, c.proxy, c.auto, got)
		}
	}
	out, _ := BuildClashFor(fullUser(), twoLines(), twoServers, "rules: []\n", "", nil, nil, false)
	if rules := clashRulesOf(t, out); len(rules) != 0 {
		t.Errorf("rules 写了空的就不补默认规则,实际 %v", rules)
	}
}

// 设置页保存时的判断:关掉开关后这份模板(空 = 内置)订阅里还会生成哪些。
func TestClashTemplateNeedsDefaultGroups(t *testing.T) {
	both := "rules:\n  - MATCH,Proxy\nproxy-groups:\n" +
		"  - name: Proxy\n    type: select\n    proxies: [Auto]\n" +
		"  - name: Auto\n    type: url-test\n    url: http://www.gstatic.com/generate_204\n    interval: 300\n    include-all: true\n"
	cases := []struct {
		name, tmpl string
		want       []string
	}{
		{"空 = 内置模板", "", []string{"Proxy", "Auto"}},
		{"只有空白 = 内置模板", "  \n", []string{"Proxy", "Auto"}},
		{"只用自己的组", "rules:\n  - MATCH,G\nproxy-groups:\n  - name: G\n    type: select\n    proxies: [DIRECT]\n", nil},
		{"只用 Auto", "rules:\n  - MATCH,Auto\n", []string{"Auto"}},
		{"两个都自己定义了", both, nil},
		{"自己定义了 Proxy、它列着 Auto", "rules:\n  - MATCH,Proxy\nproxy-groups:\n  - name: Proxy\n    type: select\n    proxies: [Auto, DIRECT]\n", []string{"Auto"}},
		{"没写 rules", "dns:\n  enable: true\n", []string{"Proxy", "Auto"}},
		{"模板自己的节点 dialer-proxy", "proxies:\n  - {name: a, type: ss, server: 1.2.3.4, port: 1, cipher: aes-128-gcm, password: x, dialer-proxy: Auto}\nrules:\n  - MATCH,a\n", []string{"Auto"}},
		{"不是合法 YAML", "rules: [\n", nil},
	}
	for _, c := range cases {
		if got := ClashTemplateNeedsDefaultGroups(c.tmpl); !sameNames(got, c.want) {
			t.Errorf("%s:应为 %v,实际 %v", c.name, c.want, got)
		}
	}
}

// 关着开关却还得生成时记的警告:同一份模板、同样的结果只记一次(以前每拉一次订阅记一条);结果变了再记。
func TestDefaultGroupsWarnOnce(t *testing.T) {
	count := func() int {
		n := 0
		for _, l := range logger.GetLogs(20000, "WARNING") {
			if strings.Contains(l, "默认 Proxy/Auto 策略组已关闭") {
				n++
			}
		}
		return n
	}
	tmpl := "# " + t.Name() + "\nrules:\n  - MATCH,Proxy\n" // 注释让模板原文只属于这个测试(摘要按原文算)
	before := count()
	for i := 0; i < 3; i++ {
		if _, err := BuildClashFor(fullUser(), twoLines(), twoServers, tmpl, "", nil, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := count() - before; got != 1 {
		t.Fatalf("同一份模板拉 3 次应只记 1 条,实际 %d", got)
	}
	tmpl2 := "# " + t.Name() + "\nrules:\n  - MATCH,Auto\n"
	if _, err := BuildClashFor(fullUser(), twoLines(), twoServers, tmpl2, "", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if got := count() - before; got != 2 {
		t.Fatalf("换了模板(结果也变了)应再记 1 条,实际共 %d", got)
	}
	// 开关开着、或关着但不用:都不记
	if _, err := BuildClashFor(fullUser(), twoLines(), twoServers, "# "+t.Name()+" on\nrules:\n  - MATCH,Proxy\n", "", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildClashFor(fullUser(), twoLines(), twoServers, "# "+t.Name()+" none\nrules:\n  - MATCH,DIRECT\n", "", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if got := count() - before; got != 2 {
		t.Fatalf("开关开着、或模板不用时不该记,实际共 %d", got)
	}
}
