package sub

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"

	"gopkg.in/yaml.v3"
)

// clashDoc 测试里看 Clash 订阅用的结构。
type clashDoc struct {
	Proxies   []map[string]interface{} `yaml:"proxies"`
	Groups    []map[string]interface{} `yaml:"proxy-groups"`
	Providers map[string]interface{}   `yaml:"proxy-providers"`
	Rules     []string                 `yaml:"rules"`
}

// clashProblem 照 mihomo 加载配置时的检查把 Clash 订阅查一遍,返回第一个会让整份配置导入失败的问题。
// 仓库里没有内嵌 mihomo,这里照它的源码(config/config.go 的 parseProxies、config/utils.go 的 proxyGroupsDagSort、
// adapter/outboundgroup/parser.go 的 ParseProxyGroup、规则的出口查找)做结构检查:
//   - 代理、策略组和内置的 DIRECT / REJECT 等共用一张名字表:名字不能为空、不能重复;
//   - 策略组之间不能成环(mihomo 按名字建图,组列了和自己同名的代理也算环);
//   - 策略组列的成员都要找得到(代理、别的组或内置出站);既没成员、也没 use / include-all 的组不行;
//   - 列了成员的组还占一个同名 provider,不能和 proxy-providers 重名;
//   - 规则的出口要找得到。
func clashProblem(body string) (clashDoc, error) {
	var doc clashDoc
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return doc, fmt.Errorf("不是合法 YAML: %w", err)
	}
	names := map[string]bool{}
	for _, n := range clashBuiltins {
		names[n] = true
	}
	for i, p := range doc.Proxies {
		n, _ := p["name"].(string)
		if n == "" {
			return doc, fmt.Errorf("proxy %d: 没有名字", i)
		}
		if names[n] {
			return doc, fmt.Errorf("proxy %s is the duplicate name", n)
		}
		names[n] = true
	}
	members := map[string][]string{}
	for i, g := range doc.Groups {
		n, _ := g["name"].(string)
		if n == "" {
			return doc, fmt.Errorf("proxy group %d: missing name", i)
		}
		if _, dup := members[n]; dup {
			return doc, fmt.Errorf("ProxyGroup %s: duplicate group name", n)
		}
		members[n] = stringList(g["proxies"])
	}
	state := map[string]int{} // 0 没走过、1 在当前路径上、2 查完了
	var loop func(string) bool
	loop = func(n string) bool {
		switch state[n] {
		case 1:
			return true
		case 2:
			return false
		}
		state[n] = 1
		for _, m := range members[n] {
			if _, isGroup := members[m]; isGroup && loop(m) {
				return true
			}
		}
		state[n] = 2
		return false
	}
	for _, g := range doc.Groups {
		if n := g["name"].(string); loop(n) {
			return doc, fmt.Errorf("loop is detected in ProxyGroup: %s", n)
		}
	}
	for _, g := range doc.Groups {
		n := g["name"].(string)
		if names[n] {
			return doc, fmt.Errorf("proxy group %s: the duplicate name", n)
		}
		ms := members[n]
		_, use := g["use"]
		all := g["include-all"] == true || g["include-all-proxies"] == true || g["include-all-providers"] == true
		if len(ms) == 0 && !use && !all {
			return doc, fmt.Errorf("%s: `use` or `proxies` missing", n)
		}
		for _, m := range ms {
			if _, isGroup := members[m]; !isGroup && !names[m] {
				return doc, fmt.Errorf("proxy group %s: '%s' not found", n, m)
			}
		}
		if _, dup := doc.Providers[n]; dup && len(ms) > 0 {
			return doc, fmt.Errorf("%s: duplicate provider name", n)
		}
	}
	for _, r := range doc.Rules {
		parts := strings.Split(r, ",")
		target := ""
		switch {
		case parts[0] == "AND" || parts[0] == "OR" || parts[0] == "NOT" || parts[0] == "SUB-RULE":
			continue // 逻辑规则里带逗号,这里的模板用不到
		case parts[0] == "MATCH" && len(parts) >= 2:
			target = parts[1]
		case len(parts) >= 3:
			target = parts[2]
		}
		target = strings.TrimSpace(target)
		if _, isGroup := members[target]; !isGroup && !names[target] && target != "GLOBAL" {
			return doc, fmt.Errorf("rules [%s]: proxy [%s] not found", r, target)
		}
	}
	return doc, nil
}

// checkClash 订阅得能被 mihomo 整份加载(见 clashProblem)。
func checkClash(t *testing.T, body string) clashDoc {
	t.Helper()
	doc, err := clashProblem(body)
	if err != nil {
		t.Fatalf("mihomo 会拒收这份 Clash 订阅: %v\n%s", err, body)
	}
	return doc
}

func (d clashDoc) proxyNames() []string {
	var out []string
	for _, p := range d.Proxies {
		out = append(out, p["name"].(string))
	}
	return out
}

func (d clashDoc) group(name string) map[string]interface{} {
	for _, g := range d.Groups {
		if g["name"] == name {
			return g
		}
	}
	return nil
}

// lineGroups Clash 订阅里的线路组(url-test,不含 Auto),按出现顺序:组名 → 成员。
func (d clashDoc) lineGroups() ([]string, map[string][]string) {
	var order []string
	members := map[string][]string{}
	for _, g := range d.Groups {
		if n := g["name"].(string); g["type"] == "url-test" && n != "Auto" {
			order = append(order, n)
			members[n] = stringList(g["proxies"])
		}
	}
	return order, members
}

// singBoxLineGroups sing-box JSON 里的线路组(urltest,不含 auto),按出现顺序。
func singBoxLineGroups(d sbDoc) []string {
	var out []string
	for _, o := range d.Outbounds {
		if tag := o["tag"].(string); o["type"] == "urltest" && tag != "auto" {
			out = append(out, tag)
		}
	}
	return out
}

// 检查器自己先过一遍:以前 Clash 订阅里会出现的几种撞名它都得认出来,否则后面的检查等于没查;正常的配置不能误报。
func TestClashCheckerCatchesBadConfigs(t *testing.T) {
	node := func(name string) string {
		return fmt.Sprintf("  - {name: %q, type: socks5, server: 1.2.3.4, port: 1080}\n", name)
	}
	for name, body := range map[string]string{
		"线路组和自己的节点同名": "proxies:\n" + node("hk") + node("hk 2") +
			"proxy-groups:\n  - {name: hk, type: url-test, proxies: [hk, hk 2]}\n",
		"线路组和外部节点同名": "proxies:\n" + node("hy-A") + node("hy-B") + node("hy") +
			"proxy-groups:\n  - {name: hy, type: url-test, proxies: [hy-A, hy-B]}\n",
		"组名撞内置出站":  "proxies:\n" + node("a") + "proxy-groups:\n  - {name: DIRECT, type: select, proxies: [a]}\n",
		"节点撞内置出站":  "proxies:\n" + node("REJECT"),
		"节点重名":     "proxies:\n" + node("a") + node("a"),
		"组重名":      "proxies:\n" + node("a") + "proxy-groups:\n  - {name: g, type: select, proxies: [a]}\n  - {name: g, type: select, proxies: [a]}\n",
		"成员找不到":    "proxies:\n" + node("a") + "proxy-groups:\n  - {name: g, type: select, proxies: [a, b]}\n",
		"两个组互相引用":  "proxies:\n" + node("a") + "proxy-groups:\n  - {name: g1, type: select, proxies: [g2, a]}\n  - {name: g2, type: select, proxies: [g1]}\n",
		"空组":       "proxy-groups:\n  - {name: g, type: select}\n",
		"组和提供者同名":  "proxies:\n" + node("a") + "proxy-providers:\n  g: {type: file, path: ./g.yaml}\nproxy-groups:\n  - {name: g, type: select, proxies: [a]}\n",
		"规则的出口找不到": "proxies:\n" + node("a") + "rules:\n  - MATCH,Proxy\n",
	} {
		if _, err := clashProblem(body); err == nil {
			t.Errorf("%s:检查器没认出来\n%s", name, body)
		}
	}
	good := "proxies:\n" + node("a") + node("b") + "proxy-providers:\n  p: {type: file, path: ./p.yaml}\n" +
		"proxy-groups:\n  - {name: Proxy, type: select, proxies: [Auto, g, a, DIRECT]}\n  - {name: Auto, type: url-test, proxies: [a, b]}\n" +
		"  - {name: g, type: url-test, proxies: [a, b]}\n  - {name: all, type: select, include-all: true}\n  - {name: pp, type: select, use: [p]}\n" +
		"rules:\n  - GEOIP,Private,DIRECT\n  - DOMAIN-SUFFIX,example.com,g\n  - MATCH,Proxy\n"
	if _, err := clashProblem(good); err != nil {
		t.Fatalf("正常的配置被误报: %v", err)
	}
}

// Clash 订阅的线路组名和 sing-box JSON 同一套去重规则:撞了本站节点、外部节点、内置名、模板里写的名字就加序号,
// 整份配置按 mihomo 的规矩查得过。以前组名直接用线路名,撞上任何一个,分配了这条线路的用户 Clash 订阅整份导入失败。
func TestClashLineGroupNameClash(t *testing.T) {
	cert := raw(map[string]interface{}{"mode": "cert"})
	user := fullUser()
	tj := func(id uint, name string, port int) model.Line {
		return model.Line{Id: id, Name: name, Protocol: "trojan", Port: port, Tls: cert, Enabled: true}
	}
	withAddrs := func(l model.Line, addrs ...map[string]interface{}) model.Line {
		l.Addrs = raw(addrs)
		return l
	}
	extNode := func(name string) ExtItem {
		return ExtItem{Name: "ext", Clash: []map[string]interface{}{
			{"name": name, "type": "trojan", "server": "198.51.100.1", "port": 443, "password": "p", "sni": "x.example.com"},
		}}
	}
	single := []Entry{{Host: "1.2.3.4"}} // 单台服务器:节点名不带后缀,就是线路名
	cases := []struct {
		name     string
		lines    []model.Line
		entries  []Entry
		tmpl     string
		notice   string
		external []ExtItem
		proxies  []string            // 期望的代理名(依次)
		groups   map[string][]string // 期望的线路组:组名 → 成员
		top      []string            // 期望的 Proxy 选择组
	}{
		{
			name:    "自定义地址的备注为空:两个节点叫 hk、hk 2,线路组改叫 hk 3",
			lines:   []model.Line{withAddrs(tj(1, "hk", 9443), map[string]interface{}{"server": "1.2.3.4"}, map[string]interface{}{"server": "5.6.7.8"})},
			entries: twoServers,
			proxies: []string{"hk", "hk 2"},
			groups:  map[string][]string{"hk 3": {"hk", "hk 2"}},
			top:     []string{"Auto", "hk 3"},
		},
		{
			name: "主地址不写备注、备用地址写 -v6(线路表单的占位符就是这么写的)",
			lines: []model.Line{withAddrs(tj(1, "hk", 9443), map[string]interface{}{"server": "1.2.3.4"},
				map[string]interface{}{"server": "2001:db8::1", "remark": "-v6"})},
			entries: twoServers,
			proxies: []string{"hk", "hk-v6"},
			groups:  map[string][]string{"hk 2": {"hk", "hk-v6"}},
			top:     []string{"Auto", "hk 2"},
		},
		{
			name:    "线路名撞两种格式的内置名",
			lines:   []model.Line{tj(1, "Auto", 1001), tj(2, "DIRECT", 1002), tj(3, "auto", 1003), tj(4, "GLOBAL", 1004)},
			entries: twoServers,
			proxies: []string{"Auto-A", "Auto-B", "DIRECT-A", "DIRECT-B", "auto-A", "auto-B", "GLOBAL-A", "GLOBAL-B"},
			groups: map[string][]string{"Auto 2": {"Auto-A", "Auto-B"}, "DIRECT 2": {"DIRECT-A", "DIRECT-B"},
				"auto 2": {"auto-A", "auto-B"}, "GLOBAL 2": {"GLOBAL-A", "GLOBAL-B"}},
			top: []string{"Auto", "Auto 2", "DIRECT 2", "auto 2", "GLOBAL 2"},
		},
		{
			name:    "单台服务器的线路名撞 Proxy、REJECT:节点改名",
			lines:   []model.Line{tj(1, "Proxy", 1001), tj(2, "REJECT", 1002)},
			entries: single,
			proxies: []string{"Proxy 2", "REJECT 2"},
			groups:  map[string][]string{},
			top:     []string{"Auto", "Proxy 2", "REJECT 2"},
		},
		{
			name:     "外部节点和线路同名、和 Auto 同名:外部节点改名,线路组照旧叫 hy",
			lines:    []model.Line{tj(1, "hy", 1001)},
			entries:  twoServers,
			external: []ExtItem{extNode("hy"), extNode("Auto")},
			proxies:  []string{"hy-A", "hy-B", "hy 2", "Auto 2"},
			groups:   map[string][]string{"hy": {"hy-A", "hy-B"}},
			top:      []string{"Auto", "hy", "hy 2", "Auto 2"},
		},
		{
			name:     "外部节点和流量提示节点同名",
			lines:    []model.Line{tj(1, "hy", 1001)},
			entries:  single,
			notice:   "00-勿选-流量",
			external: []ExtItem{extNode("00-勿选-流量")},
			proxies:  []string{"00-勿选-流量", "hy", "00-勿选-流量 2"},
			groups:   map[string][]string{},
			top:      []string{"00-勿选-流量", "Auto", "hy", "00-勿选-流量 2"},
		},
		{
			name:    "模板里的节点和线路同名:线路组改名",
			lines:   []model.Line{tj(1, "hy", 1001)},
			entries: twoServers,
			tmpl:    "proxies:\n  - {name: hy, type: socks5, server: 9.9.9.9, port: 1080}\n",
			proxies: []string{"hy", "hy-A", "hy-B"},
			groups:  map[string][]string{"hy 2": {"hy-A", "hy-B"}},
			top:     []string{"Auto", "hy 2"},
		},
		{
			name:    "模板里的策略组和单台线路的节点同名:节点改名",
			lines:   []model.Line{tj(1, "tj", 1001)},
			entries: single,
			tmpl:    "proxy-groups:\n  - {name: tj, type: select, proxies: [DIRECT]}\n",
			proxies: []string{"tj 2"},
			groups:  map[string][]string{},
			top:     []string{"Auto", "tj 2"},
		},
		{
			name:    "模板的 proxy-providers 和线路同名:线路组改名",
			lines:   []model.Line{tj(1, "hy", 1001)},
			entries: twoServers,
			tmpl:    "proxy-providers:\n  hy: {type: file, path: ./hy.yaml}\n",
			proxies: []string{"hy-A", "hy-B"},
			groups:  map[string][]string{"hy 2": {"hy-A", "hy-B"}},
			top:     []string{"Auto", "hy 2"},
		},
	}
	for _, c := range cases {
		body, err := BuildClashFor(user, c.lines, c.entries, c.tmpl, c.notice, c.external, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		doc := checkClash(t, body)
		if got := doc.proxyNames(); !reflect.DeepEqual(got, c.proxies) {
			t.Fatalf("%s:代理名\n得 %v\n应 %v", c.name, got, c.proxies)
		}
		if _, got := doc.lineGroups(); !reflect.DeepEqual(got, c.groups) {
			t.Fatalf("%s:线路组\n得 %v\n应 %v", c.name, got, c.groups)
		}
		if got := stringList(doc.group("Proxy")["proxies"]); !reflect.DeepEqual(got, c.top) {
			t.Fatalf("%s:Proxy 选择组\n得 %v\n应 %v", c.name, got, c.top)
		}
	}
}

// 线路组在外部节点之前起名(Clash 侧):外部节点出现、消失,线路组都叫 hy;和它同名的外部节点改叫 hy 2。
func TestClashLineGroupKeepsNameOverExternal(t *testing.T) {
	cert := raw(map[string]interface{}{"mode": "cert"})
	lines := []model.Line{{Id: 1, Name: "hy", Protocol: "hysteria2", Port: 443, Tls: cert, Enabled: true}}
	ext := []ExtItem{{Name: "ext", Clash: []map[string]interface{}{
		{"name": "hy", "type": "trojan", "server": "198.51.100.1", "port": 443, "password": "p", "sni": "x.example.com"},
	}}}
	for _, external := range [][]ExtItem{ext, nil} {
		body, err := BuildClashFor(fullUser(), lines, twoServers, "", "", external, nil)
		if err != nil {
			t.Fatal(err)
		}
		doc := checkClash(t, body)
		if _, groups := doc.lineGroups(); !reflect.DeepEqual(groups, map[string][]string{"hy": {"hy-A", "hy-B"}}) {
			t.Fatalf("线路组应一直叫 hy(外部节点 %d 组): %v", len(external), groups)
		}
		if external != nil && !reflect.DeepEqual(doc.proxyNames(), []string{"hy-A", "hy-B", "hy 2"}) {
			t.Fatalf("和线路组同名的外部节点应改名为 hy 2: %v", doc.proxyNames())
		}
	}
}

// 同一份输入,Clash 订阅和 sing-box JSON 起出来的线路组名一样(用户换客户端还认得),两份配置也都加载得了。
func TestLineGroupNamesSameInClashAndSingBox(t *testing.T) {
	cert := raw(map[string]interface{}{"mode": "cert"})
	user := fullUser()
	tj := func(id uint, name string, port int, addrs ...map[string]interface{}) model.Line {
		l := model.Line{Id: id, Name: name, Protocol: "trojan", Port: port, Tls: cert, Enabled: true}
		if len(addrs) > 0 {
			l.Addrs = raw(addrs)
		}
		return l
	}
	ext := func(name string) ExtItem {
		return ExtItem{Name: "ext",
			Links: []string{"trojan://p@198.51.100.1:443?sni=x.example.com#" + strings.ReplaceAll(name, " ", "%20")},
			Clash: []map[string]interface{}{{"name": name, "type": "trojan", "server": "198.51.100.1", "port": 443, "password": "p", "sni": "x.example.com"}}}
	}
	cases := []struct {
		name     string
		lines    []model.Line
		external []ExtItem
		want     []string
	}{
		{"备注为空", []model.Line{tj(1, "hk", 9443, map[string]interface{}{"server": "1.2.3.4"}, map[string]interface{}{"server": "5.6.7.8"})}, nil, []string{"hk 3"}},
		{"备用地址 -v6", []model.Line{tj(1, "hk", 9443, map[string]interface{}{"server": "1.2.3.4"}, map[string]interface{}{"server": "2001:db8::1", "remark": "-v6"})}, nil, []string{"hk 2"}},
		{"内置名", []model.Line{tj(1, "auto", 1001), tj(2, "Auto", 1002), tj(3, "proxy", 1003), tj(4, "Proxy", 1004), tj(5, "direct", 1005), tj(6, "DIRECT", 1006), tj(7, "REJECT", 1007), tj(8, "GLOBAL", 1008)},
			nil, []string{"auto 2", "Auto 2", "proxy 2", "Proxy 2", "direct 2", "DIRECT 2", "REJECT 2", "GLOBAL 2"}},
		{"外部节点同名", []model.Line{tj(1, "hy", 1001), tj(2, "jp", 1002)}, []ExtItem{ext("hy"), ext("jp 2")}, []string{"hy", "jp"}},
		{"普通", []model.Line{tj(1, "香港", 1001), tj(2, "日本", 1002)}, nil, []string{"香港", "日本"}},
	}
	for _, c := range cases {
		body, err := BuildClashFor(user, c.lines, twoServers, "", "", c.external, nil)
		if err != nil {
			t.Fatal(err)
		}
		clash, _ := checkClash(t, body).lineGroups()
		res, err := BuildSingBoxSub(user, c.lines, Options{Entries: twoServers, External: c.external})
		if err != nil {
			t.Fatal(err)
		}
		validateClientConfig(t, res.Body)
		sb := singBoxLineGroups(parseSB(t, res.Body))
		if !reflect.DeepEqual(clash, c.want) || !reflect.DeepEqual(sb, c.want) {
			t.Fatalf("%s:两种格式的线路组名应一样\nClash    %v\nsing-box %v\n应       %v", c.name, clash, sb, c.want)
		}
	}
}

// Clash 模板里写了和线路同名的策略组:照旧用模板的(放在生成的那个位置),Proxy 列的就是它,不另补 hy 2;
// 模板的这个组会绕回 Proxy(直接列、经别的组)时,用了就成环、mihomo 整份拒收,线路组改名另起,模板的不动;
// 模板自己写了 Proxy 时生成的 Proxy 不出,同名的组列着 Proxy 也不成环,照样用模板的。
func TestClashTemplateLineGroup(t *testing.T) {
	cert := raw(map[string]interface{}{"mode": "cert"})
	lines := []model.Line{{Id: 1, Name: "hy", Protocol: "trojan", Port: 1001, Tls: cert, Enabled: true}}
	build := func(tmpl string) clashDoc {
		t.Helper()
		body, err := BuildClashFor(fullUser(), lines, twoServers, tmpl, "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return checkClash(t, body)
	}
	groupNames := func(d clashDoc) []string {
		var out []string
		for _, g := range d.Groups {
			out = append(out, g["name"].(string))
		}
		return out
	}

	doc := build("proxy-groups:\n  - {name: hy, type: fallback, proxies: [hy-B, hy-A]}\n")
	if g := doc.group("hy"); g["type"] != "fallback" || doc.group("hy 2") != nil {
		t.Fatalf("模板写了同名的策略组应用模板的,不再补 hy 2: %v", doc.Groups)
	}
	if got := groupNames(doc); !reflect.DeepEqual(got, []string{"Proxy", "Auto", "hy"}) {
		t.Fatalf("模板的组放在生成的那个位置: %v", got)
	}
	if got := stringList(doc.group("Proxy")["proxies"]); !reflect.DeepEqual(got, []string{"Auto", "hy"}) {
		t.Fatalf("Proxy 应列模板的线路组: %v", got)
	}

	doc = build("proxy-groups:\n  - {name: Proxy, type: select, proxies: [Auto, DIRECT]}\n  - {name: hy, type: select, proxies: [Proxy, hy-A, hy-B]}\n")
	if doc.group("hy")["type"] != "select" || doc.group("hy 2") != nil {
		t.Fatalf("模板写了 Proxy 时同名的组照样用模板的: %v", doc.Groups)
	}

	for name, tmpl := range map[string]string{
		"直接列了 Proxy": "proxy-groups:\n  - {name: hy, type: select, proxies: [Proxy, DIRECT]}\n",
		"经别的组绕回":     "proxy-groups:\n  - {name: hy, type: select, proxies: [中转]}\n  - {name: 中转, type: select, proxies: [Proxy, DIRECT]}\n",
	} {
		doc := build(tmpl)
		if g := doc.group("hy"); g["type"] != "select" {
			t.Fatalf("%s:模板的 hy 应原样留着: %v", name, g)
		}
		if g := doc.group("hy 2"); g["type"] != "url-test" || !reflect.DeepEqual(stringList(g["proxies"]), []string{"hy-A", "hy-B"}) {
			t.Fatalf("%s:线路组应改名为 hy 2: %v", name, g)
		}
		if got := stringList(doc.group("Proxy")["proxies"]); !reflect.DeepEqual(got, []string{"Auto", "hy 2"}) {
			t.Fatalf("%s:Proxy 应列改了名的线路组: %v", name, got)
		}
	}
}
