package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"

	"gopkg.in/yaml.v3"
)

// v6MinusTemplate 用户的 v6 Clash 模板去掉 Proxy/Auto 定义后的版本:
// 规则只指到自己的手动选择组,6 个策略组全是模板自己写的。
const v6MinusTemplate = `mixed-port: 7890
allow-lan: false
mode: rule
log-level: info
external-controller: 127.0.0.1:9090
dns:
  enable: true
  ipv6: false
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  nameserver:
    - https://doh.pub/dns-query
    - https://1.0.0.1/dns-query
rules:
  - GEOIP,Private,DIRECT
  - MATCH,手动选择
proxy-groups:
  - name: GLOBAL
    type: select
    proxies:
      - 美国主线路
      - 德国[备用节点]
    include-all: true
    filter: '^(?!美国主线路$|德国\[备用节点\]$).*$'
  - name: 手动选择
    type: select
    proxies:
      - 美国主线路
      - 德国[备用节点]
    include-all: true
    filter: '^(?!美国主线路$|德国\[备用节点\]$).*$'
  - name: 宝可梦
    type: select
    include-all: true
    filter: '丨'
  - name: isp住宅
    type: select
    include-all: true
    filter: '^isp-|US-静态|前置-|🆓|👉|vless-ym|\|'
  - name: 其他
    type: select
    include-all: true
    filter: '^[A-Z]{2}-[A-Z]'
  - name: 前置-机场
    type: url-test
    url: http://www.gstatic.com/generate_204
    interval: 300
    tolerance: 50
    include-all: true
    filter: '^isp-前置-|^isp-🆓无敌前置-'
`

func clashGroupsOf(t *testing.T, out string) []interface{} {
	t.Helper()
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(out), &root); err != nil {
		t.Fatalf("输出不是合法 YAML: %v", err)
	}
	groups, _ := root["proxy-groups"].([]interface{})
	return groups
}

func clashGroupNames(t *testing.T, out string) []string {
	t.Helper()
	names := []string{}
	for _, g := range clashGroupsOf(t, out) {
		m, _ := g.(map[string]interface{})
		n, _ := m["name"].(string)
		names = append(names, n)
	}
	return names
}

func clashRulesOf(t *testing.T, out string) []string {
	t.Helper()
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(out), &root); err != nil {
		t.Fatalf("输出不是合法 YAML: %v", err)
	}
	rules, _ := root["rules"].([]interface{})
	out2 := []string{}
	for _, r := range rules {
		s, _ := r.(string)
		out2 = append(out2, s)
	}
	return out2
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func oneLine() []model.Line {
	return []model.Line{{Name: "美国主线路", Protocol: "hysteria2", Port: 49140}}
}

// (a) 开关开(默认):行为和现在完全一样。
func TestDefaultGroupsOn(t *testing.T) {
	// 无模板:Proxy/Auto 照旧生成。
	out, err := BuildClashFor(mkUser(), oneLine(), hkEntry, "", "", nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	names := clashGroupNames(t, out)
	if len(names) != 2 || names[0] != "Proxy" || names[1] != "Auto" {
		t.Fatalf("开关开、无模板时应恰好生成 Proxy、Auto,实际 %v", names)
	}

	// 模板里定义了 Proxy/Auto:同名顶替照旧工作(hidden 保留)。
	tmpl := "rules:\n  - MATCH,Proxy\nproxy-groups:\n" +
		"  - name: Proxy\n    type: select\n    proxies: [Auto]\n    include-all: true\n    hidden: true\n" +
		"  - name: Auto\n    type: url-test\n    url: http://www.gstatic.com/generate_204\n    interval: 300\n    tolerance: 50\n    include-all: true\n    hidden: true\n"
	out, err = BuildClashFor(mkUser(), oneLine(), hkEntry, tmpl, "", nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	groups := clashGroupsOf(t, out)
	if len(groups) != 2 {
		t.Fatalf("开关开、模板顶替时应恰好 2 个组,实际 %d", len(groups))
	}
	if findGroup(groups, "Proxy")["hidden"] != true || findGroup(groups, "Auto")["hidden"] != true {
		t.Errorf("模板顶替的 Proxy/Auto 应保留 hidden: true,实际 %+v", groups)
	}
}

// (b) 开关关 + v6 模板(去掉 Proxy/Auto 定义):订阅里彻底没有 Proxy/Auto。
func TestDefaultGroupsOffV6(t *testing.T) {
	out, err := BuildClashFor(mkUser(), oneLine(), hkEntry, v6MinusTemplate, "", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	names := clashGroupNames(t, out)
	for _, banned := range []string{"Proxy", "Auto"} {
		if hasName(names, banned) {
			t.Fatalf("开关关时订阅里不应有 %s,实际 %v", banned, names)
		}
	}
	for _, want := range []string{"GLOBAL", "手动选择", "宝可梦", "isp住宅", "其他", "前置-机场"} {
		if !hasName(names, want) {
			t.Errorf("开关关时 %s 组应保留,实际 %v", want, names)
		}
	}
	if len(names) != 6 {
		t.Errorf("开关关时应恰好 6 个组,实际 %v", names)
	}
	rules := clashRulesOf(t, out)
	if len(rules) != 2 || rules[0] != "GEOIP,Private,DIRECT" || rules[1] != "MATCH,手动选择" {
		t.Errorf("规则应原样保留,实际 %v", rules)
	}
}

// (c) 开关关但模板还引用 Proxy/Auto:被引用的组仍生成(不破坏现有配置)。
func TestDefaultGroupsOffReferenced(t *testing.T) {
	// 规则引用 Proxy:Proxy 生成,Auto 跟着生成(Proxy 的选单里列着 Auto)。
	tmpl := "rules:\n  - MATCH,Proxy\nproxy-groups:\n  - name: MyGroup\n    type: select\n    include-all: true\n"
	out, err := BuildClashFor(mkUser(), oneLine(), hkEntry, tmpl, "", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	names := clashGroupNames(t, out)
	if !hasName(names, "Proxy") || !hasName(names, "Auto") || !hasName(names, "MyGroup") {
		t.Fatalf("规则引用 Proxy 时应生成 Proxy、Auto,实际 %v", names)
	}

	// 规则只引用 Auto:只生成 Auto,不生成 Proxy。
	tmpl = "rules:\n  - MATCH,Auto\nproxy-groups:\n  - name: MyGroup\n    type: select\n    include-all: true\n"
	out, err = BuildClashFor(mkUser(), oneLine(), hkEntry, tmpl, "", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	names = clashGroupNames(t, out)
	if hasName(names, "Proxy") || !hasName(names, "Auto") {
		t.Fatalf("规则只引用 Auto 时应只生成 Auto,实际 %v", names)
	}

	// 策略组成员引用 Auto:Auto 生成,Proxy 不生成。
	tmpl = "rules:\n  - MATCH,MyGroup\nproxy-groups:\n  - name: MyGroup\n    type: select\n    proxies: [Auto]\n    include-all: true\n"
	out, err = BuildClashFor(mkUser(), oneLine(), hkEntry, tmpl, "", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	names = clashGroupNames(t, out)
	if hasName(names, "Proxy") || !hasName(names, "Auto") {
		t.Fatalf("策略组引用 Auto 时应只生成 Auto,实际 %v", names)
	}

	// 外部节点的 dialer-proxy 引用 Proxy:Proxy、Auto 都生成。
	ext := []ExtItem{{Name: "ext", Clash: []map[string]interface{}{
		{"name": "ext1", "type": "ss", "server": "1.2.3.4", "port": 8388, "cipher": "aes-256-gcm", "password": "x", "dialer-proxy": "Proxy"},
	}}}
	out, err = BuildClashFor(mkUser(), oneLine(), hkEntry, v6MinusTemplate, "", ext, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	names = clashGroupNames(t, out)
	if !hasName(names, "Proxy") || !hasName(names, "Auto") {
		t.Fatalf("节点 dialer-proxy 引用 Proxy 时应生成 Proxy、Auto,实际 %v", names)
	}

	// 无模板:内置骨架的规则引用 Proxy,照旧生成(开关关了也坏不了默认配置)。
	out, err = BuildClashFor(mkUser(), oneLine(), hkEntry, "", "", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	names = clashGroupNames(t, out)
	if !hasName(names, "Proxy") || !hasName(names, "Auto") {
		t.Fatalf("无模板时应照旧生成 Proxy、Auto,实际 %v", names)
	}

	// 模板没写 rules:后面会补默认规则 MATCH,Proxy,照旧生成。
	tmpl = "proxy-groups:\n  - name: MyGroup\n    type: select\n    include-all: true\n"
	out, err = BuildClashFor(mkUser(), oneLine(), hkEntry, tmpl, "", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	names = clashGroupNames(t, out)
	if !hasName(names, "Proxy") || !hasName(names, "Auto") {
		t.Fatalf("模板没写规则时应照旧生成 Proxy、Auto,实际 %v", names)
	}
	if rules := clashRulesOf(t, out); len(rules) != 2 || rules[1] != "MATCH,Proxy" {
		t.Errorf("模板没写规则时应补默认规则,实际 %v", rules)
	}

	// 模板自己定义了 Proxy:用模板的版本,不再额外生成(引用已被模板的定义满足)。
	tmpl = "rules:\n  - MATCH,Proxy\nproxy-groups:\n" +
		"  - name: Proxy\n    type: select\n    proxies: [Auto]\n    include-all: true\n" +
		"  - name: Auto\n    type: url-test\n    url: http://www.gstatic.com/generate_204\n    interval: 300\n    tolerance: 50\n    include-all: true\n"
	out, err = BuildClashFor(mkUser(), oneLine(), hkEntry, tmpl, "", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	names = clashGroupNames(t, out)
	if len(names) != 2 || !hasName(names, "Proxy") || !hasName(names, "Auto") {
		t.Fatalf("模板自己定义 Proxy/Auto 时应只有模板的 2 个组,实际 %v", names)
	}
	if !strings.Contains(out, "include-all: true") {
		t.Errorf("模板定义的组应原样保留,输出里找不到 include-all")
	}
}
