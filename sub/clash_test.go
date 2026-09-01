package sub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fangjunsheng555/m-ui/database/model"

	"gopkg.in/yaml.v3"
)

func TestBuildClashStructure(t *testing.T) {
	user := mkUser()
	ssOpts, _ := json.Marshal(map[string]interface{}{"method": "aes-256-gcm"})
	lines := []model.Line{
		{Name: "香港1(流媒体解锁)", Protocol: "hysteria2", Port: 49140},
		{Name: "香港2(流媒体解锁)", Protocol: "anytls", Port: 30688},
		{Name: "网址", Protocol: "shadowsocks", Port: 29937, Options: ssOpts},
	}
	out, err := BuildClash(user, lines, hkEntry, "", "00-勿选-流量:1.0/不限GB 重置:7天")
	if err != nil {
		t.Fatal(err)
	}

	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(out), &root); err != nil {
		t.Fatalf("输出不是合法 YAML: %v", err)
	}

	proxies, _ := root["proxies"].([]interface{})
	if len(proxies) != 4 { // 3 线路 + 1 提示
		t.Fatalf("代理数应为 4,实际 %d", len(proxies))
	}
	first := proxies[0].(map[string]interface{})
	if !strings.HasPrefix(first["name"].(string), "00-勿选-流量") {
		t.Errorf("首个代理应为提示节点,实际 %v", first["name"])
	}

	// hy2 代理字段
	hy2 := findProxy(proxies, "香港1(流媒体解锁)")
	if hy2["type"] != "hysteria2" || hy2["password"] != "qJIq2Qzhug" || hy2["sni"] != "hk.joinvip.vip" {
		t.Errorf("hy2 代理字段不符: %+v", hy2)
	}
	if hy2["port"] != 49140 {
		t.Errorf("hy2 端口应为 49140,实际 %v", hy2["port"])
	}
	// anytls
	at := findProxy(proxies, "香港2(流媒体解锁)")
	if at["type"] != "anytls" || at["password"] != "qJIq2Qzhug" {
		t.Errorf("anytls 代理字段不符: %+v", at)
	}
	// ss
	ss := findProxy(proxies, "网址")
	if ss["type"] != "ss" || ss["cipher"] != "aes-256-gcm" {
		t.Errorf("ss 代理字段不符: %+v", ss)
	}

	// 分组:Proxy(select) 与 Auto(url-test),Auto 含全部真实代理(不含提示)
	groups, _ := root["proxy-groups"].([]interface{})
	if len(groups) != 2 {
		t.Fatalf("分组数应为 2,实际 %d", len(groups))
	}
	auto := findGroup(groups, "Auto")
	autoProxies, _ := auto["proxies"].([]interface{})
	if len(autoProxies) != 3 {
		t.Errorf("Auto 组应含 3 个真实代理,实际 %d", len(autoProxies))
	}
	proxyGrp := findGroup(groups, "Proxy")
	pl, _ := proxyGrp["proxies"].([]interface{})
	if pl[0].(string) != "00-勿选-流量:1.0/不限GB 重置:7天" || pl[1].(string) != "Auto" {
		t.Errorf("Proxy 组前两项应为 提示节点、Auto,实际 %v", pl[:2])
	}
}

func findProxy(proxies []interface{}, name string) map[string]interface{} {
	for _, p := range proxies {
		m := p.(map[string]interface{})
		if m["name"] == name {
			return m
		}
	}
	return map[string]interface{}{}
}

func findGroup(groups []interface{}, name string) map[string]interface{} {
	for _, g := range groups {
		m := g.(map[string]interface{})
		if m["name"] == name {
			return m
		}
	}
	return map[string]interface{}{}
}

func TestNoticeText(t *testing.T) {
	u := model.User{Up: 1 << 30, Down: 1 << 30, Volume: 0} // 2GB used, unlimited
	got := noticeText(u)
	if !strings.Contains(got, "2.0/不限GB") {
		t.Errorf("提示文本应含 2.0/不限GB,实际 %q", got)
	}
}
