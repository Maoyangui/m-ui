package hub

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/notify"
)

// caSignedFor 用 ca 签一张只含域名(不含 IP)的证书:Let's Encrypt 签的就是这样。
func caSignedFor(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: dns}, DNSNames: []string{dns},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// 钉了指纹的副机按 IP 填 API 地址(很常见):副机证书是 Let's Encrypt 给域名签的,里面没有 IP。
// 以前按 IP 校验证书链必然失败,副机一续期主机就报"指纹变了"失联,看到的证书天数从此不再更新;
// 现在 API 地址是 IP 时按服务器的域名校验。换成别的域名的证书、换成自签的,仍然拒绝。
func TestPinnedNodeIPApiUrlFollowsCARenewal(t *testing.T) {
	db := openDB(t, "renew-ip.db").DB
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	c1, c2 := caSignedFor(t, ca, caKey, "sg.example.com"), caSignedFor(t, ca, caKey, "sg.example.com")
	other, forged := caSignedFor(t, ca, caKey, "other.example.com"), selfSigned(t, "sg.example.com")
	var cur atomic.Pointer[tls.Certificate]
	cur.Store(&c1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":"1"}`))
	}))
	srv.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return &tls.Config{Certificates: []tls.Certificate{*cur.Load()}}, nil
	}}
	srv.StartTLS()
	defer srv.Close()

	db.Create(&model.Node{Name: "sg", Domain: "sg.example.com", ApiUrl: srv.URL, Token: "tok", Insecure: true})
	h := New(Deps{DB: db})
	h.roots = pool
	load := func() model.Node {
		var n model.Node
		db.First(&n, 1)
		return n
	}
	if _, err := h.Ping(load()); err != nil || load().CertFP != fingerprint(c1) {
		t.Fatalf("首次连接应记住指纹: %v %q", err, load().CertFP)
	}
	cur.Store(&c2) // 副机自动续期
	h.CloseIdleConnections()
	if _, err := h.Ping(load()); err != nil {
		t.Fatalf("API 地址是 IP、证书是给服务器域名签的:CA 续期后应照常连上,得 %v", err)
	}
	if fp := load().CertFP; fp != fingerprint(c2) {
		t.Fatalf("应换记续期后的指纹,得 %q", fp)
	}
	for _, bad := range []tls.Certificate{other, forged} {
		bad := bad
		cur.Store(&bad)
		h.CloseIdleConnections()
		if _, err := h.Ping(load()); err == nil || !strings.Contains(err.Error(), "指纹变了") {
			t.Fatalf("换成别的域名的证书或自签证书应拒绝,得 %v", err)
		}
		if fp := load().CertFP; fp != fingerprint(c2) {
			t.Fatalf("被拒绝时不该改动指纹,得 %q", fp)
		}
	}
	h.CloseIdleConnections()
}

// 副机的证书出了问题,主机用自己的通知渠道代它说(副机一般没配通知,以前续期失败只写进副机自己的日志):
// 自动续期在失败、正用着又不会自动续的快到期了;每台每张每天一次,主机重启不重复;没人用的不提醒;
// 老版本副机不报续期状态,只看线路默认证书的天数。
func TestMasterAlertsNodeCertRenewFailure(t *testing.T) {
	db := openDB(t, "alert-cert.db").DB
	store := ""
	setting := func(string) string { return "" }
	var got []string
	hubWith := func() *Hub {
		n := notify.New(setting)
		n.Persist(func() string { return store }, func(v string) { store = v })
		return New(Deps{DB: db, Setting: setting, IsNode: func() bool { return false }, OnceDurable: n.OnceDurable,
			Notify: func(_, text string) { got = append(got, text) }})
	}
	h := hubWith()
	sg := model.Node{Id: 2, Name: "sg"}
	now := time.Now()
	failing := model.CertBrief{Name: "sgv6.example.com", NotAfter: now.Add(20 * 24 * time.Hour).Unix(), Source: "acme", Method: "http", AutoRenew: true, InUse: true,
		RenewErr: "线路「CDN-80」占着 80 端口,HTTP 验证没法进行", RenewFails: 3}
	healthy := model.CertBrief{Name: "ok.example.com", NotAfter: now.Add(80 * 24 * time.Hour).Unix(), Source: "acme", Method: "http", AutoRenew: true, InUse: true}
	rep := &Report{Certs: []model.CertBrief{failing, healthy}}
	h.setStatus(sg, true, "", rep)
	if len(got) != 1 || !strings.Contains(got[0], "sg") || !strings.Contains(got[0], "sgv6.example.com") || !strings.Contains(got[0], "CDN-80") || !strings.Contains(got[0], "3 次") {
		t.Fatalf("副机自动续期失败,主机应代它告警一次并说明原因: %v", got)
	}
	h.setStatus(sg, true, "", rep)
	if len(got) != 1 {
		t.Fatalf("同一天不重复: %v", got)
	}
	h = hubWith() // 主机重启
	h.setStatus(sg, true, "", rep)
	if len(got) != 1 {
		t.Fatalf("主机重启后同一天也不重复: %v", got)
	}
	// 正用着的外部证书剩 10 天、手动 DNS 剩 25 天:提醒;没人用的快到期也不提醒
	ext := model.CertBrief{Name: "ext.example.com", NotAfter: now.Add(10 * 24 * time.Hour).Unix(), Source: "external", InUse: true}
	manual := model.CertBrief{Name: "*.example.com", NotAfter: now.Add(25 * 24 * time.Hour).Unix(), Source: "acme", Method: "manual", InUse: true}
	idle := model.CertBrief{Name: "idle.example.com", NotAfter: now.Add(3 * 24 * time.Hour).Unix(), Source: "external"}
	h.setStatus(sg, true, "", &Report{Certs: []model.CertBrief{failing, ext, manual, idle}})
	if all := strings.Join(got, "\n"); len(got) != 3 || !strings.Contains(all, "ext.example.com") || !strings.Contains(all, "手动 DNS") || strings.Contains(all, "idle.example.com") {
		t.Fatalf("正用着的快到期证书要提醒,没人用的不提醒: %v", got)
	}
	// 老版本副机:没有续期状态,线路默认证书剩 5 天也要提醒
	old := model.CertBrief{Name: "old.example.com", NotAfter: now.Add(5 * 24 * time.Hour).Unix(), Default: true}
	h.setStatus(model.Node{Id: 3, Name: "jp"}, true, "", &Report{Certs: []model.CertBrief{old}})
	if len(got) != 4 || !strings.Contains(got[3], "jp") || !strings.Contains(got[3], "old.example.com") {
		t.Fatalf("老版本副机的默认证书快到期也要提醒: %v", got)
	}
}

// 证书清单里的续期状态(失败原因、重试时间)变了不改快照修订号:续期失败一次不该把整份配置重推给所有副机;
// 清单本身变了(续上了,到期时间变了)照旧算。
func TestRevisionIgnoresCertRenewStatus(t *testing.T) {
	db := openDB(t, "rev-cert.db").DB
	db.Create(&model.Node{Name: "sg", ApiUrl: "https://sg:2053/x/", Token: "t"})
	set := func(b model.CertBrief) {
		raw, _ := json.Marshal([]model.CertBrief{b})
		db.Model(&model.Node{}).Where("id = ?", 1).Update("certs", json.RawMessage(raw))
	}
	setting := func(string) string { return "" }
	brief := model.CertBrief{Name: "a.example.com", Domains: []string{"a.example.com"}, NotAfter: 2000000000, Default: true, Source: "acme", Method: "http", AutoRenew: true, InUse: true}
	set(brief)
	a, _ := BuildSnapshot(db, setting)
	failing := brief
	failing.RenewErr, failing.RenewFails, failing.NextRenewAt = "boom", 2, 1999999999
	set(failing)
	if b, _ := BuildSnapshot(db, setting); b.Revision != a.Revision {
		t.Fatal("续期状态变了不该改变修订号")
	}
	renewed := brief
	renewed.NotAfter = 2100000000
	set(renewed)
	if c, _ := BuildSnapshot(db, setting); c.Revision == a.Revision {
		t.Fatal("证书清单本身变了要改变修订号")
	}
}
