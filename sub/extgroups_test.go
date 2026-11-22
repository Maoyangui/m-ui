package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"gopkg.in/yaml.v3"
)

// 每个外部订阅一个策略组:组名=订阅名,成员=该订阅的节点;空订阅不生成组;
// 节点撞组名时节点改名,组成员记改名后的名字;GLOBAL 排第一,订阅组排最后。
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
			Clash: []map[string]interface{}{{"name": "单节点", "type": "ss", "server": "7.7.7.7", "port": 1, "cipher": "aes-128-gcm", "password": "x"}}, // 组名和节点名重名 → 节点改名
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
	if n := len(order); n < 2 || order[n-2] != "机场A" || order[n-1] != "单节点" {
		t.Fatalf("订阅组应排最后(机场A、单节点),实际顺序 %v", order)
	}
	if _, ok := byName["空订阅"]; ok {
		t.Fatalf("空订阅不应生成组,实际顺序 %v", order)
	}
	members := func(name string) []string {
		g, ok := byName[name]
		if !ok {
			t.Fatalf("缺少组 %q,实际顺序 %v", name, order)
		}
		var out []string
		for _, p := range stringList(g["proxies"]) {
			out = append(out, p)
		}
		return out
	}
	if got := members("机场A"); strings.Join(got, ",") != "机场A-1,机场A-2" {
		t.Fatalf("机场A 组成员应为 [机场A-1 机场A-2],实际 %v", got)
	}
	if got := members("单节点"); strings.Join(got, ",") != "单节点 2" {
		t.Fatalf("单节点 组成员应为 [单节点 2](节点撞组名已改名),实际 %v", got)
	}
	// 外部节点仍进 Auto 组(回归:proxyNames 不能丢)
	auto := strings.Join(members("Auto"), ",")
	for _, want := range []string{"机场A-1", "机场A-2", "单节点 2"} {
		if !strings.Contains(auto, want) {
			t.Fatalf("Auto 组应含 %q,实际 [%s]", want, auto)
		}
	}
	// 节点本身已改名,订阅里不应再有叫“单节点”的节点(只有同名组)
	for _, p := range root["proxies"].([]interface{}) {
		if m, _ := p.(map[string]interface{}); m["name"] == "单节点" {
			t.Fatalf("节点“单节点”撞组名,应已改名为“单节点 2”:\n%s", out)
		}
	}
}

// 关了“生成策略组”的订阅不成组,但节点照常追加进 Proxy / Auto;
// 组名不占位,同名节点不改名;没关的订阅照常成组。
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
	// 关了分组的节点仍进 Auto 组
	auto := strings.Join(stringList(byName["Auto"]["proxies"]), ",")
	for _, want := range []string{"机场A-1", "德国", "德国-2"} {
		if !strings.Contains(auto, want) {
			t.Fatalf("Auto 组应含 %q,实际 [%s]", want, auto)
		}
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
