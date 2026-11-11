package sub

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/upstream"
)

// legacySingBox 加线路组之前的 BuildSingBoxSub,原样留在测试里当对照:节点先整份平铺成链接再逐条解析。
// 改成逐条线路展开之后,节点的 tag、顺序、内容都必须和它一字不差 —— 客户端里已经选中的节点靠名字认,改一个字就丢了。
func legacySingBox(user model.User, lines []model.Line, opt Options) (Result, error) {
	links := GenerateLinksFor(user, lines, opt.Entries, opt.LineNodes)
	for _, e := range opt.External {
		links = append(links, e.Links...)
	}
	var tmpl map[string]interface{}
	if strings.TrimSpace(opt.SingBoxTmpl) != "" {
		if err := json.Unmarshal([]byte(opt.SingBoxTmpl), &tmpl); err != nil {
			return Result{}, fmt.Errorf("sing-box 模板不是合法 JSON: %w", err)
		}
	}
	var nodes []map[string]interface{}
	var tags []string
	used := map[string]bool{"proxy": true, "auto": true, "direct": true}
	var ownOutbounds []interface{}
	if raw, ok := tmpl["outbounds"].([]interface{}); ok {
		ownOutbounds = raw
		for _, o := range raw {
			if m, ok := o.(map[string]interface{}); ok {
				if t, _ := m["tag"].(string); t != "" {
					used[t] = true
				}
			}
		}
	}
	for _, l := range links {
		p, err := upstream.ParseLink(l)
		if err != nil {
			if p = httpLink(l); p == nil {
				continue
			}
		}
		tag := p.Name
		if tag == "" {
			tag = p.Type
		}
		for i := 2; used[tag]; i++ {
			tag = fmt.Sprintf("%s %d", p.Name, i)
		}
		used[tag] = true
		ob := make(map[string]interface{}, len(p.Options)+2)
		for k, v := range p.Options {
			ob[k] = v
		}
		ob["type"], ob["tag"] = p.Type, tag
		nodes = append(nodes, ob)
		tags = append(tags, tag)
	}
	if len(nodes) == 0 {
		cfg := map[string]interface{}{
			"log":       map[string]interface{}{"level": "info"},
			"outbounds": []map[string]interface{}{{"type": "direct", "tag": "direct"}},
			"route":     map[string]interface{}{"final": "direct"},
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		return Result{Body: string(b), Headers: headers(user, opt, "application/json; charset=utf-8")}, nil
	}
	outbounds := []map[string]interface{}{
		{"type": "selector", "tag": "proxy", "outbounds": append([]string{"auto"}, tags...), "default": "auto", "interrupt_exist_connections": true},
		{"type": "urltest", "tag": "auto", "outbounds": tags, "url": "http://www.gstatic.com/generate_204", "interval": "3m", "tolerance": 50},
	}
	outbounds = append(outbounds, nodes...)
	outbounds = append(outbounds, map[string]interface{}{"type": "direct", "tag": "direct"})
	if tmpl != nil {
		return singBoxFromTemplate(user, opt, tmpl, ownOutbounds, outbounds, nodes, tags)
	}
	cfg := singBoxBase()
	cfg["outbounds"] = outbounds
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Result{}, err
	}
	return Result{Body: string(b), Headers: headers(user, opt, "application/json; charset=utf-8")}, nil
}

// twoServers 两台服务器 A、B(一主一副),节点名带入口后缀。
var twoServers = []Entry{
	{Name: "A", Host: "a.example.com", SNI: "a.example.com", Suffix: "-A", NodeId: 1},
	{Name: "B", Host: "b.example.com", SNI: "b.example.com", Suffix: "-B", NodeId: 2},
}

type sbDoc struct {
	Outbounds []map[string]interface{} `json:"outbounds"`
}

func parseSB(t *testing.T, body string) sbDoc {
	t.Helper()
	var d sbDoc
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("不是合法 JSON: %v\n%s", err, body)
	}
	return d
}

func strList(v interface{}) []string {
	var out []string
	for _, x := range v.([]interface{}) {
		out = append(out, x.(string))
	}
	return out
}

// nodeOutbounds 出站里的节点(不含 proxy / auto / 线路组 / direct 这些组和内置出站)。
func nodeOutbounds(d sbDoc) []map[string]interface{} {
	var out []map[string]interface{}
	for _, o := range d.Outbounds {
		switch o["type"] {
		case "selector", "urltest", "direct":
			continue
		}
		out = append(out, o)
	}
	return out
}

func byTagSB(d sbDoc) map[string]map[string]interface{} {
	m := map[string]map[string]interface{}{}
	for _, o := range d.Outbounds {
		m[o["tag"].(string)] = o
	}
	return m
}

// singBoxProblem 照 sing-box 启动时的检查把出站之间的引用查一遍,返回第一个会让配置起不来的问题。
// 内嵌 sing-box 的干跑(core.ValidateConfig)只建实例、不启动,组成员找不到、出站互相引用成环它都查不出来:
// 这些在启动出站时才查(adapter/outbound/manager.go 的 startOutbounds),route / dns 找出站也在启动时。
//   - 出站的依赖 = selector / urltest 的成员 + detour,都要找得到,不能成环;tag 不能重复;selector 的 default 要在成员里;
//   - route 的 final 与规则的 outbound、dns 服务器的 detour 要找得到。
func singBoxProblem(body string) error {
	var doc struct {
		Outbounds []map[string]interface{} `json:"outbounds"`
		Route     struct {
			Final string                   `json:"final"`
			Rules []map[string]interface{} `json:"rules"`
		} `json:"route"`
		DNS struct {
			Servers []map[string]interface{} `json:"servers"`
		} `json:"dns"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return err
	}
	deps := map[string][]string{}
	for _, o := range doc.Outbounds {
		tag, _ := o["tag"].(string)
		if _, dup := deps[tag]; dup || tag == "" {
			return fmt.Errorf("duplicate outbound/endpoint tag: %q", tag)
		}
		members := stringList(o["outbounds"])
		if def, _ := o["default"].(string); def != "" && !slices.Contains(members, def) {
			return fmt.Errorf("outbound[%s]: default outbound not found: %s", tag, def)
		}
		if d, _ := o["detour"].(string); d != "" {
			members = append(members, d)
		}
		deps[tag] = members
	}
	for tag, ds := range deps {
		for _, d := range ds {
			if _, ok := deps[d]; !ok {
				return fmt.Errorf("dependency[%s] not found for outbound[%s]", d, tag)
			}
		}
	}
	for tag := range deps {
		for _, d := range deps[tag] {
			if reaches(d, tag, func(n string) []string { return deps[n] }) {
				return fmt.Errorf("circular outbound dependency: %s -> %s -> … -> %s", tag, d, tag)
			}
		}
	}
	refs := []string{doc.Route.Final}
	for _, r := range doc.Route.Rules {
		o, _ := r["outbound"].(string)
		refs = append(refs, o)
	}
	for _, s := range doc.DNS.Servers {
		d, _ := s["detour"].(string)
		refs = append(refs, d)
	}
	for _, r := range refs {
		if _, ok := deps[r]; r != "" && !ok {
			return fmt.Errorf("outbound not found: %s", r)
		}
	}
	return nil
}

// 检查器自己先过一遍:几种启动时才报的错它都得认出来,正常的配置不能误报。
func TestSingBoxCheckerCatchesBadConfigs(t *testing.T) {
	ob := func(list ...string) string { return `{"outbounds":[` + strings.Join(list, ",") + `]}` }
	direct := `{"type":"direct","tag":"direct"}`
	for name, body := range map[string]string{
		"组和 proxy 互相引用":  ob(`{"type":"selector","tag":"proxy","outbounds":["hy"]}`, `{"type":"selector","tag":"hy","outbounds":["proxy","direct"]}`, direct),
		"组列了自己":          ob(`{"type":"urltest","tag":"hy","outbounds":["hy","direct"]}`, direct),
		"经 detour 绕回":    ob(`{"type":"selector","tag":"proxy","outbounds":["relay"]}`, `{"type":"socks","tag":"relay","server":"127.0.0.1","server_port":1,"detour":"proxy"}`),
		"成员找不到":          ob(`{"type":"urltest","tag":"hy","outbounds":["hy-A","hy-B"]}`, direct),
		"default 不在成员里":  ob(`{"type":"selector","tag":"proxy","outbounds":["direct"],"default":"auto"}`, direct),
		"tag 重复":         ob(direct, direct),
		"route 出口找不到":    `{"outbounds":[` + direct + `],"route":{"final":"proxy"}}`,
		"dns detour 找不到": `{"outbounds":[` + direct + `],"dns":{"servers":[{"type":"https","tag":"remote","server":"1.1.1.1","detour":"proxy"}]}}`,
	} {
		if err := singBoxProblem(body); err == nil {
			t.Errorf("%s:检查器没认出来\n%s", name, body)
		}
	}
	good := `{"outbounds":[{"type":"selector","tag":"proxy","outbounds":["auto","hy","direct"],"default":"auto"},` +
		`{"type":"urltest","tag":"auto","outbounds":["direct","relay"]},{"type":"urltest","tag":"hy","outbounds":["relay"]},` +
		`{"type":"socks","tag":"relay","server":"127.0.0.1","server_port":1,"detour":"direct"},` + direct + `],` +
		`"route":{"final":"proxy","rules":[{"action":"sniff"},{"ip_is_private":true,"outbound":"direct"}]},` +
		`"dns":{"servers":[{"type":"https","tag":"remote","server":"1.1.1.1","detour":"proxy"},{"type":"local","tag":"local"}]}}`
	if err := singBoxProblem(good); err != nil {
		t.Fatalf("正常的配置被误报: %v", err)
	}
}

// validateClientConfig 内嵌 sing-box 干跑,再按启动时的规矩查出站之间的引用(干跑查不到,见 singBoxProblem)。
// 服务端构建没有注册 tun 入站(客户端才有),干跑前去掉它。
func validateClientConfig(t *testing.T, body string) {
	t.Helper()
	if err := singBoxProblem(body); err != nil {
		t.Fatalf("sing-box 启动时会拒绝生成的客户端配置: %v\n%s", err, body)
	}
	var full map[string]interface{}
	if err := json.Unmarshal([]byte(body), &full); err != nil {
		t.Fatal(err)
	}
	if ins, ok := full["inbounds"].([]interface{}); ok {
		var keep []interface{}
		for _, in := range ins {
			if in.(map[string]interface{})["type"] != "tun" {
				keep = append(keep, in)
			}
		}
		full["inbounds"] = keep
	}
	b, _ := json.Marshal(full)
	if err := core.ValidateConfig(b); err != nil {
		t.Fatalf("sing-box 拒绝生成的客户端配置: %v\n%s", err, body)
	}
}

// 多入口线路在 sing-box JSON 里和 Clash 一样出线路测速组:只有部署在不止一台服务器上、用户也没被收窄到一台的线路出组,
// mixed 不出;proxy 选择组列 auto、线路组、全部节点,auto 照旧覆盖全部节点;节点的名字、顺序、内容和改动前一字不差。
func TestSingBoxLineGroups(t *testing.T) {
	cert := raw(map[string]interface{}{"mode": "cert"})
	lines := []model.Line{
		{Id: 1, Name: "hy", Protocol: "hysteria2", Port: 443, Tls: cert, Enabled: true},
		{Id: 2, Name: "any", Protocol: "anytls", Port: 8443, Tls: cert, Enabled: true},                         // 用户只拿了 B 这一台
		{Id: 3, Name: "mx", Protocol: "mixed", Port: 1080, Enabled: true},                                      // 两台都有,但 mixed 不出组
		{Id: 4, Name: "tj", Protocol: "trojan", Port: 9443, Tls: cert, Enabled: true, NodeIds: raw([]uint{1})}, // 只部署在 A
	}
	opt := Options{Entries: twoServers, LineNodes: map[uint]map[uint]bool{2: {2: true}},
		External: []ExtItem{{Name: "ext", Links: []string{"trojan://p@198.51.100.1:443?sni=x.example.com#hy-A"}}}} // 外部节点和本站节点同名
	user := fullUser() // 凭据是随机生成的:对照的两次得用同一个用户
	res, err := BuildSingBoxSub(user, lines, opt)
	if err != nil {
		t.Fatal(err)
	}
	doc := parseSB(t, res.Body)
	tags := map[string]map[string]interface{}{}
	var groups []map[string]interface{}
	for _, o := range doc.Outbounds {
		tags[o["tag"].(string)] = o
		if o["type"] == "urltest" && o["tag"] != "auto" {
			groups = append(groups, o)
		}
	}
	if len(groups) != 1 || groups[0]["tag"] != "hy" {
		t.Fatalf("只有 hy 部署在两台服务器上、用户两台都有,只该出这一个线路组: %v", groups)
	}
	if got := strList(groups[0]["outbounds"]); !reflect.DeepEqual(got, []string{"hy-A", "hy-B"}) {
		t.Fatalf("线路组成员应是这条线路在两台服务器上的节点: %v", got)
	}
	if groups[0]["url"] != "http://www.gstatic.com/generate_204" || groups[0]["interval"] != "3m" || groups[0]["tolerance"] != float64(50) {
		t.Fatalf("线路组的测速参数应和 Clash 的 url-test 组、auto 组一样: %v", groups[0])
	}

	// 节点:名字、顺序、内容都和改动前一样(外部节点撞名照旧加序号)
	old, err := legacySingBox(user, lines, opt)
	if err != nil {
		t.Fatal(err)
	}
	gotNodes, wantNodes := nodeOutbounds(doc), nodeOutbounds(parseSB(t, old.Body))
	if !reflect.DeepEqual(gotNodes, wantNodes) {
		t.Fatalf("节点出站变了:\n得 %v\n应 %v", gotNodes, wantNodes)
	}
	var nodeTags []string
	for _, n := range gotNodes {
		nodeTags = append(nodeTags, n["tag"].(string))
	}
	// mixed 的 socks 与 http 两条链接备注相同,第二条照旧加序号
	if want := []string{"hy-A", "hy-B", "any-B", "mx-A", "mx-A 2", "mx-B", "mx-B 2", "tj-A", "hy-A 2"}; !reflect.DeepEqual(nodeTags, want) {
		t.Fatalf("节点 tag 或顺序不对:\n得 %v\n应 %v", nodeTags, want)
	}

	// proxy = auto + 线路组 + 全部节点,默认仍是 auto;auto 覆盖全部节点
	proxy := tags["proxy"]
	if got := strList(proxy["outbounds"]); !reflect.DeepEqual(got, append([]string{"auto", "hy"}, nodeTags...)) {
		t.Fatalf("proxy 选择组应列 auto、线路组、全部节点: %v", got)
	}
	if proxy["default"] != "auto" {
		t.Fatalf("proxy 默认仍应是 auto: %v", proxy["default"])
	}
	if got := strList(tags["auto"]["outbounds"]); !reflect.DeepEqual(got, nodeTags) {
		t.Fatalf("auto 应覆盖全部节点(不含线路组): %v", got)
	}
	// 出站顺序:proxy、auto、线路组、节点、direct
	var order []string
	for _, o := range doc.Outbounds {
		order = append(order, o["tag"].(string))
	}
	if want := append(append([]string{"proxy", "auto", "hy"}, nodeTags...), "direct"); !reflect.DeepEqual(order, want) {
		t.Fatalf("出站顺序不对:\n得 %v\n应 %v", order, want)
	}
	validateClientConfig(t, res.Body)
}

// 没有任何线路出组的订阅(单台服务器、或者都被收窄到一台):整份配置和改动前逐字节一样,老用户什么都看不出来。
func TestSingBoxNoLineGroupsUnchanged(t *testing.T) {
	cert := raw(map[string]interface{}{"mode": "cert"})
	lines := []model.Line{
		{Id: 1, Name: "hy", Protocol: "hysteria2", Port: 443, Tls: cert, Enabled: true},
		{Id: 2, Name: "any", Protocol: "anytls", Port: 8443, Tls: cert, Enabled: true},
	}
	user := fullUser()
	for name, opt := range map[string]Options{
		"单台服务器":     {Entries: hkEntry},
		"两台但都收窄到一台": {Entries: twoServers, LineNodes: map[uint]map[uint]bool{1: {1: true}, 2: {2: true}}},
		"带外部节点":     {Entries: hkEntry, External: []ExtItem{{Links: []string{"trojan://p@198.51.100.1:443?sni=x.example.com#ext"}}}},
		"模板":        {Entries: hkEntry, SingBoxTmpl: `{"route":{"final":"proxy"},"outbounds":[{"type":"selector","tag":"选","outbounds":["{all}"]}]}`},
	} {
		got, err := BuildSingBoxSub(user, lines, opt)
		if err != nil {
			t.Fatal(err)
		}
		want, err := legacySingBox(user, lines, opt)
		if err != nil {
			t.Fatal(err)
		}
		if got.Body != want.Body {
			t.Fatalf("%s:没有线路组时配置应和改动前逐字节一样\n得 %s\n应 %s", name, got.Body, want.Body)
		}
	}
}

// 线路组的名字等全部节点定完名再起:撞了 auto 这种内置名、或者撞了节点名(自定义地址的备注为空,节点就叫线路名),
// 只给组名加序号,节点名不动;改出来的配置照样能被 sing-box 加载。
func TestSingBoxLineGroupNameClash(t *testing.T) {
	cert := raw(map[string]interface{}{"mode": "cert"})
	lines := []model.Line{
		{Id: 1, Name: "auto", Protocol: "hysteria2", Port: 443, Tls: cert, Enabled: true},
		{Id: 2, Name: "hk", Protocol: "trojan", Port: 9443, Tls: cert, Enabled: true,
			Addrs: raw([]map[string]interface{}{{"server": "1.2.3.4"}, {"server": "5.6.7.8"}})}, // 备注为空:两个节点都叫 hk
	}
	user := fullUser()
	res, err := BuildSingBoxSub(user, lines, Options{Entries: twoServers})
	if err != nil {
		t.Fatal(err)
	}
	doc := parseSB(t, res.Body)
	by := byTagSB(doc)
	if g := by["auto 2"]; g == nil || g["type"] != "urltest" || !reflect.DeepEqual(strList(g["outbounds"]), []string{"auto-A", "auto-B"}) {
		t.Fatalf("叫 auto 的线路,组名应改成 auto 2: %v", g)
	}
	if by["auto"]["type"] != "urltest" || len(strList(by["auto"]["outbounds"])) != 4 {
		t.Fatalf("内置的 auto 组不能被线路组顶掉: %v", by["auto"])
	}
	if by["hk"]["type"] != "trojan" || by["hk 2"]["type"] != "trojan" {
		t.Fatalf("两个节点仍叫 hk、hk 2: %v %v", by["hk"], by["hk 2"])
	}
	if g := by["hk 3"]; g == nil || g["type"] != "urltest" || !reflect.DeepEqual(strList(g["outbounds"]), []string{"hk", "hk 2"}) {
		t.Fatalf("和节点撞名的线路组应改名为 hk 3,成员不变: %v", g)
	}
	old, err := legacySingBox(user, lines, Options{Entries: twoServers})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(nodeOutbounds(doc), nodeOutbounds(parseSB(t, old.Body))) {
		t.Fatal("组名撞名不能连累节点改名")
	}
	if got := strList(by["proxy"]["outbounds"]); !reflect.DeepEqual(got[:3], []string{"auto", "auto 2", "hk 3"}) {
		t.Fatalf("proxy 选择组应先列 auto 再列线路组: %v", got)
	}
	validateClientConfig(t, res.Body)
}

// 线路组在外部节点之前起名:外部订阅里冒出一个和线路同名的节点,改名的是外部节点(hy 2),线路组一直叫 hy。
// 外部节点出现、消失,线路组的名字都不变 —— 客户端按名字记住选择,选过线路组的不会被悄悄换到第三方节点上。
func TestSingBoxLineGroupKeepsNameOverExternal(t *testing.T) {
	cert := raw(map[string]interface{}{"mode": "cert"})
	lines := []model.Line{{Id: 1, Name: "hy", Protocol: "hysteria2", Port: 443, Tls: cert, Enabled: true}}
	user := fullUser()
	ext := []ExtItem{{Name: "ext", Links: []string{"trojan://p@198.51.100.1:443?sni=x.example.com#hy"}}}
	for _, opt := range []Options{{Entries: twoServers, External: ext}, {Entries: twoServers}} {
		res, err := BuildSingBoxSub(user, lines, opt)
		if err != nil {
			t.Fatal(err)
		}
		by := byTagSB(parseSB(t, res.Body))
		if g := by["hy"]; g == nil || g["type"] != "urltest" || !reflect.DeepEqual(strList(g["outbounds"]), []string{"hy-A", "hy-B"}) {
			t.Fatalf("线路组应一直叫 hy(外部节点 %d 组): %v", len(opt.External), g)
		}
		validateClientConfig(t, res.Body)
		if len(opt.External) == 0 {
			continue
		}
		if n := by["hy 2"]; n == nil || n["type"] != "trojan" || n["server"] != "198.51.100.1" {
			t.Fatalf("和线路组同名的外部节点应改名为 hy 2: %v", n)
		}
		if got := strList(by["proxy"]["outbounds"]); !reflect.DeepEqual(got, []string{"auto", "hy", "hy-A", "hy-B", "hy 2"}) {
			t.Fatalf("proxy 选择组应列 auto、线路组、全部节点: %v", got)
		}
		if got := strList(by["auto"]["outbounds"]); !reflect.DeepEqual(got, []string{"hy-A", "hy-B", "hy 2"}) {
			t.Fatalf("auto 应覆盖全部节点(含改了名的外部节点): %v", got)
		}
	}
}

// 模板模式:模板里 "{all}" 只展开成节点(不含线路组);线路组和 proxy / auto 一样补进出站,模板写了同名的 selector / urltest
// 就用模板的,不另补一份。同名的是别的出站(direct、节点),或者模板的这个组会顺着成员 / detour 绕回生成的 proxy
// (proxy 列着它,成环,sing-box 整份起不来),线路组改名另起,模板的不动。
func TestSingBoxLineGroupsWithTemplate(t *testing.T) {
	cert := raw(map[string]interface{}{"mode": "cert"})
	lines := []model.Line{{Id: 1, Name: "hy", Protocol: "hysteria2", Port: 443, Tls: cert, Enabled: true}}
	build := func(tmpl string) map[string]map[string]interface{} {
		t.Helper()
		res, err := BuildSingBoxSub(fullUser(), lines, Options{Entries: twoServers, SingBoxTmpl: tmpl})
		if err != nil {
			t.Fatal(err)
		}
		validateClientConfig(t, res.Body)
		return byTagSB(parseSB(t, res.Body))
	}
	by := build(`{"route":{"final":"流媒体","rules":[{"action":"sniff"}]},"outbounds":[{"type":"selector","tag":"流媒体","outbounds":["proxy","{all}"]}]}`)
	if got := strList(by["流媒体"]["outbounds"]); !reflect.DeepEqual(got, []string{"proxy", "hy-A", "hy-B"}) {
		t.Fatalf("{all} 只该展开成节点: %v", got)
	}
	if g := by["hy"]; g == nil || g["type"] != "urltest" {
		t.Fatalf("线路组应补进模板的出站: %v", g)
	}
	if got := strList(by["proxy"]["outbounds"]); !reflect.DeepEqual(got, []string{"auto", "hy", "hy-A", "hy-B"}) {
		t.Fatalf("补上的 proxy 应列线路组: %v", got)
	}

	// 模板自己写了同名的 urltest:用模板的(测速间隔照模板),不另补 hy 2;生成的 proxy 列的就是它
	by = build(`{"outbounds":[{"type":"urltest","tag":"hy","outbounds":["hy-B","hy-A"],"interval":"10m"}]}`)
	if g := by["hy"]; g["interval"] != "10m" || !reflect.DeepEqual(strList(g["outbounds"]), []string{"hy-B", "hy-A"}) || by["hy 2"] != nil {
		t.Fatalf("模板写了同名的 urltest 应用模板的,不再补 hy 2: %v / %v", g, by["hy 2"])
	}
	if got := strList(by["proxy"]["outbounds"]); !reflect.DeepEqual(got, []string{"auto", "hy", "hy-A", "hy-B"}) {
		t.Fatalf("proxy 应列模板的线路组: %v", got)
	}

	// 模板自己写了 proxy:生成的 proxy 不出,模板的 hy 列着 proxy 也不成环,照样用模板的
	by = build(`{"outbounds":[{"type":"selector","tag":"proxy","outbounds":["{all}"]},{"type":"selector","tag":"hy","outbounds":["proxy","{all}"]}]}`)
	if by["hy"]["type"] != "selector" || by["hy 2"] != nil {
		t.Fatalf("模板写了 proxy 时同名的 selector 照样用模板的: %v / %v", by["hy"], by["hy 2"])
	}

	// 同名的不是组,或者模板的 hy 会绕回生成的 proxy(直接列、经别的组、经 detour):线路组改名另起,模板的 hy 不动
	for name, c := range map[string]struct{ tmpl, first string }{ // first:模板的 hy 列的第一个成员
		"同名的是 direct": {`{"outbounds":[{"type":"direct","tag":"hy"}]}`, ""},
		"直接列了 proxy":  {`{"outbounds":[{"type":"selector","tag":"hy","outbounds":["proxy","{all}"]}]}`, "proxy"},
		"经别的组绕回":      {`{"outbounds":[{"type":"selector","tag":"hy","outbounds":["中转"]},{"type":"selector","tag":"中转","outbounds":["proxy","direct"]}]}`, "中转"},
		"经 detour 绕回": {`{"outbounds":[{"type":"urltest","tag":"hy","outbounds":["relay"]},{"type":"socks","tag":"relay","server":"127.0.0.1","server_port":1080,"detour":"proxy"}]}`, "relay"},
	} {
		by = build(c.tmpl)
		first := ""
		if l := stringList(by["hy"]["outbounds"]); len(l) > 0 {
			first = l[0]
		}
		if by["hy"] == nil || first != c.first {
			t.Fatalf("%s:模板的 hy 应原样留着: %v", name, by["hy"])
		}
		if g := by["hy 2"]; g == nil || g["type"] != "urltest" || !reflect.DeepEqual(strList(g["outbounds"]), []string{"hy-A", "hy-B"}) {
			t.Fatalf("%s:线路组应改名为 hy 2: %v", name, g)
		}
		if got := strList(by["proxy"]["outbounds"]); !reflect.DeepEqual(got, []string{"auto", "hy 2", "hy-A", "hy-B"}) {
			t.Fatalf("%s:proxy 应列改了名的线路组: %v", name, got)
		}
	}
}
