package render

import (
	"context"
	"encoding/json"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	R "github.com/sagernet/sing-box/route/rule"
	M "github.com/sagernet/sing/common/metadata"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

func TestParseRouteRules(t *testing.T) {
	got, err := ParseRouteRules(json.RawMessage(`[
		{"type":"domain_suffix","values":[" Netflix.COM ","netflix.com","","nflxvideo.net"],"to":2},
		{"type":"ip_cidr","values":["91.108.4.0/22","1.2.3.4","91.108.5.9/22","::ffff:8.8.8.8","2001:db8::1"],"to":0},
		{"type":"port","values":["443","8000-9000","25:25"],"to":-1}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"type":"domain_suffix","values":["netflix.com","nflxvideo.net"],"to":2},` +
		`{"type":"ip_cidr","values":["91.108.4.0/22","1.2.3.4/32","8.8.8.8/32","2001:db8::1/128"],"to":0},` +
		`{"type":"port","values":["443","8000:9000","25"],"to":-1}]`
	if b, _ := json.Marshal(got); string(b) != want {
		t.Fatalf("整理结果不对:\n%s\n要:\n%s", b, want)
	}
	for _, s := range []string{"", "null", "[]"} {
		if r, err := ParseRouteRules(json.RawMessage(s)); err != nil || len(r) != 0 {
			t.Fatalf("%q 应当是没有规则: %v %v", s, r, err)
		}
	}
	for raw, wantErr := range map[string]string{
		`[{"type":"geosite","values":["cn"],"to":0}]`:            "不支持的匹配方式",
		`[{"type":"domain","values":["  "],"to":0}]`:             "没有填匹配内容",
		`[{"type":"domain","values":["https://x.com/"],"to":0}]`: "不是域名",
		`[{"type":"domain_suffix","values":["1.2.3.4"],"to":0}]`: "是 IP",
		`[{"type":"domain_keyword","values":["a b"],"to":0}]`:    "空格",
		`[{"type":"ip_cidr","values":["x.com"],"to":0}]`:         "不是 IP",
		`[{"type":"port","values":["0"],"to":0}]`:                "不是端口",
		`[{"type":"port","values":["9000-8000"],"to":0}]`:        "不是端口",
		`[{"type":"port","values":["70000"],"to":0}]`:            "不是端口",
		`[{"type":"domain","values":["a.com"],"to":-2}]`:         "出口无效",
		`{"type":"domain"}`: "不是合法 JSON",
		`[{"type":"domain","values":["a.com"]},{"type":"x","values":["b"]}]`: "第 2 条",
	} {
		if _, err := ParseRouteRules(json.RawMessage(raw)); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("%s 应报 %q,实际 %v", raw, wantErr, err)
		}
	}
	many := make([]RouteRule, maxRouteRules+1)
	for i := range many {
		many[i] = RouteRule{Type: "domain", Values: []string{"a.com"}}
	}
	b, _ := json.Marshal(many)
	if _, err := ParseRouteRules(b); err == nil {
		t.Fatal("超过条数上限要拒绝")
	}
}

func TestLineUpstreamsIncludesRuleTargets(t *testing.T) {
	l := model.Line{UpstreamId: 1, RouteRules: json.RawMessage(`[{"type":"domain","values":["a.com"],"to":2},{"type":"domain","values":["b.com"],"to":1},{"type":"domain","values":["c.com"],"to":0},{"type":"domain","values":["d.com"],"to":-1},{"type":"domain","values":["e.com"],"to":3}]`)}
	got := LineUpstreams(l)
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("应是 [1 2 3](自己选的 + 规则指向的,去重,不含直连 / 拦截),实际 %v", got)
	}
	if got := LineUpstreams(model.Line{}); len(got) != 0 {
		t.Fatalf("直连、无规则的线路一个上游都不用: %v", got)
	}
}

// evalRoute 用 sing-box 自己的规则引擎把渲染出来的路由规则从上往下过一遍(和数据面的匹配顺序一样)。
// 目的地是域名时,resolve 动作用 answers 模拟本机解析结果;resolved 表示有没有在本机解析过。
func evalRoute(t *testing.T, raw []byte, inbound, dst string, port uint16, answers ...string) (out string, resolved bool) {
	t.Helper()
	var cfg struct {
		Route struct {
			Rules []json.RawMessage `json:"rules"`
			Final string            `json:"final"`
		} `json:"route"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	md := adapter.InboundContext{Inbound: inbound, Network: "tcp"}
	if ip, err := netip.ParseAddr(dst); err == nil {
		md.Destination = M.SocksaddrFrom(ip, port)
		md.IPVersion = 4
		if ip.Is6() {
			md.IPVersion = 6
		}
	} else {
		md.Destination = M.Socksaddr{Fqdn: dst, Port: port}
	}
	logger := log.NewNOPFactory().Logger()
	for i, b := range cfg.Route.Rules {
		var opt option.Rule
		if err := opt.UnmarshalJSONContext(ctx, b); err != nil {
			t.Fatalf("规则 %d 解不开: %v\n%s", i, err, b)
		}
		r, err := R.NewRule(ctx, logger, opt, false)
		if err != nil {
			t.Fatalf("规则 %d 建不起来: %v\n%s", i, err, b)
		}
		md.ResetRuleCache()
		matched := r.Match(&md)
		_ = r.Close()
		if !matched {
			continue
		}
		switch a := r.Action().(type) {
		case *R.RuleActionSniff, *R.RuleActionHijackDNS:
		case *R.RuleActionResolve:
			if md.Destination.IsFqdn() {
				resolved = true
				md.DestinationAddresses = nil
				for _, s := range answers {
					md.DestinationAddresses = append(md.DestinationAddresses, netip.MustParseAddr(s))
				}
			}
		case *R.RuleActionReject:
			return "reject", resolved
		case *R.RuleActionRoute:
			return a.Outbound, resolved
		default:
			t.Fatalf("规则 %d 的动作没料到: %T", i, a)
		}
	}
	return cfg.Route.Final, resolved
}

func splitDB(t *testing.T) (*gorm.DB, func()) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	ss := func(pw string) []byte { return []byte(`{"method":"aes-256-gcm","password":"` + pw + `"}`) }
	db.Create(&model.Upstream{Name: "warp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`)})
	db.Create(&model.Upstream{Name: "us", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40001}`)})
	db.Create(&model.Upstream{Name: "unused", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40002}`)})
	db.Create(&model.Upstream{Name: "jp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40003}`)}) // 只有分流规则用它
	// 默认走 warp,规则:netflix → us、带 ads 的拦掉、某段 IP 直连、25 端口拦掉、cn.example 直连;
	// 后两条和前面重叠的(www.netflix.com 直连)不该生效 —— 第一条命中的算
	db.Create(&model.Line{Name: "split", Protocol: "shadowsocks", Port: 30011, Enabled: true, UpstreamId: 1, Options: ss("a"),
		RouteRules: json.RawMessage(`[
			{"type":"domain_suffix","values":["netflix.com"],"to":2},
			{"type":"domain_keyword","values":["ads"],"to":-1},
			{"type":"ip_cidr","values":["91.108.4.0/22"],"to":0},
			{"type":"port","values":["25"],"to":-1},
			{"type":"domain_suffix","values":["cn.example"],"to":0},
			{"type":"domain","values":["www.netflix.com"],"to":0},
			{"type":"domain_suffix","values":["jp.example"],"to":4}
		]`)})
	// 默认直连,规则:openai 走 warp
	db.Create(&model.Line{Name: "direct-split", Protocol: "shadowsocks", Port: 30012, Enabled: true, Options: ss("b"),
		RouteRules: json.RawMessage(`[{"type":"domain_suffix","values":["openai.com"],"to":1}]`)})
	// 没有规则的直连 / 上游线路:和没有这个功能时一样
	db.Create(&model.Line{Name: "plain", Protocol: "shadowsocks", Port: 30013, Enabled: true, Options: ss("c")})
	db.Create(&model.Line{Name: "plain-us", Protocol: "shadowsocks", Port: 30014, Enabled: true, UpstreamId: 2, Options: ss("d")})
	return db, func() { database.Close(db) }
}

func TestRouteRulesRender(t *testing.T) {
	tdb, done := splitDB(t)
	defer done()
	raw, err := BuildConfig(tdb, NodeCert{})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.ValidateConfig(raw); err != nil {
		t.Fatalf("带分流规则的配置要通过 sing-box 干跑: %v\n%s", err, raw)
	}
	for _, c := range []struct {
		in, dst  string
		port     uint16
		answers  []string
		out      string
		resolved bool
	}{
		{"split", "www.netflix.com", 443, nil, "us", false}, // 第一条命中;后面那条直连不生效
		{"split", "netflix.com", 443, nil, "us", false},     // 后缀含域名本身
		{"split", "notnetflix.com", 443, nil, "warp", false},
		{"split", "x.ads.example", 443, nil, "reject", false},
		{"split", "91.108.5.1", 443, nil, "direct", false},
		{"split", "91.108.8.1", 443, nil, "warp", false},
		{"split", "a.jp.example", 443, nil, "jp", false},
		{"split", "mail.example.net", 25, nil, "reject", false},
		{"split", "a.cn.example", 443, []string{"1.2.3.4"}, "direct", true},   // 直连的那部分在本机解析
		{"split", "a.cn.example", 443, []string{"10.0.0.1"}, "reject", true},  // 解析到内网照样拦
		{"split", "10.0.0.1", 443, nil, "reject", false},                      // 写成内网 IP 的不管走哪都拦
		{"split", "www.google.com", 443, []string{"10.0.0.1"}, "warp", false}, // 走上游的不在本机解析
		{"direct-split", "chat.openai.com", 443, []string{"10.0.0.5"}, "warp", false},
		{"direct-split", "example.org", 443, []string{"93.184.216.34"}, "direct", true},
		{"direct-split", "example.org", 443, []string{"192.168.1.1"}, "reject", true},
		{"plain", "example.org", 443, []string{"93.184.216.34"}, "direct", true},
		{"plain", "example.org", 443, []string{"127.0.0.1"}, "reject", true},
		{"plain-us", "example.org", 443, []string{"127.0.0.1"}, "us", false},
	} {
		out, resolved := evalRoute(t, raw, c.in, c.dst, c.port, c.answers...)
		if out != c.out || resolved != c.resolved {
			t.Errorf("%s → %s:%d(解析 %v):得到 %s / 本机解析=%v,要 %s / %v", c.in, c.dst, c.port, c.answers, out, resolved, c.out, c.resolved)
		}
	}
	var cfg struct {
		Outbounds []struct {
			Tag string `json:"tag"`
		} `json:"outbounds"`
	}
	_ = json.Unmarshal(raw, &cfg)
	tags := map[string]bool{}
	for _, o := range cfg.Outbounds {
		tags[o.Tag] = true
	}
	if !tags["us"] || !tags["warp"] || !tags["jp"] || tags["unused"] {
		t.Fatalf("规则指向的上游要渲染出站,没人用的不渲染: %v", tags)
	}

	// 放行内网时一条 resolve 都不该有,分流照常
	tdb.Create(&model.Setting{Key: "allowPrivate", Value: "true"})
	raw, err = BuildConfig(tdb, NodeCert{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"resolve"`) {
		t.Fatalf("allowPrivate=true 时不该有 resolve: %s", raw)
	}
	if out, _ := evalRoute(t, raw, "split", "www.netflix.com", 443); out != "us" {
		t.Fatalf("放行内网时分流照常,得到 %s", out)
	}
	if err := core.ValidateConfig(raw); err != nil {
		t.Fatal(err)
	}
}

// 规则指向的上游被删(正常删不掉,见 web 的引用检查;这里模拟库被手改)要明说,不能悄悄改走默认出口。
func TestRouteRulesMissingUpstream(t *testing.T) {
	tdb, done := splitDB(t)
	defer done()
	tdb.Delete(&model.Upstream{}, 2)
	if _, err := BuildConfig(tdb, NodeCert{}); err == nil || !strings.Contains(err.Error(), "不存在的上游 #2") {
		t.Fatalf("应报规则指向不存在的上游,实际 %v", err)
	}
}

// 规则名只用于显示:整理后保留(去控制字符、首尾空白,最长 32 字),不进数据面配置。
func TestRouteRuleName(t *testing.T) {
	long := strings.Repeat("长", 40)
	raw := json.RawMessage(`[{"name":"  抖音\u0007 ","type":"domain_suffix","values":["amemv.com"],"to":0},{"name":"` + long + `","type":"domain","values":["x.com"],"to":0},{"type":"port","values":["25"],"to":-1}]`)
	rules, err := ParseRouteRules(raw)
	if err != nil {
		t.Fatal(err)
	}
	if rules[0].Name != "抖音" || len([]rune(rules[1].Name)) != 32 || rules[2].Name != "" {
		t.Fatalf("规则名整理不对: %q %q %q", rules[0].Name, rules[1].Name, rules[2].Name)
	}
	b, _ := json.Marshal(rules[2])
	if strings.Contains(string(b), "name") {
		t.Fatalf("没有名字的规则不该带 name 字段: %s", b)
	}
	m, err := routeRuleJSON("in", rules[0], nil)
	if err != nil || strings.Contains(string(m), "抖音") || strings.Contains(string(m), "name") {
		t.Fatalf("规则名不该进数据面配置: %s %v", m, err)
	}
}
