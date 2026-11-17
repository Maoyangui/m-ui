package render

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 本机的流量额度用完 / 到期、被停止服务(model.Node.Suspended):渲染出来的数据面一个用户都不放行 ——
// 线路入站还在(只剩占位用户,谁也连不上),别的服务器把本机线路当上游用的专用用户也不给,经本机的中转入口一个不开;
// sing-box 认这份配置。恢复后原样回来。出口被停止服务时,转给它的中转入口也不开。
func TestHeldSelfServesNobody(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "c.crt"), filepath.Join(dir, "c.key")
	if err := certutil.GenerateSelfSigned([]string{"relay.test"}, crt, key, 7); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	self := model.Node{Name: "按量机", IsLocal: true, Enabled: true, PublicIP: "198.51.100.1", Sort: 1}
	tokyo := model.Node{Name: "东京", ApiUrl: "https://tokyo.test", Enabled: true, PublicIP: "203.0.113.2", Sort: 2}
	for _, n := range []*model.Node{&self, &tokyo} {
		if err := db.Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	lines := []model.Line{
		{Name: "local", Protocol: "trojan", Port: 20443, NodeIds: raw([]uint{self.Id})},
		{Name: "hy2", Protocol: "hysteria2", Port: 443, NodeIds: raw([]uint{tokyo.Id}), Forwards: raw([]model.LineForward{{Id: 1, Via: self.Id, Port: 30001, To: tokyo.Id}})},
	}
	for i := range lines {
		lines[i].Enabled, lines[i].Sort = true, i+1
		if err := db.Create(&lines[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	u := model.User{Name: "alice", Enabled: true, Credentials: raw(creds.Generate("alice"))}
	db.Create(&u)
	db.Create(&model.UserLine{UserId: u.Id, LineId: lines[0].Id})
	// 别的服务器把本机的 local 线路当上游:本机入站上有它的专用用户
	relayCreds := raw(creds.Generate(model.RelayUserName(1)))
	db.Create(&model.Upstream{Name: "relay", Type: "line", FromLine: lines[0].Id, FromNode: self.Id, RelayCreds: relayCreds, Options: raw(map[string]interface{}{})})

	build := func() (tags []string, users string) {
		t.Helper()
		cfg, err := BuildConfig(db, NodeCert{ServerName: "relay.test", CertPath: crt, KeyPath: key})
		if err != nil {
			t.Fatal(err)
		}
		if err := core.ValidateConfig(cfg); err != nil {
			t.Fatalf("sing-box 不认这份配置: %v\n%s", err, cfg)
		}
		var parsed struct {
			Inbounds []map[string]interface{} `json:"inbounds"`
		}
		if err := json.Unmarshal(cfg, &parsed); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, in := range parsed.Inbounds {
			tags = append(tags, in["tag"].(string))
			list, _ := in["users"].([]interface{})
			for _, x := range list {
				if m, ok := x.(map[string]interface{}); ok {
					names = append(names, m["name"].(string))
				}
			}
		}
		return tags, strings.Join(names, ",")
	}
	has := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	hold := func(id uint, on bool) {
		t.Helper()
		if err := db.Model(&model.Node{}).Where("id = ?", id).Update("suspended", on).Error; err != nil {
			t.Fatal(err)
		}
	}
	tags, users := build()
	if !has(tags, "local") || !has(tags, "hy2 ⇄30001") || !strings.Contains(users, "alice") || !strings.Contains(users, model.RelayUserName(1)) {
		t.Fatalf("前提:本机线路有 alice 和上游专用用户,中转入口开着: 入站 %v 用户 %s", tags, users)
	}
	sig := ForwardSig(db)
	if sig == "" {
		t.Fatal("前提:有中转入口,摘要不为空")
	}

	hold(self.Id, true)
	if !SelfHeld(db, self.Id) || SelfHeld(db, tokyo.Id) || SelfHeld(db, 0) || SelfHeld(db, 9999) {
		t.Fatal("SelfHeld 判错了")
	}
	tags, users = build()
	if !has(tags, "local") {
		t.Fatalf("线路入站照常在(只是没有用户): %v", tags)
	}
	if strings.Contains(users, "alice") || strings.Contains(users, "relay") {
		t.Fatalf("被停止服务:不该下发任何真实用户、上游专用用户,实际 %s", users)
	}
	for _, tag := range tags {
		if IsForwardTag(tag) {
			t.Fatalf("被停止服务:经本机的中转入口不该开,却有 %s", tag)
		}
	}
	if s2 := ForwardSig(db); s2 == sig {
		t.Fatal("中转入口没了,摘要应变(runner 据此重新渲染)")
	}

	hold(self.Id, false)
	tags, users = build()
	if !has(tags, "hy2 ⇄30001") || !strings.Contains(users, "alice") || ForwardSig(db) != sig {
		t.Fatalf("恢复后应原样回来: 入站 %v 用户 %s", tags, users)
	}

	hold(tokyo.Id, true) // 出口被停止服务:转过去也没人接
	tags, users = build()
	if has(tags, "hy2 ⇄30001") || !strings.Contains(users, "alice") {
		t.Fatalf("出口被停止服务:转给它的中转入口不开,本机线路照常: 入站 %v 用户 %s", tags, users)
	}
}
