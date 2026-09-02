package sub

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/fangjunsheng555/m-ui/certutil"
	"github.com/fangjunsheng555/m-ui/database"
	"github.com/fangjunsheng555/m-ui/database/model"
)

func TestInsecureSubscriptionForSelfSignedIPServer(t *testing.T) {
	// 纯 IP 入口:无 SNI
	db, _ := database.Open(filepath.Join(t.TempDir(), "x.db"))
	defer database.Close(db)
	db.Create(&model.Node{Name: "HK", Domain: "1.2.3.4", IsLocal: true, Enabled: true})
	e := EntriesFromNodes(db, "")
	if len(e) != 1 || e[0].Host != "1.2.3.4" || e[0].SNI != "" {
		t.Fatalf("IP 入口不应带 SNI: %+v", e)
	}
	e[0].Insecure = true

	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"},"anytls":{"password":"p"},"tuic":{"uuid":"11111111-1111-1111-1111-111111111111","password":"p"},"trojan":{"password":"p"},"vless":{"uuid":"22222222-2222-2222-2222-222222222222"}}`)}
	lines := []model.Line{
		{Name: "hy2", Protocol: "hysteria2", Port: 1, Enabled: true},
		{Name: "any", Protocol: "anytls", Port: 2, Enabled: true},
		{Name: "tuic", Protocol: "tuic", Port: 3, Enabled: true},
		{Name: "trojan", Protocol: "trojan", Port: 4, Enabled: true, Tls: []byte(`{"mode":"cert"}`)},
		{Name: "vless-reality", Protocol: "vless", Port: 5, Enabled: true, Tls: []byte(`{"mode":"reality","reality":{"private_key":"k","public_key":"pk","short_ids":["ab"],"handshake_server":"www.apple.com"}}`)},
	}
	links := GenerateLinks(user, lines, e)
	want := map[string]string{"hysteria2://": "insecure=1", "anytls://": "insecure=1", "tuic://": "allow_insecure=1", "trojan://": "allowInsecure=1"}
	for prefix, param := range want {
		found := false
		for _, l := range links {
			if strings.HasPrefix(l, prefix) {
				found = true
				if !strings.Contains(l, param) {
					t.Fatalf("%s 链接缺少 %s: %s", prefix, param, l)
				}
				if strings.Contains(l, "sni=") {
					t.Fatalf("IP 入口不应带 sni: %s", l)
				}
			}
		}
		if !found {
			t.Fatalf("缺少 %s 链接", prefix)
		}
	}
	for _, l := range links {
		if strings.HasPrefix(l, "vless://") && strings.Contains(l, "allowInsecure") {
			t.Fatal("Reality 线路不应加 allowInsecure(与自签证书无关)")
		}
	}

	out, err := BuildClash(user, lines, e, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "skip-cert-verify: true"); n != 4 {
		t.Fatalf("hy2/anytls/tuic/trojan 应各带 skip-cert-verify,实际 %d 处:\n%s", n, out)
	}

	// 不自签:一个也不带
	e[0].Insecure = false
	links = GenerateLinks(user, lines, e)
	for _, l := range links {
		if strings.Contains(strings.ToLower(l), "insecure") {
			t.Fatalf("正式证书不应带 insecure: %s", l)
		}
	}

	// 自签判断
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	certutil.GenerateSelfSigned([]string{"1.2.3.4"}, crt, key, 10)
	if !CertIsSelfSigned(crt, "") || CertIsSelfSigned("", "") || !CertIsSelfSigned("", crt) {
		t.Fatal("自签判断错误")
	}
}
