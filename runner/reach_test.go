package runner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 大陆连通检测测哪个地址、哪个端口:手填的 IP 优先,其次上报的公网 IP;
// 端口优先部署在这台上的第一条 TCP 线路,只有 UDP 线路时用订阅端口(本机)或面板 API 端口(副机);停用的服务器不测。
func TestReachTargets(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.db)
	db := r.db
	// 本机的连接地址填 127.0.0.1(不做入站的主机就是这么配的):要跳过它,用探测到的公网 IP
	db.Model(&model.Node{}).Where("is_local = ?", true).Updates(map[string]interface{}{"name": "香港1", "addr": "127.0.0.1", "public_ip": "203.0.113.1"})
	r.setSetting("subPort", "2096")
	a := model.Node{Name: "台湾", ApiUrl: "https://tw.example.com:2053/app/", Addr: "198.51.100.2", PublicIP: "198.51.100.99", Enabled: true, Sort: 2}
	b := model.Node{Name: "日本", ApiUrl: "https://jp.example.com/app/", PublicIP: "198.51.100.3", Enabled: true, Sort: 3}
	c := model.Node{Name: "停用", PublicIP: "198.51.100.4", Enabled: true, Sort: 4}
	d := model.Node{Name: "没地址", Addr: "10.0.0.5", PublicIP: "192.168.1.9", Enabled: true, Sort: 5}
	db.Create(&a)
	db.Create(&b)
	db.Create(&c)
	db.Create(&d)
	db.Model(&model.Node{}).Where("id = ?", c.Id).Update("enabled", false)
	ids := func(v ...uint) json.RawMessage { raw, _ := json.Marshal(v); return raw }
	// 台湾:先是一条 UDP 线路,再是一条 TCP 线路(应当选 TCP 那条);日本只有 UDP;本机什么都没部署
	db.Create(&model.Line{Name: "hy2", Protocol: "hysteria2", Port: 8443, Enabled: true, Sort: 1, NodeIds: ids(a.Id, b.Id)})
	db.Create(&model.Line{Name: "anytls-a", Protocol: "anytls", Port: 443, Enabled: true, Sort: 2, NodeIds: ids(a.Id)})
	off := model.Line{Name: "停用的 vless", Protocol: "vless", Port: 9443, Sort: 0, NodeIds: ids(b.Id)}
	db.Create(&off)
	db.Model(&model.Line{}).Where("id = ?", off.Id).Update("enabled", false) // Enabled 带 default:true,建的时候写 false 不生效

	got := map[string]struct {
		host, from, line string
		port             int
	}{}
	for _, tg := range r.reachTargets(context.Background()) {
		got[tg.Name] = struct {
			host, from, line string
			port             int
		}{tg.Host, tg.PortFrom, tg.PortLine, tg.Port}
	}
	if _, ok := got["停用"]; ok || len(got) != 4 {
		t.Fatalf("目标 = %+v", got)
	}
	if g := got["没地址"]; g.host != "" {
		t.Fatalf("内网地址不该拿去测: %+v", g)
	}
	if g := got["香港1"]; g.host != "203.0.113.1" || g.port != 2096 || g.from != "sub" {
		t.Fatalf("本机 = %+v", g)
	}
	if g := got["台湾"]; g.host != "198.51.100.2" || g.port != 443 || g.from != "line" || g.line != "anytls-a" {
		t.Fatalf("台湾 = %+v(手填 IP 优先于上报的;TCP 线路优先于 UDP)", g)
	}
	if g := got["日本"]; g.host != "198.51.100.3" || g.port != 443 || g.from != "api" {
		t.Fatalf("日本 = %+v(只有 UDP 线路时用 API 端口,https 没写端口按 443)", g)
	}
}
