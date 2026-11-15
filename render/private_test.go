package render

import (
	"encoding/json"
	"path/filepath"
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

// 「允许访问内网」关着时只拦 ip_is_private(回环、内网、链路本地,含云厂商元数据 169.254.169.254),和 0.7.16 一样:
// 本机和集群里别的服务器的公网地址不拦(用户 11-15 定)。开发途中加过「本机公网地址全端口」「集群里别的服务器的面板 / 订阅 /
// 代理面板 / 副机接口端口」两条拦截,会让经代理开面板、走代理的商城 / 机器人调外部 API(和面板同一个端口)、连着代理刷新订阅、
// 登代理面板一律失败;这里守着别再加回去。
func TestOwnAndClusterPublicIPsNotBlocked(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Line{Name: "ss", Protocol: "shadowsocks", Port: 30011, Enabled: true, Options: []byte(`{"method":"aes-256-gcm","password":"x"}`)})
	db.Create(&model.Setting{Key: "publicIp", Value: "198.51.100.7"})
	db.Create(&model.Setting{Key: "publicIp6", Value: "2001:db8::1"})
	ports, _ := json.Marshal([]model.ServicePort{{Label: "面板", Port: 2053}, {Label: "订阅", Port: 2056}, {Label: "代理面板", Port: 2054}})
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, PublicIP: "198.51.100.7", Sort: 1})
	db.Create(&model.Node{Name: "副机1", ApiUrl: "https://203.0.113.9:2053/app/", PublicIP: "203.0.113.9", PublicIP6: "2001:db8::9", ServicePorts: ports, Enabled: true, Sort: 2})
	raw, err := BuildConfig(db, NodeCert{})
	if err != nil {
		t.Fatal(err)
	}
	private := false
	for _, r := range rulesOf(t, raw) {
		if r["ip_is_private"] == true && r["action"] == "reject" {
			private = true
		}
		if _, ok := r["ip_cidr"]; ok && r["action"] == "reject" && r["inbound"] == nil {
			t.Fatalf("本机 / 集群服务器的公网地址不该被拦: %v\n%s", r, raw)
		}
	}
	if !private {
		t.Fatalf("回环、内网、元数据地址照拦: %s", raw)
	}
	if err := core.ValidateConfig(raw); err != nil {
		t.Fatalf("配置应通过 sing-box 干跑: %v", err)
	}
}
