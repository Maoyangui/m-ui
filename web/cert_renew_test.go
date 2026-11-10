package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// HTTP 验证每次续期都要临时占 80 端口:部署范围里有这样的证书时,新建 TCP 80 线路要拦下并说清楚
// (以前一条走 CDN 的 80 线路推到全部服务器,所有 HTTP 验证的证书从此天天续期失败)。UDP 的 hy2 线路不占 TCP 80。
func TestLineOnPort80RejectedWithHTTP01Cert(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	db.Create(&model.Cert{Name: "hk.example.com", Source: "acme", Method: "http", AutoRenew: true, CertFile: "/x.crt", KeyFile: "/x.key"})

	err = s.validateLine(&model.Line{Name: "CDN-80", Protocol: "vmess", Port: 80, Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "hk.example.com") || !strings.Contains(err.Error(), "80") {
		t.Fatalf("部署到有 HTTP 验证证书的服务器的 TCP 80 线路应拒绝并点名证书: %v", err)
	}
	// hy2 只用 UDP:不归这条规则管(本机 80 端口能不能绑是另一回事)
	if err := s.validateLine(&model.Line{Name: "HY2-80", Protocol: "hysteria2", Port: 80, Enabled: true}); err != nil && strings.Contains(err.Error(), "hk.example.com") {
		t.Fatalf("UDP 线路不该因为 HTTP 验证被拦: %v", err)
	}

	// 副机按它报上来的清单看(老版本副机不报验证方式,不拦)
	briefs, _ := json.Marshal([]model.CertBrief{{Name: "sg.example.com", NotAfter: time.Now().Add(60 * 24 * time.Hour).Unix(), Source: "acme", Method: "http", AutoRenew: true}})
	db.Create(&model.Node{Name: "副机A", ApiUrl: "http://a", Enabled: true, Sort: 2, Certs: briefs})
	old, _ := json.Marshal([]model.CertBrief{{Name: "old.example.com", NotAfter: time.Now().Add(60 * 24 * time.Hour).Unix()}})
	db.Create(&model.Node{Name: "副机B", ApiUrl: "http://b", Enabled: true, Sort: 3, Certs: old})
	err = s.validateLine(&model.Line{Name: "A-80", Protocol: "vless", Port: 80, Enabled: true, NodeIds: []byte(`[2]`)})
	if err == nil || !strings.Contains(err.Error(), "副机A") || !strings.Contains(err.Error(), "sg.example.com") {
		t.Fatalf("部署到副机A 的 80 线路应拒绝并点名那台的证书: %v", err)
	}
	if err := s.validateLine(&model.Line{Name: "A-HY2-80", Protocol: "hysteria2", Port: 80, Enabled: true, NodeIds: []byte(`[2]`)}); err != nil {
		t.Fatalf("只在副机上的 hy2 80 线路应放行: %v", err)
	}
	if err := s.validateLine(&model.Line{Name: "B-80", Protocol: "vless", Port: 80, Enabled: true, NodeIds: []byte(`[3]`)}); err != nil {
		t.Fatalf("副机B 没有 HTTP 验证的证书,应放行: %v", err)
	}
	// 原本就在 80 上的线路:改别的不拦(续期前的预检会点它的名)
	existing := model.Line{Name: "B-80", Protocol: "vless", Port: 80, Enabled: true, NodeIds: []byte(`[3]`)}
	db.Create(&existing)
	existing.NodeIds = []byte(`[2,3]`)
	if err := s.validateLine(&existing); err != nil && strings.Contains(err.Error(), "续期") {
		t.Fatalf("已经在 80 上的线路改部署范围不该因为证书续期被拦: %v", err)
	}
}

// leCertFile 写一张 R11(Let's Encrypt)签的证书,还剩 left,有效期 90 天。
func leCertFile(t *testing.T, dir, name string, left time.Duration) (string, string) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "R11", Organization: []string{"Let's Encrypt"}},
		NotBefore: time.Now().Add(-365 * 24 * time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(left - 90*24*time.Hour), NotAfter: time.Now().Add(left)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c, k := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	os.WriteFile(c, append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...), 0o644)
	os.WriteFile(k, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return c, k
}

func certServer(t *testing.T) (*Server, *runner.Runner) {
	t.Helper()
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(run.DB()) })
	return NewServer(run), run
}

func certCall(s *Server, method, action, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, "http://x/app/api/cert/"+action, strings.NewReader(body))
	if action == "" {
		r = httptest.NewRequest(method, "http://x/app/api/cert", nil)
		s.handleCert(w, r)
	} else {
		s.handleCertSub(w, r)
	}
	return w
}

// failingRenewal 证书库里一张该续期、却续不上的证书(Cloudflare 验证,没有 Token),外加一张手动 DNS 的;
// 点一次「续期」并等它结束。
func failingRenewal(t *testing.T) (*Server, *runner.Runner) {
	t.Helper()
	s, run := certServer(t)
	c, k := leCertFile(t, filepath.Join(run.DataDir(), "cert"), "hk.example.com", 23*24*time.Hour)
	run.DB().Create(&model.Cert{Name: "hk.example.com", Source: "acme", Method: "cloudflare", AutoRenew: true, CertFile: c, KeyFile: k})
	mc, mk := leCertFile(t, filepath.Join(run.DataDir(), "cert"), "m.example.com", 60*24*time.Hour)
	run.DB().Create(&model.Cert{Name: "m.example.com", Source: "acme", Method: "manual", CertFile: mc, KeyFile: mk})
	return s, run
}

func waitIssue(t *testing.T, run *runner.Runner) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for run.CertStatus()["running"] == true && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

// 证书库的「续期」按这张自己的验证方式等设置续,不改签发表单的全局默认
// (以前走签发接口,续一张手动 DNS 证书,之后新签证书的自动续期就默认不勾了)。
func TestCertRenewEndpointKeepsGlobals(t *testing.T) {
	s, run := failingRenewal(t)
	globals := map[string]string{"acmeDomain": "form.example.com", "acmeMethod": "http", "acmeStaging": "false", "acmeAutoRenew": "false"}
	if err := run.SetSettings(globals); err != nil {
		t.Fatal(err)
	}
	if w := certCall(s, "POST", "renew", `{"name":"hk.example.com"}`); w.Code != http.StatusOK {
		t.Fatalf("续期: %d %s", w.Code, w.Body.String())
	}
	waitIssue(t, run)
	for k, v := range globals {
		if got := s.setting(k); got != v {
			t.Fatalf("续期改了签发表单的默认值 %s: %q → %q", k, v, got)
		}
	}
	if w := certCall(s, "POST", "renew", `{"name":"nope.example.com"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("库里没有的证书不能续: %d", w.Code)
	}
}

// 证书页的接口带上每张证书的续期情况:能不能自动续、为什么、上次失败的原因、次数、下次重试时间;本机没配通知也要说。
func TestCertAPIShowsRenewStatus(t *testing.T) {
	s, run := failingRenewal(t)
	if w := certCall(s, "POST", "renew", `{"name":"hk.example.com"}`); w.Code != http.StatusOK {
		t.Fatalf("续期: %d %s", w.Code, w.Body.String())
	}
	waitIssue(t, run)
	w := certCall(s, "GET", "", "")
	var resp struct {
		Certs []struct {
			Name         string `json:"name"`
			Renewable    bool   `json:"renewable"`
			Reason       string `json:"reason"`
			RenewBefore  int64  `json:"renewBefore"`
			LastRenewErr string `json:"lastRenewErr"`
			RenewFails   int    `json:"renewFails"`
			NextRenewAt  int64  `json:"nextRenewAt"`
		} `json:"certs"`
		NotifyConfigured *bool `json:"notifyConfigured"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Certs) != 2 {
		t.Fatalf("证书接口: %v %s", err, w.Body.String())
	}
	for _, c := range resp.Certs {
		switch c.Name {
		case "hk.example.com":
			if !c.Renewable || c.RenewFails != 1 || !strings.Contains(c.LastRenewErr, "Cloudflare") || c.NextRenewAt <= time.Now().Unix() || c.RenewBefore != 30*86400 {
				t.Fatalf("续不上的那张要看得到失败原因、次数和下次重试时间: %+v", c)
			}
		case "m.example.com":
			if c.Renewable || c.Reason != "manual" {
				t.Fatalf("手动 DNS 的要标明不能自动续: %+v", c)
			}
		}
	}
	if resp.NotifyConfigured == nil || *resp.NotifyConfigured {
		t.Fatalf("要说本机没配 Telegram / Webhook: %s", w.Body.String())
	}
	run.SetSettings(map[string]string{"webhookUrl": "https://hook.example.com/m-ui"})
	if w := certCall(s, "GET", "", ""); !strings.Contains(w.Body.String(), `"notifyConfigured":true`) {
		t.Fatalf("配了 Webhook 就算配了通知: %s", w.Body.String())
	}
}

// 证书页的自动续期开关:手动 DNS、服务器上已有的打不开,说清楚为什么;面板签发的能开能关。
func TestCertAutoRenewToggleRejectsManualAndExternal(t *testing.T) {
	s, run := certServer(t)
	dir := filepath.Join(run.DataDir(), "cert")
	for _, row := range []model.Cert{
		{Name: "a.example.com", Source: "acme", Method: "http"},
		{Name: "m.example.com", Source: "acme", Method: "manual"},
		{Name: "e.example.com", Source: "external"},
	} {
		row.CertFile, row.KeyFile = leCertFile(t, dir, row.Name, 60*24*time.Hour)
		run.DB().Create(&row)
	}
	autoRenew := func(name string) bool {
		var c model.Cert
		run.DB().Where("name = ?", name).First(&c)
		return c.AutoRenew
	}
	if w := certCall(s, "POST", "autorenew", `{"name":"a.example.com","on":true}`); w.Code != http.StatusOK || !autoRenew("a.example.com") {
		t.Fatalf("面板签发的应能打开: %d %s", w.Code, w.Body.String())
	}
	if w := certCall(s, "POST", "autorenew", `{"name":"a.example.com","on":false}`); w.Code != http.StatusOK || autoRenew("a.example.com") {
		t.Fatalf("应能关掉: %d %s", w.Code, w.Body.String())
	}
	for name, why := range map[string]string{"m.example.com": "手动 DNS", "e.example.com": "certbot"} {
		w := certCall(s, "POST", "autorenew", `{"name":"`+name+`","on":true}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), why) || autoRenew(name) {
			t.Fatalf("%s 打不开,要说为什么: %d %s", name, w.Code, w.Body.String())
		}
	}
}

// 添加服务器上已有的证书撞上同名、开着自动续期的面板证书:409,确认后带 force 再提交才换。
func TestExternalCertConflictNeedsForce(t *testing.T) {
	s, run := certServer(t)
	c, k := leCertFile(t, filepath.Join(run.DataDir(), "cert"), "hk.example.com", 60*24*time.Hour)
	run.DB().Create(&model.Cert{Name: "hk.example.com", Source: "acme", Method: "http", AutoRenew: true, CertFile: c, KeyFile: k})
	ec, ek := leCertFile(t, t.TempDir(), "hk.example.com", 80*24*time.Hour)
	body, _ := json.Marshal(map[string]interface{}{"certFile": ec, "keyFile": ek})
	w := certCall(s, "POST", "external", string(body))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "needForce") {
		t.Fatalf("应 409 要求确认: %d %s", w.Code, w.Body.String())
	}
	body, _ = json.Marshal(map[string]interface{}{"certFile": ec, "keyFile": ek, "force": true})
	if w := certCall(s, "POST", "external", string(body)); w.Code != http.StatusOK {
		t.Fatalf("确认后应换上: %d %s", w.Code, w.Body.String())
	}
	var row model.Cert
	run.DB().Where("name = ?", "hk.example.com").First(&row)
	if row.Source != "external" || row.AutoRenew || row.CertFile != ec {
		t.Fatalf("应换成服务器上的证书: %+v", row)
	}
}
