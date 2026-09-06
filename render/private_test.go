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
