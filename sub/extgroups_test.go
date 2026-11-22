package sub

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"gopkg.in/yaml.v3"
)

// 每个外部订阅一个策略组:组名=订阅名,成员=该订阅的节点;空订阅不生成组;GLOBAL 排第一,订阅组排最后。
// 节点撞组名时改的是组名(「单节点 2」),节点名和没有分组时一字不差;组列在 Proxy 里,在 Proxy 里选得到。
func TestExternalSubGroups(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)}
	lines := []model.Line{{Name: "本站", Protocol: "hysteria2", Port: 1, Enabled: true}}
	entries := []Entry{{Host: "1.2.3.4"}}
	tmpl := `proxy-groups:
  - name: GLOBAL
    type: select
    proxies: [本站]
  - name: 前置-机场
    type: select
    proxies: [DIRECT]
rules:
  - MATCH,GLOBAL
`
	external := []ExtItem{
		{
			Name: "机场A",
			Clash: []map[string]interface{}{
				{"name": "机场A-1", "type": "trojan", "server": "9.9.9.9", "port": 443, "password": "pw", "sni": "t.example.com"},
				{"name": "机场A-2", "type": "ss", "server": "8.8.8.8", "port": 1, "cipher": "aes-128-gcm", "password": "x"},
			},
		},
		{
			Name:  "单节点",
			Clash: []map[string]interface{}{{"name": "单节点", "type": "ss", "server": "7.7.7.7", "port": 1, "cipher": "aes-128-gcm", "password": "x"}}, // 组名和节点名重名 → 组改名
		},
		{
			Name:  "空订阅",
			Clash: nil, // 空订阅不生成组
		},
	}
	out, err := BuildClashFull(user, lines, entries, tmpl, "", external)
	if err != nil {
		t.Fatal(err)
	}
	checkClash(t, out)
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(out), &root); err != nil {
		t.Fatalf("订阅 YAML 解析失败: %v\n%s", err, out)
	}
	groups, _ := root["proxy-groups"].([]interface{})
	byName := map[string]map[string]interface{}{}
	var order []string
	for _, g := range groups {
		m, _ := g.(map[string]interface{})
		n, _ := m["name"].(string)
		byName[n] = m
		order = append(order, n)
	}
	if len(order) == 0 || order[0] != "GLOBAL" {
		t.Fatalf("GLOBAL 应排第一,实际顺序 %v", order)
	}
	if n := len(order); n < 2 || order[n-2] != "机场A" || order[n-1] != "单节点 2" {
		t.Fatalf("订阅组应排最后(机场A、单节点 2),实际顺序 %v", order)
	}
	if _, ok := byName["空订阅"]; ok {
		t.Fatalf("空订阅不应生成组,实际顺序 %v", order)
	}
	members := func(name string) []string {
		g, ok := byName[name]
		if !ok {
			t.Fatalf("缺少组 %q,实际顺序 %v", name, order)
		}
		return stringList(g["proxies"])
	}
	if got := members("机场A"); strings.Join(got, ",") != "机场A-1,机场A-2" {
		t.Fatalf("机场A 组成员应为 [机场A-1 机场A-2],实际 %v", got)
	}
	if got := members("单节点 2"); strings.Join(got, ",") != "单节点" {
		t.Fatalf("单节点 2 组成员应为 [单节点](撞名时组改名,节点不改),实际 %v", got)
	}
	// 外部节点仍进 Auto 组(回归:proxyNames 不能丢)
	if got := strings.Join(members("Auto"), ","); got != "本站,机场A-1,机场A-2,单节点" {
		t.Fatalf("Auto 组应含全部节点,实际 [%s]", got)
	}
	// Proxy:本站之后、外部节点之前是订阅组;外部节点照旧平铺在后面
	if got := strings.Join(members("Proxy"), ","); got != "Auto,本站,机场A,单节点 2,机场A-1,机场A-2,单节点" {
		t.Fatalf("Proxy 应为 [Auto 本站 机场A 单节点 2 机场A-1 机场A-2 单节点],实际 [%s]", got)
	}
	// 节点名没被分组改掉
	var names []string
	for _, p := range root["proxies"].([]interface{}) {
		m, _ := p.(map[string]interface{})
		names = append(names, m["name"].(string))
	}
	if strings.Join(names, ",") != "本站,机场A-1,机场A-2,单节点" {
		t.Fatalf("节点名应和不分组时一样,实际 %v", names)
	}
}

// 关了「单独成组」的订阅不成组,但节点照常追加进 Proxy / Auto;组名不占位,同名节点不改名;没关的订阅照常成组。
func TestExternalSubNoGroup(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)}
	lines := []model.Line{{Name: "本站", Protocol: "hysteria2", Port: 1, Enabled: true}}
	entries := []Entry{{Host: "1.2.3.4"}}
	tmpl := `proxy-groups:
  - name: GLOBAL
    type: select
    proxies: [本站]
rules:
  - MATCH,GLOBAL
`
	external := []ExtItem{
		{
			Name: "机场A",
			Clash: []map[string]interface{}{
				{"name": "机场A-1", "type": "ss", "server": "9.9.9.9", "port": 1, "cipher": "aes-128-gcm", "password": "x"},
			},
		},
		{
			Name:    "德国",
			NoGroup: true,
			Clash: []map[string]interface{}{
				{"name": "德国", "type": "ss", "server": "8.8.8.8", "port": 1, "cipher": "aes-128-gcm", "password": "x"},
				{"name": "德国-2", "type": "ss", "server": "7.7.7.7", "port": 1, "cipher": "aes-128-gcm", "password": "x"},
			},
		},
	}
	out, err := BuildClashFull(user, lines, entries, tmpl, "", external)
	if err != nil {
		t.Fatal(err)
	}
	checkClash(t, out)
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(out), &root); err != nil {
		t.Fatalf("订阅 YAML 解析失败: %v\n%s", err, out)
	}
	groups, _ := root["proxy-groups"].([]interface{})
	byName := map[string]map[string]interface{}{}
	for _, g := range groups {
		m, _ := g.(map[string]interface{})
		n, _ := m["name"].(string)
		byName[n] = m
	}
	if _, ok := byName["德国"]; ok {
		t.Fatalf("关了分组的“德国”不应生成组,实际组 %v", groupNames(groups))
	}
	g, ok := byName["机场A"]
	if !ok {
		t.Fatalf("没关分组的“机场A”应生成组,实际组 %v", groupNames(groups))
	}
	if got := strings.Join(stringList(g["proxies"]), ","); got != "机场A-1" {
		t.Fatalf("机场A 组成员应为 [机场A-1],实际 [%s]", got)
	}
	// 关了分组的节点仍进 Auto 组,也平铺在 Proxy 里
	auto := strings.Join(stringList(byName["Auto"]["proxies"]), ",")
	for _, want := range []string{"机场A-1", "德国", "德国-2"} {
		if !strings.Contains(auto, want) {
			t.Fatalf("Auto 组应含 %q,实际 [%s]", want, auto)
		}
	}
	if got := strings.Join(stringList(byName["Proxy"]["proxies"]), ","); got != "Auto,本站,机场A,机场A-1,德国,德国-2" {
		t.Fatalf("Proxy 应为 [Auto 本站 机场A 机场A-1 德国 德国-2],实际 [%s]", got)
	}
	// 组名没占位:节点“德国”不改名
	found := false
	for _, p := range root["proxies"].([]interface{}) {
		if m, _ := p.(map[string]interface{}); m["name"] == "德国" {
			found = true
		}
		if m, _ := p.(map[string]interface{}); m["name"] == "德国 2" {
			t.Fatalf("节点“德国”不应改名(组名没占位):\n%s", out)
		}
	}
	if !found {
		t.Fatalf("节点“德国”应在 proxies 里:\n%s", out)
	}
}

func groupNames(groups []interface{}) []string {
	var out []string
	for _, g := range groups {
		if m, ok := g.(map[string]interface{}); ok {
			if n, _ := m["name"].(string); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

// 没有要成组的外部订阅时(没分配外部节点、全是分享链接或都关了分组),订阅和加分组之前一字不差:
// 拿同一份输入、把外部节点全标成不成组算出来的,和直接不给外部节点时的本站部分比,外部节点只多在 proxies / Auto / Proxy 的末尾。
func TestExtGroupsNothingToGroupKeepsOutput(t *testing.T) {
	user := fullUser()
	lines := []model.Line{{Id: 1, Name: "hy", Protocol: "hysteria2", Port: 443, Tls: raw(map[string]interface{}{"mode": "cert"}), Enabled: true}}
	ext := []ExtItem{
		{Name: "链接", NoGroup: true, Clash: []map[string]interface{}{{"name": "DE-1", "type": "ss", "server": "198.51.100.7", "port": 1, "cipher": "aes-128-gcm", "password": "x"}}},
		{Name: "空的", Clash: nil},
	}
	body, err := BuildClashFor(user, lines, twoServers, "", "00-提示", ext, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	doc := checkClash(t, body)
	var selects []string
	for _, g := range doc.Groups {
		if g["type"] == "select" {
			selects = append(selects, g["name"].(string))
		}
	}
	if !reflect.DeepEqual(selects, []string{"Proxy"}) {
		t.Fatalf("没有要成组的订阅时只该有 Proxy 一个选择组,实际 %v", selects)
	}
	if got := stringList(doc.group("Proxy")["proxies"]); !reflect.DeepEqual(got, []string{"00-提示", "Auto", "hy", "DE-1"}) {
		t.Fatalf("Proxy 应和以前一样 [00-提示 Auto hy DE-1],实际 %v", got)
	}
}

// 模板里写了和订阅同名的组(以前想按订阅分组只能这样手写):用模板的,不另生成一份,Proxy 里列的就是它。
// 模板的那个组会绕回生成的 Proxy、或者名字已经被线路组用了时不用它,订阅组加序号另起。整份配置 mihomo 都要能加载。
func TestExtGroupsUseTemplateGroup(t *testing.T) {
	user := fullUser()
	cert := raw(map[string]interface{}{"mode": "cert"})
	lines := []model.Line{{Id: 1, Name: "香港", Protocol: "trojan", Port: 9443, Tls: cert, Enabled: true}} // 两台服务器:出线路组「香港」
	node := func(name, server string) map[string]interface{} {
		return map[string]interface{}{"name": name, "type": "ss", "server": server, "port": 1, "cipher": "aes-128-gcm", "password": "x"}
	}
	ext := []ExtItem{
		{Name: "机场A", Clash: []map[string]interface{}{node("A-1", "198.51.100.1"), node("A-2", "198.51.100.2")}},
		{Name: "机场B", Clash: []map[string]interface{}{node("B-1", "198.51.100.3")}},
		{Name: "香港", Clash: []map[string]interface{}{node("HK-1", "198.51.100.4")}},
	}
	tmpl := `proxy-groups:
  - {name: 机场A, type: select, include-all-proxies: true, filter: "^A-"}
  - {name: 机场B, type: select, proxies: [Proxy, DIRECT]}
  - {name: 香港, type: select, proxies: [DIRECT]}
`
	body, err := BuildClashFor(user, lines, twoServers, tmpl, "", ext, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	doc := checkClash(t, body)
	count := map[string]int{}
	for _, g := range doc.Groups {
		count[g["name"].(string)]++
	}
	if count["机场A"] != 1 || doc.group("机场A")["filter"] != "^A-" {
		t.Fatalf("机场A 应只有模板写的那一个组,实际 %v\n%s", count, body)
	}
	if count["机场B"] != 1 || count["机场B 2"] != 1 || !reflect.DeepEqual(stringList(doc.group("机场B 2")["proxies"]), []string{"B-1"}) {
		t.Fatalf("模板的机场B 会绕回 Proxy,不能用:应另起「机场B 2」,实际 %v", count)
	}
	// 模板的「香港」已被线路组拿去用(模板写了用模板的),订阅「香港」另起「香港 2」
	if count["香港"] != 1 || count["香港 2"] != 1 || !reflect.DeepEqual(stringList(doc.group("香港 2")["proxies"]), []string{"HK-1"}) {
		t.Fatalf("订阅「香港」应另起「香港 2」,实际 %v", count)
	}
	want := []string{"Auto", "香港", "机场A", "机场B 2", "香港 2", "A-1", "A-2", "B-1", "HK-1"}
	if got := stringList(doc.group("Proxy")["proxies"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("Proxy\n得 %v\n应 %v", got, want)
	}
}

// 订阅名撞上内置名、线路名(单台服务器时节点就叫线路名)、外部节点名、模板的 proxy-providers:组名加序号,节点名不动。
func TestExtGroupNameCollisions(t *testing.T) {
	user := fullUser()
	cert := raw(map[string]interface{}{"mode": "cert"})
	lines := []model.Line{{Id: 1, Name: "hk", Protocol: "trojan", Port: 9443, Tls: cert, Enabled: true}}
	node := func(name string) map[string]interface{} {
		return map[string]interface{}{"name": name, "type": "ss", "server": "198.51.100.9", "port": 1, "cipher": "aes-128-gcm", "password": "x"}
	}
	ext := []ExtItem{
		{Name: "GLOBAL", Clash: []map[string]interface{}{node("g-1")}},
		{Name: "DIRECT", Clash: []map[string]interface{}{node("d-1")}},
		{Name: "hk", Clash: []map[string]interface{}{node("h-1")}},
		{Name: "pp", Clash: []map[string]interface{}{node("p-1")}},
		{Name: "x-1", Clash: []map[string]interface{}{node("x-1")}},
	}
	tmpl := "proxy-providers:\n  pp:\n    type: http\n    url: https://example.com/p.yaml\n    path: ./p.yaml\n"
	body, err := BuildClashFor(user, lines, []Entry{{Host: "1.2.3.4"}}, tmpl, "", ext, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	doc := checkClash(t, body)
	if got := doc.proxyNames(); !reflect.DeepEqual(got, []string{"hk", "g-1", "d-1", "h-1", "p-1", "x-1"}) {
		t.Fatalf("节点名不该被分组改掉,实际 %v", got)
	}
	want := []string{"Auto", "hk", "GLOBAL 2", "DIRECT 2", "hk 2", "pp 2", "x-1 2", "g-1", "d-1", "h-1", "p-1", "x-1"}
	if got := stringList(doc.group("Proxy")["proxies"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("Proxy\n得 %v\n应 %v", got, want)
	}
}

// 关了「自动生成默认策略组」、模板只用自己的组:订阅组照样生成(不在任何 Proxy 里,模板可以按名字用),排在最后。
func TestExtGroupsWithDefaultGroupsOff(t *testing.T) {
	user := fullUser()
	lines := []model.Line{{Id: 1, Name: "hy", Protocol: "hysteria2", Port: 443, Tls: raw(map[string]interface{}{"mode": "cert"}), Enabled: true}}
	ext := []ExtItem{{Name: "机场A", Clash: []map[string]interface{}{{"name": "A-1", "type": "ss", "server": "198.51.100.1", "port": 1, "cipher": "aes-128-gcm", "password": "x"}}}}
	tmpl := "rules:\n  - MATCH,我的组\nproxy-groups:\n  - {name: 我的组, type: select, proxies: [机场A, DIRECT]}\n"
	body, err := BuildClashFor(user, lines, twoServers, tmpl, "", ext, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	doc := checkClash(t, body)
	var names []string
	for _, g := range doc.Groups {
		names = append(names, g["name"].(string))
	}
	if !reflect.DeepEqual(names, []string{"我的组", "机场A"}) {
		t.Fatalf("应只有模板的组和订阅组,实际 %v", names)
	}
}

// 老内核 Clash:订阅里它不认识的协议去掉后,剩下的节点照样成组;一个都不剩的订阅不出组(空组整份导入失败)。
func TestExtGroupsLegacyClash(t *testing.T) {
	user := fullUser()
	lines := []model.Line{{Id: 1, Name: "ss", Protocol: "shadowsocks", Port: 8388, Options: raw(map[string]interface{}{"method": "aes-128-gcm"}), Enabled: true}}
	ext := []ExtItem{
		{Name: "混合", Clash: []map[string]interface{}{
			{"name": "M-ss", "type": "ss", "server": "198.51.100.1", "port": 1, "cipher": "aes-128-gcm", "password": "x"},
			{"name": "M-hy2", "type": "hysteria2", "server": "198.51.100.2", "port": 443, "password": "x"},
		}},
		{Name: "全是新协议", Clash: []map[string]interface{}{{"name": "N-vless", "type": "vless", "server": "198.51.100.3", "port": 443, "uuid": "8b9b4d6a-1c1e-4bd4-9a5a-2f4c3d2b1a00"}}},
	}
	res, err := BuildClashSub(user, lines, Options{Entries: []Entry{{Host: "1.2.3.4"}}, External: ext, LegacyClash: true, ClashDefaultGroups: true})
	if err != nil {
		t.Fatal(err)
	}
	doc := checkClash(t, res.Body)
	if g := doc.group("混合"); g == nil || !reflect.DeepEqual(stringList(g["proxies"]), []string{"M-ss"}) {
		t.Fatalf("老内核:混合 组应只剩 M-ss,实际 %v", g)
	}
	if doc.group("全是新协议") != nil {
		t.Fatal("老内核:一个节点都不剩的订阅不该出组")
	}
}
