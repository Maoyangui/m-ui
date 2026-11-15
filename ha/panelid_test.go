package ha

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database/model"
)

// 面板跟着主机走(用户 11-15:接任后外部 API 不用重填):连订阅一起接管、面板用域名时,新主机换上原主机的面板、代理面板的
// 域名、端口、路径、证书、受信任的反向代理,面板、外部 API、代理面板的地址不变;面板用 IP、或没接管订阅时不动。
func TestPanelFollowsOnTakeover(t *testing.T) {
	for _, tc := range []struct {
		name      string
		take      bool
		domain    string
		lineClash bool
	}{
		{"接管订阅 + 面板域名:跟过去", true, "panel.example.com", false},
		{"只接管主机的工作:不动", false, "panel.example.com", false},
		{"面板用 IP:跟不过去", true, "", false},
		{"新主机上线路占着主机的面板端口:面板留自己的", true, "panel.example.com", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newP2(t, 1, ModeConfirm)
			m, c := p.m, p.cands[0]
			dir := t.TempDir()
			crt, key := filepath.Join(dir, "sub.crt"), filepath.Join(dir, "sub.key")
			if err := certutil.GenerateSelfSigned([]string{"sub.example.com", "panel.example.com"}, crt, key, 60); err != nil {
				t.Fatal(err)
			}
			m.set(map[string]string{"subDomain": "sub.example.com", "subPort": "2096", "subPath": "/s/", "subCertFile": crt, "subKeyFile": key,
				"webDomain": tc.domain, "webPort": "8443", "webPath": "/mgmt/", "webCertFile": crt, "webKeyFile": key,
				"resellerPort": "8444", "resellerPath": "/agent/", "trustedProxies": "173.245.48.0/20"})
			if tc.take {
				m.set(map[string]string{KeyTakeSub: "true", KeyDnsToken: "tok", KeyDnsNames: "sub.example.com"})
			}
			c.set(map[string]string{"webPort": "2053", "webPath": "/app/", "webDomain": "", "resellerPort": "2054", "resellerPath": "/dl/", "subPort": "2087"})
			if tc.lineClash {
				nodes, _ := json.Marshal([]uint{p.ids[0]})
				m.d.DB.Create(&model.Line{Name: "VL", Protocol: "vless", Port: 8443, Enabled: true, NodeIds: nodes})
			}
			if err := m.ReplicateNow(); err != nil {
				t.Fatal(err)
			}
			if err := c.Takeover(TakeoverOpts{Force: true, By: "cli"}); err != nil {
				t.Fatal(err)
			}
			ndb := swapIn(t, c)
			get := func(k string) string {
				var v string
				ndb.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
				return v
			}
			var b Became
			if err := json.Unmarshal([]byte(get(KeyBecame)), &b); err != nil {
				t.Fatal(err)
			}
			moved := tc.take && tc.domain != "" && !tc.lineClash
			if moved {
				if get("webDomain") != "panel.example.com" || get("webPort") != "8443" || get("webPath") != "/mgmt/" || get("resellerPort") != "8444" ||
					get("resellerPath") != "/agent/" || get("trustedProxies") != "173.245.48.0/20" || get("webCertFile") == "" || get("webCertFile") != get("subCertFile") {
					t.Fatalf("面板、代理面板换成原主机的(和订阅同一张证书): %s %s %s %s %s %s cert=%s sub=%s", get("webDomain"), get("webPort"), get("webPath"),
						get("resellerPort"), get("resellerPath"), get("trustedProxies"), get("webCertFile"), get("subCertFile"))
				}
				if !b.Identity.PanelKept || !b.Identity.ResellerKept || b.Identity.PrevPanel == nil || b.Identity.PrevPanel.Port != 2053 {
					t.Fatalf("记下面板跟过去了、原来的面板(启动绑不上时改回): %+v", b.Identity)
				}
				return
			}
			if get("webPort") != "2053" || get("webPath") != "/app/" || get("resellerPort") != "2054" || b.Identity.PanelKept {
				t.Fatalf("面板不动: %s %s %s %+v", get("webPort"), get("webPath"), get("resellerPort"), b.Identity)
			}
			if tc.lineClash {
				found := false
				for _, w := range b.Identity.Warnings {
					if w.Code == idwPanelPortClash && w.Args["what"] == "VL" && w.Args["port"] == "8443" {
						found = true
					}
				}
				if !found {
					t.Fatalf("线路占着主机的面板端口:提醒面板留自己的: %+v", b.Identity.Warnings)
				}
			}
		})
	}
}

// 迁来的面板 / 代理面板端口启动时绑不上:改回本机原来的(只在成为主机后第一次启动、还没发通知时),记进告警;改回后不再改。
func TestPanelFallbackAtStartup(t *testing.T) {
	s := testService(t, false, map[string]string{"webPort": "8443", "webPath": "/mgmt/", "webDomain": "panel.example.com", "resellerPort": "8444", "resellerPath": "/agent/"})
	b := Became{Identity: identityResult{PanelKept: true, ResellerKept: true,
		PrevPanel:    &panelPrev{Port: 2053, Path: "/app/", Domain: "", ResellerPort: 2054, ResellerPath: "/dl/"},
		PrevReseller: &panelPrev{Port: 2053, Path: "/app/", ResellerPort: 2054, ResellerPath: "/dl/"}}}
	s.set(map[string]string{KeyBecame: mustJSON(b)})
	if !s.PanelFallback(errors.New("address already in use")) {
		t.Fatal("绑不上:改回本机原来的面板")
	}
	if s.setting("webPort") != "2053" || s.setting("webPath") != "/app/" || s.setting("webDomain") != "" {
		t.Fatalf("面板设置改回: %s %s %q", s.setting("webPort"), s.setting("webPath"), s.setting("webDomain"))
	}
	if s.PanelFallback(errors.New("again")) {
		t.Fatal("已经改回过:不再改")
	}
	if !s.ResellerFallback(errors.New("address already in use")) || s.setting("resellerPort") != "2054" || s.setting("resellerPath") != "/dl/" {
		t.Fatal("代理面板同样改回")
	}
	var got Became
	s.getJSON(KeyBecame, &got)
	codes := map[string]bool{}
	for _, w := range got.Identity.Warnings {
		codes[w.Code] = true
	}
	if !codes[idwPanelPortBind] || !codes[idwResellerPortBind] || got.Identity.PanelKept || got.Identity.ResellerKept {
		t.Fatalf("记进告警、不再算跟过去: %+v", got.Identity)
	}
	// 已经发过通知(不是刚成为主机的那次启动):不改
	s2 := testService(t, false, map[string]string{"webPort": "8443"})
	b.Notified = true
	s2.set(map[string]string{KeyBecame: mustJSON(b)})
	if s2.PanelFallback(errors.New("x")) {
		t.Fatal("不是刚成为主机:端口绑不上照常报错,不偷偷改")
	}
}

// 面板跟过来了、证书不能用:经 DNS 接口给面板域名重签;和订阅是同一个域名、订阅也要重签时只签一张两处都用。
func TestPanelCertAfterTakeover(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, hosts []string, days int) string {
		crt, key := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
		if err := certutil.GenerateSelfSigned(hosts, crt, key, days); err != nil {
			t.Fatal(err)
		}
		return crt
	}
	soon := mk("soon", []string{"panel.example.com", "sub.example.com"}, 5)
	good := mk("good", []string{"panel.example.com"}, 60)
	s := testService(t, false, map[string]string{KeyDnsToken: "tok"})
	var subCalls []string
	var panelCalls []string
	s.d.IssueSubCert = func(d string) error { subCalls = append(subCalls, d); return nil }
	s.d.IssuePanelCert = func(d string, alsoSub bool) error {
		panelCalls = append(panelCalls, d+map[bool]string{true: "+sub", false: ""}[alsoSub])
		return nil
	}
	// 不同域名:面板证书单独重签
	s.set(map[string]string{"webCertFile": soon, "subCertFile": good})
	b := Became{Identity: identityResult{Taken: true, SubHTTPS: true, SubHost: "sub.example.com", PanelKept: true, PanelHTTPS: true, PanelHost: "panel.example.com"}}
	s.ensureSubCert(&b)
	s.ensurePanelCert(&b)
	if len(panelCalls) != 1 || panelCalls[0] != "panel.example.com" || len(subCalls) != 1 {
		t.Fatalf("面板证书单独重签;订阅证书(good 不含 sub 域名)也重签: panel=%v sub=%v", panelCalls, subCalls)
	}
	// 同一个域名、订阅证书也要重签:一张两处都用(走面板钩子带 alsoSub),面板不再单独签
	panelCalls, subCalls = nil, nil
	s.set(map[string]string{"webCertFile": soon, "subCertFile": soon})
	b = Became{Identity: identityResult{Taken: true, SubHTTPS: true, SubHost: "panel.example.com", PanelKept: true, PanelHTTPS: true, PanelHost: "panel.example.com"}}
	s.ensureSubCert(&b)
	s.ensurePanelCert(&b)
	if len(panelCalls) != 1 || panelCalls[0] != "panel.example.com+sub" || len(subCalls) != 0 {
		t.Fatalf("同一个域名:一张证书两处都用: panel=%v sub=%v", panelCalls, subCalls)
	}
	// 面板证书能用:不签
	panelCalls = nil
	s.set(map[string]string{"webCertFile": good})
	b = Became{Identity: identityResult{PanelKept: true, PanelHTTPS: true, PanelHost: "panel.example.com"}}
	s.ensurePanelCert(&b)
	if len(panelCalls) != 0 {
		t.Fatal("面板证书能用:不签")
	}
	_ = time.Now
}
