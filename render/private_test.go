package render

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func rulesOf(t *testing.T, raw []byte) []map[string]interface{} {
	t.Helper()
	var cfg struct {
		Route struct {
			Rules []map[string]interface{} `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Route.Rules
}

func TestPrivateBlockDefaultOnAndSwitchable(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Line{Name: "ss", Protocol: "shadowsocks", Port: 30011, Enabled: true, Options: []byte(`{"method":"aes-256-gcm","password":"x"}`)})
	db.Create(&model.Upstream{Name: "warp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`)})
	db.Create(&model.Line{Name: "via-warp", Protocol: "shadowsocks", Port: 30012, Enabled: true, UpstreamId: 1, Options: []byte(`{"method":"aes-256-gcm","password":"y"}`)})

	raw, err := BuildConfig(db, NodeCert{})
	if err != nil {
		t.Fatal(err)
	}
	found, resolveOn := false, []interface{}(nil)
	for _, r := range rulesOf(t, raw) {
		if r["ip_is_private"] == true && r["action"] == "reject" {
			found = true
		}
		if r["action"] == "resolve" {
			resolveOn, _ = r["inbound"].([]interface{})
		}
	}
	if !found {
		t.Fatalf("默认必须有私网屏蔽规则: %s", raw)
	}
	// 域名解析只对直连出口的线路做:解析失败会拒绝连接,经上游出去的线路不该多这个失败点
	if len(resolveOn) != 1 || resolveOn[0] != "ss" {
		t.Fatalf("resolve 应只作用于直连线路 ss,实际 %v: %s", resolveOn, raw)
	}
	if err := core.ValidateConfig(raw); err != nil {
		t.Fatalf("带私网屏蔽的配置应通过 sing-box 干跑: %v", err)
	}

	db.Create(&model.Setting{Key: "allowPrivate", Value: "true"})
	raw, _ = BuildConfig(db, NodeCert{})
	for _, r := range rulesOf(t, raw) {
		if r["ip_is_private"] == true || r["action"] == "resolve" {
			t.Fatalf("allowPrivate=true 时不该有私网规则: %s", raw)
		}
	}
}

// 「允许访问内网」关着时,私网屏蔽同时屏蔽这台服务器自己的公网地址(全部端口):不然用户经代理连本机公网 IP,
// 面板、代理面板、订阅、副机接口端口照样摸得到,管理员按来源 IP 限制面板访问(或云防火墙只放行自己的 IP)就被绕过了 ——
// 经代理过去的连接来源是这台服务器自己。地址取探测到的公网 IPv4 / IPv6,加上网卡上的公网地址(多 IP、IPv6 多地址);
// 打开「允许访问内网」时和以前一样不加。
func TestOwnPublicIPBlocked(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	old := localAddrs
	defer func() { localAddrs = old }()
	localAddrs = func() []net.IP {
		return []net.IP{net.ParseIP("203.0.113.10"), net.ParseIP("10.0.0.5"), net.ParseIP("127.0.0.1"), net.ParseIP("fe80::1"),
			net.ParseIP("2001:db8::abcd"), net.ParseIP("198.51.100.7")}
	}
	db.Create(&model.Line{Name: "ss", Protocol: "shadowsocks", Port: 30011, Enabled: true, Options: []byte(`{"method":"aes-256-gcm","password":"x"}`)})
	db.Create(&model.Setting{Key: "publicIp", Value: "198.51.100.7"})
	db.Create(&model.Setting{Key: "publicIp6", Value: "2001:db8::1"})

	own := func(raw []byte) (cidrs []string, after bool) {
		rules := rulesOf(t, raw)
		for i, r := range rules {
			list, ok := r["ip_cidr"].([]interface{})
			if !ok || r["action"] != "reject" || r["inbound"] != nil {
				continue
			}
			for _, c := range list {
				cidrs = append(cidrs, c.(string))
			}
			after = i > 0 && rules[i-1]["ip_is_private"] == true
		}
		return
	}
	raw, err := BuildConfig(db, NodeCert{})
	if err != nil {
		t.Fatal(err)
	}
	cidrs, after := own(raw)
	want := []string{"198.51.100.7/32", "2001:db8::1/128", "203.0.113.10/32", "2001:db8::abcd/128"}
	if strings.Join(cidrs, ",") != strings.Join(want, ",") || !after {
		t.Fatalf("应紧跟私网屏蔽、屏蔽本机公网地址 %v(去重、不含内网 / 回环 / 链路本地),实际 %v after=%v: %s", want, cidrs, after, raw)
	}
	if err := core.ValidateConfig(raw); err != nil {
		t.Fatalf("配置应通过 sing-box 干跑: %v", err)
	}

	// 打开「允许访问内网」:和以前一样什么都不拦
	db.Create(&model.Setting{Key: "allowPrivate", Value: "true"})
	raw, _ = BuildConfig(db, NodeCert{})
	if cidrs, _ := own(raw); len(cidrs) != 0 {
		t.Fatalf("allowPrivate=true 时不该屏蔽本机地址: %s", raw)
	}

	// 一个公网地址都没有(还没探测到、网卡上只有内网地址):只有私网屏蔽
	db.Model(&model.Setting{}).Where("key = ?", "allowPrivate").Update("value", "false")
	db.Where("key IN ?", []string{"publicIp", "publicIp6"}).Delete(&model.Setting{})
	localAddrs = func() []net.IP { return []net.IP{net.ParseIP("10.0.0.5")} }
	raw, _ = BuildConfig(db, NodeCert{})
	if cidrs, _ := own(raw); len(cidrs) != 0 {
		t.Fatalf("没有公网地址时不该有这条规则: %s", raw)
	}
}
