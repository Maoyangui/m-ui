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

// 协议 × 加密方式:哪些配得上。期望值照着实机跑出来的结果写(tests/rs/t60-combos.mjs),不是照着表抄。
func TestCheckCombo(t *testing.T) {
	ok := map[string]string{ // 协议 → 配得上的方式(c = 证书、r = Reality、n = 不加密)
		"hysteria2": "c", "tuic": "c", "anytls": "c",
		"trojan": "crn", "vless": "crn", "vmess": "cn", "http": "cn",
		"shadowsocks": "n", "socks": "n", "mixed": "n",
	}
	if len(ok) != len(Protocols) {
		t.Fatalf("Protocols 里有 %d 个协议,这里只写了 %d 个:新协议要补上它配得上哪几种", len(Protocols), len(ok))
	}
	short := map[string]string{"cert": "c", "reality": "r", "none": "n"}
	for proto, spec := range Protocols {
		for _, mode := range []string{"cert", "reality", "none"} {
			line := model.Line{Name: "x", Protocol: proto, Tls: raw(map[string]string{"mode": mode})}
			err := CheckCombo(line)
			want := strings.Contains(ok[proto], short[mode])
			if (err == nil) != want {
				t.Errorf("%s + %s:应该%s,CheckCombo = %v", proto, mode, map[bool]string{true: "放行", false: "拦下"}[want], err)
			}
			if err != nil && !strings.Contains(err.Error(), protoLabel[proto]) && !strings.Contains(err.Error(), proto) {
				t.Errorf("%s + %s 的报错没点名协议: %v", proto, mode, err)
			}
		}
		// 没写加密方式 = 用这个协议的默认方式:默认方式一定配得上
		if err := CheckCombo(model.Line{Name: "x", Protocol: proto}); err != nil {
			t.Errorf("%s 的默认加密方式 %s 自己就配不上: %v", proto, spec.TLSDefault, err)
		}
		if modes := TLSModes(proto); len(modes) == 0 || !tlsModeAllowed(proto, spec.TLSDefault) {
			t.Errorf("%s:能选的方式 %v 里没有默认的 %s", proto, modes, spec.TLSDefault)
		}
	}
	// 报错要说清楚改成什么
	for _, tc := range []struct{ proto, mode, want string }{
		{"hysteria2", "reality", "QUIC"}, {"tuic", "reality", "QUIC"},
		{"anytls", "reality", "「证书」"}, {"vmess", "reality", "「证书」或「不加密」"}, {"http", "reality", "「证书」或「不加密」"},
		{"mixed", "cert", "只能是「不加密」"}, {"socks", "reality", "只能是「不加密」"}, {"shadowsocks", "cert", "只能是「不加密」"},
		{"anytls", "none", "必须启用 TLS"},
	} {
		err := CheckCombo(model.Line{Protocol: tc.proto, Tls: raw(map[string]string{"mode": tc.mode})})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s + %s 的报错里应有 %q: %v", tc.proto, tc.mode, tc.want, err)
		}
	}
	// 不认识的协议、不认识的加密方式不归这里管
	if err := CheckCombo(model.Line{Protocol: "wireguard", Tls: raw(map[string]string{"mode": "reality"})}); err != nil {
		t.Errorf("不认识的协议应留给别处报错: %v", err)
	}
	if err := CheckCombo(model.Line{Protocol: "vless", Tls: raw(map[string]string{"mode": "weird"})}); err != nil {
		t.Errorf("不认识的加密方式应留给别处报错: %v", err)
	}
	// 只有走 QUIC 的配 Reality 算「起不来」;别的配不上的入站起得来,不能在渲染时摘掉
	for proto := range Protocols {
		for _, mode := range []string{"cert", "reality", "none"} {
			got := Unstartable(model.Line{Protocol: proto, Tls: raw(map[string]string{"mode": mode})}) != ""
			if want := (proto == "hysteria2" || proto == "tuic") && mode == "reality"; got != want {
				t.Errorf("Unstartable(%s + %s) = %v,应为 %v", proto, mode, got, want)
			}
		}
	}
}

func TestSSMethodProblem(t *testing.T) {
	for _, m := range []string{"2022-blake3-chacha20-poly1305", "none", "NONE", " plain "} {
		if SSMethodProblem(m) == nil {
			t.Errorf("%q 在多用户入站上起不来,应拦下", m)
		}
	}
	for _, m := range []string{"aes-256-gcm", "aes-128-gcm", "chacha20-ietf-poly1305", "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "xchacha20-ietf-poly1305"} {
		if err := SSMethodProblem(m); err != nil {
			t.Errorf("%q 能用,不该拦: %v", m, err)
		}
	}
}

// 库里已经有一条起不来的线路(Hysteria2 / TUIC 配 Reality,以前存得下):渲染时跳过它,别的线路照常、整份配置过得了内核校验;
// 它的入站、路由规则都不出。对照:不跳过的话这份配置里就带着那条入站(干跑能过、启动失败,见 core.TestQUICRealityFailsOnlyAtStart)。
// 入站起得来、只是订阅给不出节点的那几种(AnyTLS / VMess 配 Reality)不能摘:有人手写客户端配置在用。
func TestUnstartableLineSkippedAtRender(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "c.crt"), filepath.Join(dir, "c.key")
	if err := certutil.GenerateSelfSigned([]string{"hk.test"}, crt, key, 7); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	self := model.Node{Name: "香港", IsLocal: true, Enabled: true, PublicIP: "198.51.100.1", Sort: 1}
	db.Create(&self)
	priv, _, err := creds.RealityKeypair()
	if err != nil {
		t.Fatal(err)
	}
	reality := raw(map[string]interface{}{"mode": "reality", "reality": map[string]interface{}{
		"private_key": priv, "short_ids": []string{"0123456789abcdef"}, "handshake_server": "www.microsoft.com", "handshake_port": 443}})
	lines := []model.Line{
		{Name: "好的", Protocol: "trojan", Port: 20443},
		{Name: "hy2坏", Protocol: "hysteria2", Port: 20444, Tls: reality},
		{Name: "tuic坏", Protocol: "tuic", Port: 20445, Tls: reality},
		{Name: "anytls真", Protocol: "anytls", Port: 20446, Tls: reality},
		{Name: "vmess真", Protocol: "vmess", Port: 20447, Tls: reality},
		{Name: "hy2好", Protocol: "hysteria2", Port: 20448},
	}
	u := model.User{Name: "alice", Enabled: true, Credentials: raw(creds.Generate("alice"))}
	db.Create(&u)
	for i := range lines {
		lines[i].Enabled, lines[i].Sort = true, i+1
		if err := db.Create(&lines[i]).Error; err != nil {
			t.Fatal(err)
		}
		db.Create(&model.UserLine{UserId: u.Id, LineId: lines[i].Id})
	}
	cfg, err := BuildConfig(db, NodeCert{ServerName: "hk.test", CertPath: crt, KeyPath: key})
	if err != nil {
		t.Fatalf("有一条起不来的线路时,整份配置应照样渲染出来: %v", err)
	}
	if err := core.ValidateConfig(cfg); err != nil {
		t.Fatalf("渲染出来的配置内核应认: %v", err)
	}
	var parsed struct {
		Inbounds []struct {
			Tag string `json:"tag"`
		} `json:"inbounds"`
		Route struct {
			Rules []json.RawMessage `json:"rules"`
		} `json:"route"`
	}
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, ib := range parsed.Inbounds {
		tags = append(tags, ib.Tag)
	}
	if got, want := strings.Join(tags, ","), "好的,anytls真,vmess真,hy2好"; got != want {
		t.Fatalf("入站应是 %s(走 QUIC 配 Reality 的两条摘掉,别的原样),实际 %s", want, got)
	}
	for _, r := range parsed.Route.Rules {
		if strings.Contains(string(r), "hy2坏") || strings.Contains(string(r), "tuic坏") {
			t.Fatalf("摘掉的线路不该留着路由规则: %s", r)
		}
	}
	// 停用的坏线路本来就不渲染;改好之后(换成证书)照常回来
	db.Model(&model.Line{}).Where("name = ?", "hy2坏").Update("tls", raw(map[string]string{"mode": "cert"}))
	cfg, err = BuildConfig(db, NodeCert{ServerName: "hk.test", CertPath: crt, KeyPath: key})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), `"tag": "hy2坏"`) {
		t.Fatal("加密方式改成证书后,这条线路应回到配置里")
	}
}
