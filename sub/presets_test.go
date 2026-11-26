package sub

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database/model"

	"gopkg.in/yaml.v3"
)

// 订阅模板的现成写法(sub.Presets):每一套在真的客户端内核上跑过分流(tests/rs/t70-templates.mjs),这里守结构 ——
// 内置的两套和留空时用的一字不差(「默认的保持不变」);其余的能解析、规则指到的策略组和规则集都在、规则集都经代理下载、
// DNS 不强求 DoH(用户:「有的人也不需要doh」),拿真实的线路生成出来的订阅内嵌内核认。
func TestPresetsListAndBuiltins(t *testing.T) {
	ps := Presets()
	want := []string{"clash-builtin", "clash-cn-lite", "clash-cn-ruleset", "clash-gfw", "singbox-builtin", "singbox-cn", "singbox-gfw"}
	if len(ps) != len(want) {
		t.Fatalf("现成模板应有 %d 套,实际 %d 套", len(want), len(ps))
	}
	for i, p := range ps {
		if p.ID != want[i] {
			t.Errorf("第 %d 套应是 %s,实际 %s(前端的选择框按这个顺序和名字列)", i+1, want[i], p.ID)
		}
		if kind := strings.SplitN(p.ID, "-", 2)[0]; p.Kind != kind || (kind != "clash" && kind != "singbox") {
			t.Errorf("%s 的格式写成了 %q", p.ID, p.Kind)
		}
		if strings.TrimSpace(p.Body) == "" {
			t.Errorf("%s 是空的", p.ID)
		}
	}
	if ps[0].Body != DefaultClashTemplate() || ps[0].Body != basicClashConfig {
		t.Error("Clash 的内置那套必须和留空时用的一字不差")
	}
	if ps[4].Body != DefaultSingBoxTemplate() {
		t.Error("sing-box 的内置那套必须和留空时用的一字不差")
	}
	// 内置的保持不变:钉住它的规则(全部走代理)。要改内置模板,先想清楚所有留空的面板都会跟着变
	if !strings.Contains(basicClashConfig, "  - GEOIP,Private,DIRECT\n  - MATCH,Proxy\n") || strings.Contains(basicClashConfig, "rule-providers") {
		t.Error("内置的 Clash 模板变了")
	}
	base := singBoxBase()
	if base["route"].(map[string]interface{})["final"] != "proxy" || base["route"].(map[string]interface{})["rule_set"] != nil {
		t.Error("内置的 sing-box 模板变了")
	}
}

func presetLines() []model.Line {
	return []model.Line{
		{Id: 1, Name: "hy", Protocol: "hysteria2", Port: 443, Tls: raw(map[string]interface{}{"mode": "cert"})},
		{Id: 2, Name: "vl", Protocol: "vless", Port: 8888, Tls: raw(map[string]interface{}{"mode": "reality", "reality": map[string]interface{}{
			"private_key": "priv", "public_key": "MJnakD6uWVj6xdd5BTOIo8yP6WRiAciOtTcF_23k50A", "short_ids": []string{"abcd1234"},
			"handshake_server": "www.microsoft.com", "handshake_port": 443}}), Options: raw(map[string]interface{}{"vision": true})},
	}
}

func TestClashPresets(t *testing.T) {
	for _, p := range Presets() {
		if p.Kind != "clash" || p.ID == "clash-builtin" {
			continue
		}
		t.Run(p.ID, func(t *testing.T) {
			var root map[string]interface{}
			if err := yaml.Unmarshal([]byte(p.Body), &root); err != nil {
				t.Fatalf("不是合法的 YAML: %v", err)
			}
			if !strings.HasPrefix(p.Body, "# ") {
				t.Error("开头要有一段注释说明这套是干什么的(管理员在框里看得到)")
			}
			for _, k := range []string{"proxies", "proxy-groups"} {
				if _, ok := root[k]; ok {
					t.Errorf("模板里不该写 %s:节点和策略组由面板填", k)
				}
			}
			// DNS:国内的普通解析,不是 DoH;留着换 DoH 的注释
			dns, _ := root["dns"].(map[string]interface{})
			for _, k := range []string{"nameserver", "default-nameserver"} {
				list := stringList(dns[k])
				if len(list) == 0 {
					t.Errorf("dns.%s 是空的", k)
				}
				for _, s := range list {
					if _, err := netip.ParseAddr(s); err != nil {
						t.Errorf("dns.%s 里的 %q 不是普通的 DNS 地址(这几套不强求 DoH)", k, s)
					}
				}
			}
			if dns["enhanced-mode"] != "fake-ip" || !strings.Contains(p.Body, "https://doh.pub/dns-query") {
				t.Error("应是 fake-ip,并留着换成 DoH 的注释")
			}
			// 规则:目标只能是内置的直连 / 拒绝,或面板生成的 Proxy;RULE-SET 指到的规则集都在
			providers, _ := root["rule-providers"].(map[string]interface{})
			used := map[string]bool{}
			rules := stringList(root["rules"])
			if len(rules) < 3 || !strings.HasPrefix(rules[len(rules)-1], "MATCH,") {
				t.Fatalf("规则最后一条应是 MATCH: %v", rules)
			}
			proxied := false
			for _, r := range rules {
				f := strings.Split(r, ",")
				target := f[len(f)-1]
				if target == "no-resolve" {
					target = f[len(f)-2]
				}
				switch target {
				case "Proxy":
					proxied = true
				case "DIRECT", "REJECT":
				default:
					t.Errorf("规则 %q 的目标 %q 不是 DIRECT / REJECT / Proxy", r, target)
				}
				if f[0] == "RULE-SET" {
					used[f[1]] = true
					if _, ok := providers[f[1]]; !ok {
						t.Errorf("规则 %q 用的规则集 %s 没定义", r, f[1])
					}
				}
			}
			if !proxied {
				t.Error("没有一条规则走 Proxy")
			}
			for name, v := range providers {
				m, _ := v.(map[string]interface{})
				if !used[name] {
					t.Errorf("规则集 %s 定义了没用上", name)
				}
				url, _ := m["url"].(string)
				if m["type"] != "http" || m["format"] != "mrs" || m["proxy"] != "Proxy" || !strings.HasPrefix(url, mrs) || !strings.HasSuffix(url, ".mrs") {
					t.Errorf("规则集 %s 应是经 Proxy 下载的 mrs: %v", name, m)
				}
				if b := m["behavior"]; (b != "domain" && b != "ipcidr") || (b == "ipcidr") != strings.Contains(url, "/geoip/") {
					t.Errorf("规则集 %s 的 behavior 和地址对不上: %v", name, m)
				}
			}
			if (p.ID == "clash-cn-lite") != (len(providers) == 0) {
				t.Errorf("只有精简版不用规则集: %s 有 %d 个", p.ID, len(providers))
			}
			// 关掉「自动生成默认策略组」时 Proxy 照样要生成(规则引用着它)
			if need := ClashTemplateNeedsDefaultGroups(p.Body); len(need) == 0 || need[0] != "Proxy" {
				t.Errorf("模板引用着 Proxy,关掉默认策略组时也得生成: %v", need)
			}
			// 拿真实的线路生成订阅:开关开、关两种都要有 Proxy 组,规则和规则集原样带上
			for _, gen := range []bool{true, false} {
				out, err := BuildClashFor(fullUser(), presetLines(), hkEntry, p.Body, "", nil, nil, gen)
				if err != nil {
					t.Fatalf("生成订阅: %v", err)
				}
				var got map[string]interface{}
				if err := yaml.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("生成的订阅不是合法 YAML: %v", err)
				}
				names := map[string]bool{"DIRECT": true, "REJECT": true}
				for _, g := range got["proxy-groups"].([]interface{}) {
					names[g.(map[string]interface{})["name"].(string)] = true
				}
				if !names["Proxy"] || !names["Auto"] {
					t.Errorf("默认策略组开关 = %v 时订阅里应有 Proxy 和 Auto: %v", gen, names)
				}
				if len(got["proxies"].([]interface{})) != 2 || len(stringList(got["rules"])) != len(rules) {
					t.Errorf("节点或规则数不对: %d 个节点、%d 条规则", len(got["proxies"].([]interface{})), len(stringList(got["rules"])))
				}
				if gp, _ := got["rule-providers"].(map[string]interface{}); len(gp) != len(providers) {
					t.Errorf("规则集没原样带上: %d / %d", len(gp), len(providers))
				}
			}
		})
	}
}

func TestSingBoxPresets(t *testing.T) {
	for _, p := range Presets() {
		if p.Kind != "singbox" || p.ID == "singbox-builtin" {
			continue
		}
		t.Run(p.ID, func(t *testing.T) {
			var tmpl map[string]interface{}
			if err := json.Unmarshal([]byte(p.Body), &tmpl); err != nil {
				t.Fatalf("不是合法的 JSON: %v", err)
			}
			if _, ok := tmpl["outbounds"]; ok {
				t.Error("模板里不该写 outbounds:节点和分组由面板填")
			}
			// 入站、实验项和内置的一样:只改 DNS 和分流
			base := singBoxBase()
			for _, k := range []string{"inbounds", "experimental", "log"} {
				a, _ := json.Marshal(tmpl[k])
				b, _ := json.Marshal(base[k])
				if string(a) != string(b) {
					t.Errorf("%s 应和内置模板一样", k)
				}
			}
			res, err := BuildSingBoxSub(fullUser(), presetLines(), Options{Entries: hkEntry, SingBoxTmpl: p.Body})
			if err != nil {
				t.Fatalf("生成订阅: %v", err)
			}
			var cfg struct {
				DNS struct {
					Servers []map[string]interface{} `json:"servers"`
					Rules   []map[string]interface{} `json:"rules"`
					Final   string                   `json:"final"`
				} `json:"dns"`
				Outbounds []map[string]interface{} `json:"outbounds"`
				Route     struct {
					Rules    []map[string]interface{} `json:"rules"`
					RuleSet  []map[string]interface{} `json:"rule_set"`
					Final    string                   `json:"final"`
					Resolver string                   `json:"default_domain_resolver"`
				} `json:"route"`
			}
			if err := json.Unmarshal([]byte(res.Body), &cfg); err != nil {
				t.Fatal(err)
			}
			outs := map[string]bool{}
			for _, o := range cfg.Outbounds {
				outs[o["tag"].(string)] = true
			}
			if !outs["proxy"] || !outs["auto"] || !outs["direct"] || !outs["hy"] || !outs["vl"] {
				t.Fatalf("订阅里应有 proxy / auto / direct 和两个节点: %v", outs)
			}
			servers := map[string]bool{}
			for _, s := range cfg.DNS.Servers {
				servers[s["tag"].(string)] = true
				if d, ok := s["detour"].(string); ok && !outs[d] {
					t.Errorf("DNS 服务器 %v 的出站 %s 不存在", s["tag"], d)
				}
				if s["type"] == "https" || s["type"] == "tls" || s["type"] == "quic" || s["type"] == "h3" {
					t.Errorf("DNS 服务器 %v 用了加密 DNS(这几套不强求 DoH)", s)
				}
			}
			if !servers[cfg.DNS.Final] || !servers[cfg.Route.Resolver] {
				t.Errorf("dns.final(%s)/ default_domain_resolver(%s)指到的服务器不存在", cfg.DNS.Final, cfg.Route.Resolver)
			}
			sets := map[string]bool{}
			for _, rs := range cfg.Route.RuleSet {
				tag, _ := rs["tag"].(string)
				sets[tag] = false
				url, _ := rs["url"].(string)
				// download_detour 在 sing-box 1.14 起标了过时、1.16 拿掉(换成 http_client,1.14 才有):现在市面上的客户端从 1.11 到 1.15 都认它,
				// 先用它;等 1.16 临近再换,见 presets.go
				if rs["type"] != "remote" || rs["format"] != "binary" || rs["download_detour"] != "proxy" || !strings.HasPrefix(url, srs) || !strings.HasSuffix(url, ".srs") {
					t.Errorf("规则集 %s 应是经 proxy 下载的 srs: %v", tag, rs)
				}
			}
			if len(sets) == 0 {
				t.Fatal("没有规则集")
			}
			refs := func(rules []map[string]interface{}, what string) {
				for _, r := range rules {
					for _, tag := range stringList(r["rule_set"]) {
						if _, ok := sets[tag]; !ok {
							t.Errorf("%s规则用的规则集 %s 没定义", what, tag)
						}
						sets[tag] = true
					}
					if o, ok := r["outbound"].(string); ok && !outs[o] {
						t.Errorf("%s规则的出站 %s 不存在", what, o)
					}
					if s, ok := r["server"].(string); ok && !servers[s] {
						t.Errorf("%s规则的服务器 %s 不存在", what, s)
					}
				}
			}
			refs(cfg.Route.Rules, "分流")
			refs(cfg.DNS.Rules, "DNS ")
			for tag, used := range sets {
				if !used {
					t.Errorf("规则集 %s 定义了没用上", tag)
				}
			}
			if !outs[cfg.Route.Final] || (p.ID == "singbox-cn") != (cfg.Route.Final == "proxy") || (p.ID == "singbox-gfw") != (cfg.Route.Final == "direct") {
				t.Errorf("route.final = %s", cfg.Route.Final)
			}
			// 内嵌内核干跑(服务端构建没有 tun 入站,去掉它再验;字段名、类型、过时项都在这里暴露)
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
				t.Fatalf("内嵌 sing-box 不认生成的配置: %v", err)
			}
		})
	}
}
