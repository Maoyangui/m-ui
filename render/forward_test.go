package render

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 中转入口:本机是中转服务器的,开 direct 入站转给出口服务器上的线路;规则排在最前面(不嗅探、不受私网屏蔽);
// 出口不在 / 停用 / 不再部署这条线路、暂停的、经别的服务器的、和本机线路撞端口的不开;sing-box 认这份配置。
func TestForwardInbounds(t *testing.T) {
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
	self := model.Node{Name: "广州中转", IsLocal: true, Enabled: true, PublicIP: "198.51.100.1", Sort: 1}
	tokyo := model.Node{Name: "东京", ApiUrl: "https://tokyo.test", Enabled: true, PublicIP: "203.0.113.2", PublicIP6: "2001:db8::2", Sort: 2}
	v6only := model.Node{Name: "纯六", ApiUrl: "https://v6.test", Enabled: true, PublicIP6: "2001:db8::3", Sort: 3}
	off := model.Node{Name: "停用的", ApiUrl: "https://off.test", Enabled: true, PublicIP: "203.0.113.4", Sort: 4}
	other := model.Node{Name: "别的中转", ApiUrl: "https://other.test", Enabled: true, PublicIP: "203.0.113.5", Sort: 5}
	for _, n := range []*model.Node{&self, &tokyo, &v6only, &off, &other} {
		if err := db.Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	db.Model(&model.Node{}).Where("id = ?", off.Id).Update("enabled", false)
	db.Create(&model.Setting{Key: "allowPrivate", Value: "false"}) // 私网屏蔽开着:中转规则得排在它前面
	defer func(old func(context.Context, string) ([]net.IP, error)) { lookupIP = old }(lookupIP)
	lookupIP = func(context.Context, string) ([]net.IP, error) { return nil, errors.New("不解析") }

	priv, pub, err := creds.RealityKeypair()
	if err != nil {
		t.Fatal(err)
	}
	reality := raw(map[string]interface{}{"mode": "reality", "reality": map[string]interface{}{
		"private_key": priv, "public_key": pub, "short_ids": []string{creds.ShortID()}, "handshake_server": "www.microsoft.com", "handshake_port": 443}})
	ids := func(v ...uint) json.RawMessage { return raw(v) }
	fw := func(v ...model.LineForward) json.RawMessage { return raw(v) }
	lines := []model.Line{
		// 只部署在东京:经本机转 UDP
		{Name: "hy2", Protocol: "hysteria2", Port: 443, NodeIds: ids(tokyo.Id), Forwards: fw(
			model.LineForward{Via: self.Id, Port: 30001, To: tokyo.Id},
			model.LineForward{Via: self.Id, Port: 30002, To: tokyo.Id, Off: true},          // 暂停
			model.LineForward{Via: other.Id, Port: 30003, To: tokyo.Id},                    // 经别的服务器
			model.LineForward{Via: self.Id, Port: 30004, To: off.Id},                       // 出口不在部署范围(还停用了)
			model.LineForward{Via: self.Id, Port: 30005, To: tokyo.Id, Target: "10.0.0.2"}, // 手填转到地址(内网)
			model.LineForward{Via: self.Id, Port: 20443, To: tokyo.Id},                     // 和本机线路撞端口
			model.LineForward{Via: 0, Host: "hk.relay.example", Port: 31000, To: tokyo.Id}, // 外部中转:不渲染
		)},
		{Name: "vless", Protocol: "vless", Port: 8443, Tls: reality, NodeIds: ids(tokyo.Id, v6only.Id), Forwards: fw(
			model.LineForward{Via: self.Id, Port: 30010, To: v6only.Id}, // 出口只有 IPv6
		)},
		{Name: "ss", Protocol: "shadowsocks", Port: 9388, NodeIds: ids(tokyo.Id), Options: raw(map[string]interface{}{"method": "aes-256-gcm", "password": creds.Base64Key(32)}),
			Forwards: fw(model.LineForward{Via: self.Id, Port: 30020, To: tokyo.Id})},
		// 部署在本机的线路(占着 20443)
		{Name: "local", Protocol: "trojan", Port: 20443, NodeIds: ids(self.Id)},
	}
	for i := range lines {
		lines[i].Enabled, lines[i].Sort = true, i+1
		if err := db.Create(&lines[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	u := model.User{Name: "alice", Enabled: true, Credentials: raw(creds.Generate("alice"))}
	db.Create(&u)
	db.Create(&model.UserLine{UserId: u.Id, LineId: lines[3].Id})

	cfg, err := BuildConfig(db, NodeCert{ServerName: "relay.test", CertPath: crt, KeyPath: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.ValidateConfig(cfg); err != nil {
		t.Fatalf("sing-box 拒绝中转入站: %v\n%s", err, cfg)
	}
	var parsed struct {
		Inbounds []map[string]interface{} `json:"inbounds"`
		Route    struct {
			Rules []map[string]interface{} `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatal(err)
	}
	byTag := map[string]map[string]interface{}{}
	for _, in := range parsed.Inbounds {
		byTag[in["tag"].(string)] = in
	}
	want := map[string][3]interface{}{ // tag → 转到地址、端口、network
		"hy2 ⇄30001":   {"203.0.113.2", float64(443), "udp"},
		"hy2 ⇄30005":   {"10.0.0.2", float64(443), "udp"},
		"vless ⇄30010": {"2001:db8::3", float64(8443), "tcp"},
		"ss ⇄30020":    {"203.0.113.2", float64(9388), nil},
	}
	for tag, w := range want {
		in, ok := byTag[tag]
		if !ok {
			t.Fatalf("缺中转入站 %s;有:%v", tag, keysOf(byTag))
		}
		if in["type"] != "direct" || in["override_address"] != w[0] || in["override_port"] != w[1] || in["network"] != w[2] {
			t.Fatalf("%s 不对: %v", tag, in)
		}
	}
	for tag := range byTag {
		if strings.Contains(tag, "⇄") {
			if _, ok := want[tag]; !ok {
				t.Fatalf("不该开的中转入站 %s 开了", tag)
			}
		}
	}
	if _, ok := byTag["local"]; !ok {
		t.Fatal("本机线路照常渲染")
	}
	if _, ok := byTag["hy2"]; ok {
		t.Fatal("只部署在东京的线路不该在中转服务器上开")
	}
	first := parsed.Route.Rules[0]
	tags, _ := first["inbound"].([]interface{})
	if first["action"] != "route" || first["outbound"] != "direct" || len(tags) != len(want) {
		t.Fatalf("中转规则要排第一条(在嗅探、私网屏蔽之前): %v", first)
	}
	if parsed.Route.Rules[1]["action"] != "sniff" {
		t.Fatalf("第二条才是嗅探: %v", parsed.Route.Rules[1])
	}

	// 摘要:出口换了 IP 要变(runner 据此重新渲染)
	sig := ForwardSig(db)
	if !strings.Contains(sig, "hy2 ⇄30001=203.0.113.2:443") {
		t.Fatalf("摘要应带转到地址: %s", sig)
	}
	db.Model(&model.Node{}).Where("id = ?", tokyo.Id).Update("public_ip", "203.0.113.22")
	if s2 := ForwardSig(db); s2 == sig || !strings.Contains(s2, "203.0.113.22") {
		t.Fatalf("出口换了 IP 摘要应变: %s", s2)
	}
}

// 没有中转入口时配置和以前逐字节一样:第一条规则还是嗅探,摘要为空(runner 不会因此重载)。
func TestNoForwardsKeepsConfig(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true})
	db.Create(&model.Line{Name: "socks", Protocol: "socks", Port: 1080, Enabled: true})
	cfg, err := BuildConfig(db, NodeCert{})
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Inbounds []map[string]interface{} `json:"inbounds"`
		Route    struct {
			Rules []map[string]interface{} `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Route.Rules[0]["action"] != "sniff" {
		t.Fatalf("没有中转入口时第一条规则应是嗅探: %v", parsed.Route.Rules[0])
	}
	for _, in := range parsed.Inbounds {
		if in["type"] == "direct" {
			t.Fatalf("没有中转入口时不该有 direct 入站: %v", in)
		}
	}
	if s := ForwardSig(db); s != "" {
		t.Fatalf("没有中转入口时摘要应为空: %q", s)
	}
}

// 转哪种流量、转到哪个地址的选择。
func TestForwardNetworkAndTarget(t *testing.T) {
	cases := map[string]string{"hysteria2": "udp", "tuic": "udp", "shadowsocks": "", "vless": "tcp", "trojan": "tcp", "anytls": "tcp", "socks": "tcp"}
	for proto, want := range cases {
		if got := ForwardNetwork(model.Line{Protocol: proto}); got != want {
			t.Fatalf("%s 应转 %q,实际 %q", proto, want, got)
		}
	}
	if got := ForwardNetwork(model.Line{Protocol: "vless", Transport: raw(map[string]string{"type": "quic"})}); got != "udp" {
		t.Fatalf("QUIC 传输应转 UDP,实际 %q", got)
	}
	exit := model.Node{PublicIP: "203.0.113.2", PublicIP6: "2001:db8::2", Addr: "198.51.100.9", Domain: "jp.example"}
	if got := ForwardTarget(model.LineForward{}, exit, model.Node{PublicIP: "198.51.100.1"}); got != "203.0.113.2" {
		t.Fatalf("有 IPv4 的中转用出口的 IPv4,实际 %q", got)
	}
	if got := ForwardTarget(model.LineForward{}, exit, model.Node{PublicIP6: "2001:db8::1"}); got != "2001:db8::2" {
		t.Fatalf("只有 IPv6 的中转用出口的 IPv6,实际 %q", got)
	}
	if got := ForwardTarget(model.LineForward{Target: "[fd00::2]"}, exit, model.Node{}); got != "fd00::2" {
		t.Fatalf("手填的转到地址优先(去方括号),实际 %q", got)
	}
	if got := ForwardTarget(model.LineForward{}, model.Node{Addr: "198.51.100.9", Domain: "jp.example"}, model.Node{}); got != "198.51.100.9" {
		t.Fatalf("出口没探测到公网 IP 时用连接地址,实际 %q", got)
	}
	if got := ForwardTarget(model.LineForward{}, model.Node{Domain: "jp.example"}, model.Node{}); got != "jp.example" {
		t.Fatalf("再没有就用域名,实际 %q", got)
	}
}

func keysOf(m map[string]map[string]interface{}) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
