package render

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 纯 IPv6 机器:默认直连的线路把 1.1.1.1 这类公共 DNS 换成同一家的 IPv6 地址;经 WARP 的线路不动;
// 有 IPv4 的机器(包括双栈)一条都不加。配置要能过 sing-box 干跑。
func TestV6DNSOverrideOnlyOnPureIPv6DirectLines(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Line{Name: "direct", Protocol: "shadowsocks", Port: 30011, Enabled: true, Options: []byte(`{"method":"aes-256-gcm","password":"x"}`)})
	db.Create(&model.Upstream{Name: "warp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`)})
	db.Create(&model.Line{Name: "via-warp", Protocol: "shadowsocks", Port: 30012, Enabled: true, UpstreamId: 1, Options: []byte(`{"method":"aes-256-gcm","password":"y"}`)})

	overrides := func() map[string]string {
		raw, err := BuildConfig(db, NodeCert{})
		if err != nil {
			t.Fatal(err)
		}
		if err := core.ValidateConfig(raw); err != nil {
			t.Fatalf("配置应通过 sing-box 干跑: %v\n%s", err, raw)
		}
		out := map[string]string{}
		for _, r := range rulesOf(t, raw) {
			if r["action"] != "route-options" {
				continue
			}
			in, _ := r["inbound"].([]interface{})
			if len(in) != 1 || in[0] != "direct" {
				t.Fatalf("只该作用于默认直连的线路: %v", r)
			}
			cidr := r["ip_cidr"].([]interface{})[0].(string)
			out[cidr] = r["override_address"].(string)
		}
		return out
	}
	for _, pub := range []string{"", "203.0.113.1"} { // 没探测到、有 IPv4(双栈时 publicIp 也是 IPv4)
		db.Where("key = ?", "publicIp").Delete(&model.Setting{})
		if pub != "" {
			db.Create(&model.Setting{Key: "publicIp", Value: pub})
		}
		if o := overrides(); len(o) != 0 {
			t.Fatalf("publicIp=%q 时不该改 DNS 目标: %v", pub, o)
		}
	}
	db.Model(&model.Setting{}).Where("key = ?", "publicIp").Update("value", "2001:db8::1")
	o := overrides()
	if o["1.1.1.1/32"] != "2606:4700:4700::1111" || o["8.8.8.8/32"] != "2001:4860:4860::8888" || o["223.5.5.5/32"] != "2400:3200::1" || len(o) != len(v6DNS) {
		t.Fatalf("纯 IPv6 时应把常用公共 DNS 换成 IPv6: %v", o)
	}
	for _, p := range v6DNS {
		if a, b := net.ParseIP(p[0]), net.ParseIP(p[1]); a == nil || a.To4() == nil || b == nil || b.To4() != nil {
			t.Fatalf("对照表写错了: %v", p)
		}
	}
}

// 改址只作用于最后走默认直连出口的流量:用户写了"1.1.1.1 走 warp"的分流规则排在改址前面、照旧生效;
// 有 IPv4 的机器规则顺序与以前一致(每条线路的分流规则紧跟它自己的默认出口)。
func TestV6DNSOverrideRespectsRouteRules(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Upstream{Name: "warp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`)})
	db.Create(&model.Line{Name: "a", Protocol: "shadowsocks", Port: 30011, Enabled: true, Options: []byte(`{"method":"aes-256-gcm","password":"x"}`),
		RouteRules: []byte(`[{"type":"ip_cidr","values":["1.1.1.1"],"to":1}]`)})
	db.Create(&model.Line{Name: "b", Protocol: "shadowsocks", Port: 30012, Enabled: true, Options: []byte(`{"method":"aes-256-gcm","password":"y"}`)})

	order := func() (split, firstOverride, defA, defB, lastIdx int) {
		raw, err := BuildConfig(db, NodeCert{})
		if err != nil {
			t.Fatal(err)
		}
		if err := core.ValidateConfig(raw); err != nil {
			t.Fatalf("配置应通过 sing-box 干跑: %v", err)
		}
		split, firstOverride, defA, defB = -1, -1, -1, -1
		rules := rulesOf(t, raw)
		for i, r := range rules {
			in, _ := r["inbound"].([]interface{})
			switch {
			case r["action"] == "route-options" && firstOverride < 0:
				firstOverride = i
			case r["action"] == "route" && r["outbound"] == "warp" && len(in) == 1 && in[0] == "a":
				split = i
			case r["action"] == "route" && r["outbound"] == "direct" && len(in) == 1 && in[0] == "a" && r["ip_cidr"] == nil:
				defA = i
			case r["action"] == "route" && r["outbound"] == "direct" && len(in) == 1 && in[0] == "b":
				defB = i
			}
		}
		return split, firstOverride, defA, defB, len(rules)
	}
	db.Create(&model.Setting{Key: "publicIp", Value: "203.0.113.1"})
	split, ov, defA, defB, _ := order()
	if ov != -1 || !(split < defA && defA < defB) {
		t.Fatalf("有 IPv4:不改址、顺序照旧(a 的分流 %d < a 默认 %d < b 默认 %d),改址 %d", split, defA, defB, ov)
	}
	db.Model(&model.Setting{}).Where("key = ?", "publicIp").Update("value", "2001:db8::1")
	split, ov, defA, defB, _ = order()
	if !(split >= 0 && split < ov && ov < defA && ov < defB) {
		t.Fatalf("纯 IPv6:分流规则 %d 应在改址 %d 前,改址应在各线路默认出口 %d / %d 前", split, ov, defA, defB)
	}
}
