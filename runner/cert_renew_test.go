package runner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/acme"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

const day = 24 * time.Hour

// testIssuer 一张"Let's Encrypt 中间证书":签出来的叶子证书和真的一样,签发者 CN 是 R11 / YE1 这类名字、O = Let's Encrypt。
type testIssuer struct {
	cert *x509.Certificate
	der  []byte
	key  *ecdsa.PrivateKey
}

func newIssuer(t *testing.T, cn string) *testIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn, Organization: []string{"Let's Encrypt"}, Country: []string{"US"}},
		NotBefore: time.Now().Add(-365 * day), NotAfter: time.Now().Add(365 * day), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testIssuer{cert: c, der: der, key: key}
}

// sign 给 names 签一张 notBefore ~ notAfter 的证书,返回 fullchain PEM 与私钥 PEM。
func (is *testIssuer) sign(t *testing.T, names []string, notBefore, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: notBefore, NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, is.cert, &key.PublicKey, is.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: is.der})...)
	return chain, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

// leCert 在 dir 下写一张签发者为 issuerCN 的证书(文件名和面板自己签的一样:<域名>.crt / .key),有效期 notBefore ~ notAfter。
func leCert(t *testing.T, dir, name, issuerCN string, notBefore, notAfter time.Time) (string, string) {
	t.Helper()
	chain, key := newIssuer(t, issuerCN).sign(t, []string{name}, notBefore, notAfter)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c, k := filepath.Join(dir, certFileName(name)+".crt"), filepath.Join(dir, certFileName(name)+".key")
	if err := os.WriteFile(c, chain, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(k, key, 0o600); err != nil {
		t.Fatal(err)
	}
	return c, k
}

// le90 一张 90 天的 Let's Encrypt 证书(签发者 R11),还剩 left。
func le90(t *testing.T, dir, name string, left time.Duration) (string, string) {
	t.Helper()
	now := time.Now()
	return leCert(t, dir, name, "R11", now.Add(left-90*day), now.Add(left))
}

// notices 打开 Telegram 通知,把要发出去的消息收下来。Event 是异步发的,取之前稍等。
func notices(t *testing.T, r *Runner) func() []string {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	r.notifier.SetSender(func(_, _, text string) error {
		mu.Lock()
		sent = append(sent, text)
		mu.Unlock()
		return nil
	})
	if err := r.SetSettings(map[string]string{"tgEnabled": "true", "tgToken": "x", "tgChatId": "1"}); err != nil {
		t.Fatal(err)
	}
	return func() []string {
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

// reopen 同一个库再起一个 Runner:模拟进程重启(内存里的状态全没了,库里的还在)。
func reopen(t *testing.T, r *Runner) *Runner {
	t.Helper()
	r2, err := New(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r2.core.Stop()
		database.Close(r2.DB())
	})
	return r2
}

// 老版本升上来的 Let's Encrypt 证书没记来源,按签发者认:现在的中间证书叫 R10/R11/E5/E6/YE1 这类名字,
// CN 里没有 "encrypt",以前全被当成"服务器上已有的证书",永远不续(审计 I2)。
func TestGuessSourceLetsEncrypt(t *testing.T) {
	data := t.TempDir()
	in := filepath.Join(data, "cert", "a.example.com.crt")
	for _, issuer := range []string{"R10", "R11", "R14", "E5", "E6", "E9", "YE1", "YR2", "(STAGING) Pseudo Plum E5", "(STAGING) Counterfeit Cashew R10", "Let's Encrypt Authority X3"} {
		if got := guessSource(acme.CertInfo{Exists: true, Issuer: issuer}, in, data); got != "acme" {
			t.Errorf("签发者 %q 是 Let's Encrypt,应识别为 acme,得 %q", issuer, got)
		}
	}
	for _, issuer := range []string{"Sectigo RSA Domain Validation Secure Server CA", "WE1", "WR2", "GTS CA 1P5", "ZeroSSL ECC Domain Secure Site CA", "R", "E", "SSL.com SSL Intermediate CA ECC R2"} {
		if got := guessSource(acme.CertInfo{Exists: true, Issuer: issuer}, in, data); got != "external" {
			t.Errorf("签发者 %q 不是 Let's Encrypt,应算外部证书,得 %q", issuer, got)
		}
	}
	// 签发者的组织是 Let's Encrypt 的,CN 叫什么都认(以后再换中间证书也不怕)
	for _, org := range []string{"Let's Encrypt", "(STAGING) Let's Encrypt"} {
		if got := guessSource(acme.CertInfo{Exists: true, Issuer: "Some Future CA", IssuerOrg: []string{org}}, in, data); got != "acme" {
			t.Errorf("签发者组织 %q 应识别为 acme,得 %q", org, got)
		}
	}
	if !(acme.CertInfo{Issuer: "(STAGING) Pseudo Plum E5"}).IsStaging() || (acme.CertInfo{Issuer: "R11"}).IsStaging() {
		t.Error("测试环境的证书要认得出来")
	}
	// 数据目录外的一律算外部证书:certbot 的原件不能让面板续期覆盖
	if got := guessSource(acme.CertInfo{Exists: true, Issuer: "R11"}, "/etc/letsencrypt/live/a.example.com/fullchain.pem", data); got != "external" {
		t.Errorf("数据目录外的证书应算外部证书,得 %q", got)
	}
	if got := guessSource(acme.CertInfo{Exists: true, Issuer: "R11"}, data+"-old/cert/a.example.com.crt", data); got != "external" {
		t.Errorf("只是路径前缀相同的别的目录不算数据目录,得 %q", got)
	}
	// 还原时放进 cert/restored/ 的是 certbot 等外部证书的副本,不是面板签发的
	if got := guessSource(acme.CertInfo{Exists: true, Issuer: "R11"}, filepath.Join(data, "cert", "restored", "a.example.com-fullchain.pem"), data); got != "external" {
		t.Errorf("还原进来的外部证书副本应算外部证书,得 %q", got)
	}
}

// 老版本留在数据目录 cert/ 下的 Let's Encrypt 证书,升级时收进库要记成 acme 并开着自动续期。
func TestEnsureCertStoreImportsLEAsACME(t *testing.T) {
	dir := t.TempDir()
	le90(t, filepath.Join(dir, "cert"), "hk.example.com", 23*day+time.Hour)
	r, err := New(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.core.Stop(); database.Close(r.DB()) }()
	got := r.Certs()
	if len(got) != 1 {
		t.Fatalf("应收进 1 张: %+v", got)
	}
	if got[0].Source != "acme" || !got[0].AutoRenew || got[0].Method != acme.MethodHTTP {
		t.Fatalf("签发者是 R11 的证书应记成 acme、开着自动续期: source=%q autoRenew=%v method=%q", got[0].Source, got[0].AutoRenew, got[0].Method)
	}
}

// 已经被误判成外部证书的:数据目录 cert/ 下、签发者是 Let's Encrypt 的,升级时改回 acme(只修一次);
// 数据目录外的不动。
func TestEnsureCertStoreRepairsMisclassifiedLE(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	in, inKey := leCert(t, filepath.Join(dir, "cert"), "sgv6.example.com", "YE1", now.Add(-50*day), now.Add(40*day))
	out, outKey := leCert(t, t.TempDir(), "ext.example.com", "YE1", now.Add(-50*day), now.Add(40*day))
	// 还原备份时放进 cert/restored/ 的 certbot 证书副本:在数据目录 cert/ 下,但不是面板签发的
	copied, copiedKey := leCert(t, filepath.Join(dir, "cert", "restored"), "copy.example.com", "YE1", now.Add(-50*day), now.Add(40*day))
	db, err := database.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Create(&model.Cert{Name: "sgv6.example.com", Source: "external", CertFile: in, KeyFile: inKey, CreatedAt: 1})
	db.Create(&model.Cert{Name: "ext.example.com", Source: "external", CertFile: out, KeyFile: outKey, CreatedAt: 1})
	db.Create(&model.Cert{Name: "copy.example.com", Source: "external", CertFile: copied, KeyFile: copiedKey, CreatedAt: 1})
	database.Close(db)

	r, err := New(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.core.Stop(); database.Close(r.DB()) }()
	row := func(name string) model.Cert {
		var c model.Cert
		if err := r.db.Where("name = ?", name).First(&c).Error; err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := row("sgv6.example.com"); c.Source != "acme" || !c.AutoRenew || c.Method != acme.MethodHTTP {
		t.Fatalf("数据目录下被误判的 LE 证书应改回 acme 并开自动续期: %+v", c)
	}
	if c := row("ext.example.com"); c.Source != "external" || c.AutoRenew {
		t.Fatalf("数据目录外的证书不能动: %+v", c)
	}
	if c := row("copy.example.com"); c.Source != "external" || c.AutoRenew {
		t.Fatalf("还原进来的外部证书副本不能改成面板签发的: %+v", c)
	}
	// 只修一次:管理员事后又把它改回外部证书,下次启动不再动它
	r.db.Model(&model.Cert{}).Where("name = ?", "sgv6.example.com").Updates(map[string]interface{}{"source": "external", "auto_renew": false})
	r.ensureCertStore()
	if c := row("sgv6.example.com"); c.Source != "external" {
		t.Fatalf("修复只做一次,之后不该再改: %+v", c)
	}
}

// 续期那一刻刚好有别的签发在跑(比如有人在等手动 DNS 记录):这次不算失败、不发通知、不占重试名额;
// 那边一结束,下一轮马上补试(以前占掉 20 小时的去重名额,当天不再试)。
func TestRenewDueBusyDoesNotConsumeAttempt(t *testing.T) {
	r := certTestRunner(t)
	sent := notices(t, r)
	c, k := le90(t, filepath.Join(r.DataDir(), "cert"), "hk.example.com", 23*day+time.Hour)
	if err := r.saveCertRow(model.Cert{Name: "hk.example.com", Source: "acme", CertFile: c, KeyFile: k, Method: acme.MethodCloudflare, AutoRenew: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.startIssue(); err != nil { // 另一个签发正在进行
		t.Fatal(err)
	}
	r.renewDue()
	if msgs := sent(); len(msgs) != 0 {
		t.Fatalf("忙的时候不该有通知: %v", msgs)
	}
	r.cert.mu.Lock()
	r.cert.running = false // 那边结束了
	r.cert.mu.Unlock()
	r.renewDue()
	if st := r.CertStatus(); st["lastError"] == "" {
		t.Fatalf("忙完之后的下一轮要立即补试: %v", st)
	}
	if msgs := sent(); len(msgs) != 1 || !strings.Contains(msgs[0], "自动续期失败") {
		t.Fatalf("补试失败应通知一次: %v", msgs)
	}
}

// 续期失败要落库:原因、连续失败次数、下次重试时间;重启后照样在,马上再扫一轮也不会重试(按退避来)。
func TestRenewDueFailurePersisted(t *testing.T) {
	r := certTestRunner(t)
	c, k := le90(t, filepath.Join(r.DataDir(), "cert"), "hk.example.com", 23*day+time.Hour)
	if err := r.saveCertRow(model.Cert{Name: "hk.example.com", Source: "acme", CertFile: c, KeyFile: k, Method: acme.MethodCloudflare, AutoRenew: true}); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Unix()
	r.renewDue()
	r.renewDue() // 退避期内不重试
	r2 := reopen(t, r)
	got := r2.Certs()
	if len(got) != 1 {
		t.Fatalf("证书库: %+v", got)
	}
	v := got[0]
	if v.LastRenewAt < before || !strings.Contains(v.LastRenewErr, "Cloudflare") || v.RenewFails != 1 || v.NextRenewAt < before+3600 {
		t.Fatalf("重启后应还能看到这次失败(1 次,至少一小时后重试): %+v", v.Cert)
	}
	if !v.Renewable || !v.Overdue || v.NextCheckAt != v.NextRenewAt {
		t.Fatalf("证书库列表要说清楚:能自动续、该续了、下次什么时候试: %+v", v.RenewState)
	}
	b, _ := json.Marshal(v)
	for _, key := range []string{`"lastRenewErr"`, `"renewFails"`, `"nextRenewAt"`, `"renewable"`, `"renewBefore"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("证书库接口里要带上 %s: %s", key, b)
		}
	}
}

// 自动续期的那一行日志不能被这次签发自己清掉:失败后签发日志仍以「自动续期 xxx:剩余 N 天」开头。
func TestAutoRenewLogLineKept(t *testing.T) {
	r := certTestRunner(t)
	c, k := le90(t, filepath.Join(r.DataDir(), "cert"), "hk.example.com", 23*day+time.Hour)
	if err := r.saveCertRow(model.Cert{Name: "hk.example.com", Source: "acme", CertFile: c, KeyFile: k, Method: acme.MethodCloudflare, AutoRenew: true}); err != nil {
		t.Fatal(err)
	}
	r.renewDue()
	log, _ := r.CertStatus()["log"].([]string)
	if len(log) < 2 || !strings.Contains(log[0], "自动续期 hk.example.com:剩余 23 天") || !strings.Contains(strings.Join(log, "\n"), "失败") {
		t.Fatalf("签发日志应以自动续期那行开头、后面是失败原因: %q", log)
	}
}

// 手动 DNS 的证书不能自动续期:从剩 30 天起每天明说一次(以前剩 14 天才提醒,剩 23 天时一声不吭);
// 剩 7 / 3 / 1 天各升级一次。去重记录落库,重启不重复。
func TestManualCertRemindersFrom30Days(t *testing.T) {
	r := certTestRunner(t)
	sent := notices(t, r)
	dir := filepath.Join(r.DataDir(), "cert")
	c, k := le90(t, dir, "*.example.com", 31*day+time.Hour)
	if err := r.saveCertRow(model.Cert{Name: "*.example.com", Source: "acme", CertFile: c, KeyFile: k, Method: acme.MethodManual}); err != nil {
		t.Fatal(err)
	}
	if err := saveSettings(r.db, map[string]string{"certFile": c, "keyFile": k}); err != nil { // 线路默认证书:正用着
		t.Fatal(err)
	}
	r.renewDue()
	if msgs := sent(); len(msgs) != 0 {
		t.Fatalf("剩 31 天还不用提醒: %v", msgs)
	}
	if st := r.Certs()[0]; st.Renewable || st.Reason != "manual" {
		t.Fatalf("证书页要标明手动 DNS、不能自动续期: %+v", st.RenewState)
	}
	le90(t, dir, "*.example.com", 30*day+time.Hour)
	r.renewDue()
	msgs := sent()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "不能自动续期") || !strings.Contains(msgs[0], "30 天") {
		t.Fatalf("剩 30 天应明说不能自动续期: %v", msgs)
	}
	r.renewDue()
	if msgs := sent(); len(msgs) != 1 {
		t.Fatalf("同一天不重复提醒: %v", msgs)
	}
	r2 := reopen(t, r)
	sent2 := notices(t, r2)
	r2.renewDue()
	if msgs := sent2(); len(msgs) != 0 {
		t.Fatalf("重启后同一天也不重复: %v", msgs)
	}
	le90(t, dir, "*.example.com", 7*day+time.Hour)
	r.renewDue()
	msgs = sent()
	if len(msgs) != 2 || !strings.Contains(msgs[1], "7 天") {
		t.Fatalf("剩 7 天要升级再提醒一次: %v", msgs)
	}
	le90(t, dir, "*.example.com", day+time.Hour)
	r.renewDue()
	if msgs := sent(); len(msgs) != 3 || !strings.Contains(msgs[2], "1 天") {
		t.Fatalf("剩 1 天要再提醒: %v", msgs)
	}
}

// 不归面板续的证书也要提醒:服务器上已有的(certbot 等续)、关了自动续期的,剩 14 天起提醒;证书文件读不出来的马上提醒。
func TestExternalDisabledMissingCertsWarned(t *testing.T) {
	r := certTestRunner(t)
	sent := notices(t, r)
	ec, ek := le90(t, t.TempDir(), "ext.example.com", 5*day+time.Hour)
	fc, fk := le90(t, t.TempDir(), "far.example.com", 20*day+time.Hour)
	ic, ik := le90(t, t.TempDir(), "idle.example.com", 5*day+time.Hour)
	oc, okey := le90(t, filepath.Join(r.DataDir(), "cert"), "off.example.com", 10*day+time.Hour)
	for _, row := range []model.Cert{
		{Name: "ext.example.com", Source: "external", CertFile: ec, KeyFile: ek},
		{Name: "far.example.com", Source: "external", CertFile: fc, KeyFile: fk},
		{Name: "idle.example.com", Source: "external", CertFile: ic, KeyFile: ik},
		{Name: "off.example.com", Source: "acme", Method: acme.MethodHTTP, CertFile: oc, KeyFile: okey},
		{Name: "gone.example.com", Source: "acme", Method: acme.MethodHTTP, AutoRenew: true, CertFile: filepath.Join(r.DataDir(), "cert", "gone.example.com.crt"), KeyFile: filepath.Join(r.DataDir(), "cert", "gone.example.com.key")},
	} {
		if err := r.saveCertRow(row); err != nil {
			t.Fatal(err)
		}
	}
	// 线路单独选了它们(正用着);idle 没人用,快到期了也不用提醒(证书页上照样看得到)
	for i, name := range []string{"ext.example.com", "far.example.com", "off.example.com", "gone.example.com"} {
		tls, _ := json.Marshal(map[string]string{"mode": "cert", "cert": name})
		r.db.Create(&model.Line{Name: fmt.Sprintf("l%d", i), Protocol: "trojan", Port: 41000 + i, Enabled: true, Tls: tls})
	}
	r.renewDue()
	msgs := sent()
	all := strings.Join(msgs, "\n")
	if len(msgs) != 3 || !strings.Contains(all, "ext.example.com") || !strings.Contains(all, "off.example.com") || !strings.Contains(all, "gone.example.com") {
		t.Fatalf("外部证书、关了自动续期的、文件读不出来的各提醒一次: %v", msgs)
	}
	if strings.Contains(all, "far.example.com") || strings.Contains(all, "idle.example.com") {
		t.Fatalf("剩 20 天的、没人用的还不用提醒: %v", msgs)
	}
	reasons := map[string]string{}
	for _, c := range r.Certs() {
		reasons[c.Name] = c.Reason
	}
	if reasons["ext.example.com"] != "external" || reasons["off.example.com"] != "disabled" || reasons["gone.example.com"] != "missing" {
		t.Fatalf("证书页要说清楚为什么不能自动续: %v", reasons)
	}
	r.renewDue()
	if msgs := sent(); len(msgs) != 3 {
		t.Fatalf("同一天不重复提醒: %v", msgs)
	}
}

// 启动后几分钟就先查一次(以前要等进程连续跑满 12 小时,中间一键更新、重启一次就又从头等)。
func TestCertLoopChecksSoonAfterStart(t *testing.T) {
	first, every := certFirstCheck, certCheckEvery
	certFirstCheck, certCheckEvery = 50*time.Millisecond, time.Hour
	defer func() { certFirstCheck, certCheckEvery = first, every }()
	r := certTestRunner(t)
	c, k := le90(t, filepath.Join(r.DataDir(), "cert"), "hk.example.com", 23*day+time.Hour)
	if err := r.saveCertRow(model.Cert{Name: "hk.example.com", Source: "acme", CertFile: c, KeyFile: k, Method: acme.MethodCloudflare, AutoRenew: true}); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { r.certLoop(stop); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for r.CertStatus()["lastTime"].(int64) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	<-done
	if r.CertStatus()["lastTime"].(int64) == 0 {
		t.Fatal("启动后很快就该查一次并尝试续期")
	}
}

// testCA 最小的 ACME 服务(不验签名):够走完 下单 → 验证 → 签发;签出的证书由 R11 中间证书签,和真的一样不是自签。
// hits 记收到的请求数。
type testCA struct {
	srv      *httptest.Server
	issuer   *testIssuer
	validity time.Duration
	mu       sync.Mutex
	hits     int
	order    []string
	accepted map[string]bool
	csr      *x509.CertificateRequest
}

func newTestCA(t *testing.T) *testCA {
	ca := &testCA{issuer: newIssuer(t, "R11"), validity: 90 * day, accepted: map[string]bool{}}
	ca.srv = httptest.NewServer(http.HandlerFunc(ca.handle))
	t.Cleanup(ca.srv.Close)
	return ca
}

func (ca *testCA) url(p string) string { return ca.srv.URL + p }

func (ca *testCA) requests() int {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	return ca.hits
}

func jwsPayload(r *http.Request) []byte {
	var env struct {
		Payload string `json:"payload"`
	}
	_ = json.NewDecoder(r.Body).Decode(&env)
	b, _ := base64.RawURLEncoding.DecodeString(env.Payload)
	return b
}

func (ca *testCA) handle(w http.ResponseWriter, r *http.Request) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	ca.hits++
	w.Header().Set("Replay-Nonce", fmt.Sprintf("nonce%d", ca.hits))
	switch {
	case r.URL.Path == "/dir":
		json.NewEncoder(w).Encode(map[string]string{"newNonce": ca.url("/nonce"), "newAccount": ca.url("/acct"), "newOrder": ca.url("/order")})
	case r.URL.Path == "/nonce":
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/acct":
		w.Header().Set("Location", ca.url("/acct/1"))
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"status":"valid"}`))
	case r.URL.Path == "/order":
		var body struct {
			Identifiers []struct{ Type, Value string }
		}
		json.Unmarshal(jwsPayload(r), &body)
		ca.order = nil
		auths := []string{}
		for i, id := range body.Identifiers {
			ca.order = append(ca.order, id.Value)
			auths = append(auths, ca.url(fmt.Sprintf("/authz/%d", i)))
		}
		w.Header().Set("Location", ca.url("/order/1"))
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "pending", "authorizations": auths, "finalize": ca.url("/finalize"), "identifiers": body.Identifiers})
	case strings.HasPrefix(r.URL.Path, "/authz/"):
		var i int
		fmt.Sscanf(r.URL.Path, "/authz/%d", &i)
		value := ca.order[i]
		chals := []map[string]string{{"type": "dns-01", "url": ca.url(fmt.Sprintf("/chal/%d", i)), "token": fmt.Sprintf("tok%d", i), "status": "pending"}}
		if !strings.HasPrefix(value, "*.") {
			chals = append(chals, map[string]string{"type": "http-01", "url": ca.url(fmt.Sprintf("/chal/%d", i)), "token": fmt.Sprintf("tok%d", i), "status": "pending"})
		}
		status := "pending"
		if ca.accepted[value] {
			status = "valid"
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "challenges": chals,
			"identifier": map[string]string{"type": "dns", "value": strings.TrimPrefix(value, "*.")}})
	case strings.HasPrefix(r.URL.Path, "/chal/"):
		var i int
		fmt.Sscanf(r.URL.Path, "/chal/%d", &i)
		ca.accepted[ca.order[i]] = true
		json.NewEncoder(w).Encode(map[string]string{"type": "dns-01", "status": "pending", "url": ca.url(r.URL.Path), "token": "t"})
	case r.URL.Path == "/finalize":
		var body struct {
			CSR string `json:"csr"`
		}
		json.Unmarshal(jwsPayload(r), &body)
		der, _ := base64.RawURLEncoding.DecodeString(body.CSR)
		ca.csr, _ = x509.ParseCertificateRequest(der)
		w.Header().Set("Location", ca.url("/order/1"))
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "valid", "certificate": ca.url("/cert"), "finalize": ca.url("/finalize"), "authorizations": []string{}, "identifiers": []string{}})
	case r.URL.Path == "/order/1":
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "valid", "certificate": ca.url("/cert"), "finalize": ca.url("/finalize")})
	case r.URL.Path == "/cert":
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: ca.csr.DNSNames[0]}, DNSNames: ca.csr.DNSNames,
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(ca.validity)}
		der, _ := x509.CreateCertificate(rand.Reader, tpl, ca.issuer.cert, key.Public(), ca.issuer.key)
		w.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		w.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.issuer.der}))
	default:
		http.NotFound(w, r)
	}
}

// HTTP 验证要临时占 80 端口:被线路占着时续期前就要发现,点名是哪条线路,不去打扰 CA
// (以前每天向 CA 走一遍流程,最后只报"监听 80 端口失败")。UDP 的 hy2 线路不占 TCP 80,不点它的名。
func TestHTTP01RenewFailsFastWhenPort80Taken(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	old := http01Addr
	http01Addr = fmt.Sprintf(":%d", port)
	defer func() { http01Addr = old }()

	r := certTestRunner(t)
	ca := newTestCA(t)
	c, k := le90(t, filepath.Join(r.DataDir(), "cert"), "hk.example.com", 23*day+time.Hour)
	if err := r.saveCertRow(model.Cert{Name: "hk.example.com", Source: "acme", CertFile: c, KeyFile: k, Method: acme.MethodHTTP, AutoRenew: true}); err != nil {
		t.Fatal(err)
	}
	r.db.Create(&model.Line{Name: "HY2-80", Protocol: "hysteria2", Port: port, Enabled: true})
	r.db.Create(&model.Line{Name: "CDN-80", Protocol: "vmess", Port: port, Enabled: true})
	req := IssueRequest{Domain: "hk.example.com", Method: acme.MethodHTTP, AutoRenew: true, directoryURL: ca.url("/dir")}
	err = r.runIssue(req)
	if err == nil || !strings.Contains(err.Error(), "CDN-80") || strings.Contains(err.Error(), "HY2-80") {
		t.Fatalf("应点名占着 TCP 端口的那条线路: %v", err)
	}
	if n := ca.requests(); n != 0 {
		t.Fatalf("80 端口被占时不该去找 CA,实际请求了 %d 次", n)
	}
	// 没有线路占,是别的程序占着:也要说清楚
	r.db.Where("name = ?", "CDN-80").Delete(&model.Line{})
	if err := r.runIssue(req); err == nil || !strings.Contains(err.Error(), "其它程序") {
		t.Fatalf("别的程序占着 80 端口也要说清楚: %v", err)
	}
	if n := ca.requests(); n != 0 {
		t.Fatalf("80 端口被占时不该去找 CA,实际请求了 %d 次", n)
	}
}

// 同名证书又「添加服务器上已有的证书」(比如从主机拷来的、certbot 的):不能悄悄把自动续期关掉。
func TestUseExternalCertRefusesToShadowAutoRenewACME(t *testing.T) {
	r := certTestRunner(t)
	c, k := le90(t, filepath.Join(r.DataDir(), "cert"), "hk.example.com", 80*day)
	if err := r.saveCertRow(model.Cert{Name: "hk.example.com", Source: "acme", CertFile: c, KeyFile: k, Method: acme.MethodHTTP, AutoRenew: true}); err != nil {
		t.Fatal(err)
	}
	ec, ek := le90(t, t.TempDir(), "hk.example.com", 80*day)
	err := r.UseExternalCert(ec, ek, CertTargets{})
	if err == nil || !strings.Contains(err.Error(), "自动续期") {
		t.Fatalf("同名证书开着自动续期,导入外部证书要先拒绝并说明: %v", err)
	}
	var row model.Cert
	r.db.Where("name = ?", "hk.example.com").First(&row)
	if row.Source != "acme" || !row.AutoRenew || row.CertFile != c {
		t.Fatalf("拒绝时证书库不能变: %+v", row)
	}
	// 确认之后照样换,那张的自动续期随之关掉,日志里写明
	if err := r.UseExternalCertForce(ec, ek, CertTargets{}); err != nil {
		t.Fatal(err)
	}
	r.db.Where("name = ?", "hk.example.com").First(&row)
	if row.Source != "external" || row.AutoRenew || row.CertFile != ec {
		t.Fatalf("确认后应换成服务器上的证书: %+v", row)
	}
	if log := strings.Join(r.CertStatus()["log"].([]string), "\n"); !strings.Contains(log, "不再自动续期") {
		t.Fatalf("日志里要写明不再自动续期: %s", log)
	}
	// 同名的是手动 DNS 证书(本来就不自动续):直接换,不用确认
	mc, mk := le90(t, filepath.Join(r.DataDir(), "cert"), "m.example.com", 80*day)
	if err := r.saveCertRow(model.Cert{Name: "m.example.com", Source: "acme", CertFile: mc, KeyFile: mk, Method: acme.MethodManual}); err != nil {
		t.Fatal(err)
	}
	if mec, mek := le90(t, t.TempDir(), "m.example.com", 80*day); r.UseExternalCert(mec, mek, CertTargets{}) != nil {
		t.Fatal("同名证书没开自动续期时不用确认")
	}
}

// certbot 等外部工具续过的证书:本机服务器记录里的到期时间要跟着更新(线路编辑框显示的剩余天数用它),
// 以前只在证书操作时才刷新,一直显示旧的。
func TestSyncLocalCertsRefreshesExternalNotAfter(t *testing.T) {
	r := certTestRunner(t)
	dir := t.TempDir()
	ec, ek := le90(t, dir, "ext.example.com", 20*day)
	if err := r.UseExternalCert(ec, ek, CertTargets{}); err != nil {
		t.Fatal(err)
	}
	le90(t, dir, "ext.example.com", 89*day) // certbot 原地覆盖
	r.renewDue()
	var local model.Node
	if err := r.db.Where("is_local = ?", true).First(&local).Error; err != nil {
		t.Fatal(err)
	}
	var briefs []model.CertBrief
	if err := json.Unmarshal(local.Certs, &briefs); err != nil || len(briefs) != 1 {
		t.Fatalf("本机证书清单不对: %s %v", local.Certs, err)
	}
	if left := time.Until(time.Unix(briefs[0].NotAfter, 0)); left < 88*day {
		t.Fatalf("外部工具续过之后到期时间要跟着更新,还是旧的: 剩 %v", left)
	}
}

// fakeCloudflare 假的 Cloudflare API:只有 example.com 一个区域,TXT 记录增删都答应。
func fakeCloudflare(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "example.com" {
				w.Write([]byte(`{"success":true,"result":[{"id":"z1","name":"example.com"}]}`))
			} else {
				w.Write([]byte(`{"success":true,"result":[]}`))
			}
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"success":true,"result":{"id":"rec1"}}`))
		default:
			w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// renewable 一台真能续上的:hk.example.com 用 Cloudflare 验证、开着自动续期、是线路默认证书,还剩 left;
// 签发走假 CA 和假 Cloudflare API。
func renewable(t *testing.T, left time.Duration) (*Runner, *testCA, func() []string) {
	t.Helper()
	r := certTestRunner(t)
	sent := notices(t, r)
	ca, cf := newTestCA(t), fakeCloudflare(t)
	r.cert.acmeHook = func(c *acme.Config) {
		c.DirectoryURL, c.CFBaseURL = ca.url("/dir"), cf.URL
		c.TXTLookup = func(context.Context, string, string) bool { return true }
	}
	c, k := le90(t, filepath.Join(r.DataDir(), "cert"), "hk.example.com", left)
	if err := r.saveCertRow(model.Cert{Name: "hk.example.com", Source: "acme", CertFile: c, KeyFile: k, Method: acme.MethodCloudflare, AutoRenew: true}); err != nil {
		t.Fatal(err)
	}
	if err := saveSettings(r.db, map[string]string{"acmeCfToken": "tok", "certFile": c, "keyFile": k, "webDomain": "hk.example.com"}); err != nil {
		t.Fatal(err)
	}
	return r, ca, sent
}

func certRow(t *testing.T, r *Runner, name string) model.Cert {
	t.Helper()
	var c model.Cert
	if err := r.db.Where("name = ?", name).First(&c).Error; err != nil {
		t.Fatal(err)
	}
	return c
}

// 全流程:进了续期窗口 → 自动续期 → 证书文件换成新签的 → 证书库、报给主机的清单、本机服务器记录都跟着变 → 通知一声;
// 续上之后不在窗口里了,再查一轮不会再签。
func TestAutoRenewEndToEnd(t *testing.T) {
	r, ca, sent := renewable(t, 23*day+time.Hour)
	r.renewDue()
	c := certRow(t, r, "hk.example.com")
	if info := acme.Info(c.CertFile); time.Until(info.NotAfter) < 89*day || !info.IsLetsEncrypt() {
		t.Fatalf("证书文件应换成新签的(90 天): %+v", info)
	}
	if c.RenewFails != 0 || c.LastRenewErr != "" || c.LastRenewOK == 0 || c.NextRenewAt != 0 || c.Source != "acme" || c.Method != acme.MethodCloudflare || !c.AutoRenew {
		t.Fatalf("续上之后的状态不对: %+v", c)
	}
	if b := r.CertBriefs(); len(b) != 1 || time.Until(time.Unix(b[0].NotAfter, 0)) < 89*day || !b[0].InUse || !b[0].AutoRenew || b[0].RenewErr != "" {
		t.Fatalf("报给主机的清单要跟着更新: %+v", b)
	}
	var local model.Node
	r.db.Where("is_local = ?", true).First(&local)
	var briefs []model.CertBrief
	if json.Unmarshal(local.Certs, &briefs) != nil || len(briefs) != 1 || time.Until(time.Unix(briefs[0].NotAfter, 0)) < 89*day {
		t.Fatalf("本机服务器记录里的证书清单要跟着更新: %s", local.Certs)
	}
	if msgs := sent(); len(msgs) != 1 || !strings.Contains(msgs[0], "已自动续期") || !strings.Contains(msgs[0], "hk.example.com") {
		t.Fatalf("续上了要通知一声: %v", msgs)
	}
	hits := ca.requests()
	r.renewDue()
	if ca.requests() != hits {
		t.Fatal("续上之后不该再签")
	}
}

// 自动续期和证书库的「续期」按钮都按这张证书自己的设置续,不改签发表单的全局默认
// (以前续一张证书就把表单里的域名、验证方式、自动续期勾选全改成了它的)。
func TestAutoRenewDoesNotTouchGlobalSettings(t *testing.T) {
	r, _, _ := renewable(t, 23*day+time.Hour)
	globals := map[string]string{"acmeDomain": "form.example.com", "acmeMethod": "http", "acmeStaging": "false", "acmeAutoRenew": "false"}
	if err := r.SetSettings(globals); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		for k, v := range globals {
			if got := r.setting(k); got != v {
				t.Fatalf("%s改了全局设置 %s: %q → %q", when, k, v, got)
			}
		}
		if c := certRow(t, r, "hk.example.com"); c.Method != acme.MethodCloudflare || !c.AutoRenew || c.RenewFails != 0 {
			t.Fatalf("%s后这张证书的设置不该变: %+v", when, c)
		}
	}
	r.renewDue()
	check("自动续期")
	if err := r.RenewCert("hk.example.com"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for r.CertStatus()["running"] == true && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := r.CertStatus(); st["running"] == true || st["lastError"] != "" {
		t.Fatalf("手点续期应成功: %v", st)
	}
	check("手点续期")
	if err := r.RenewCert("ext.example.com"); err == nil {
		t.Fatal("库里没有的证书不能续")
	}
}

// 连续失败按 1、2、4 小时退避,最长 6 小时;剩不到 7 天时最长 2 小时且每次失败都通知;平时第一次失败马上通知,
// 之后一天一次;修好后下一次重试续上,清零并说"已恢复"。
func TestRenewDueBackoffAndRecovery(t *testing.T) {
	r, _, sent := renewable(t, 23*day+time.Hour)
	if err := r.SetSettings(map[string]string{"acmeCfToken": ""}); err != nil { // Token 被删了:续不上
		t.Fatal(err)
	}
	const name = "hk.example.com"
	retry := func() { // 退避期过了
		r.db.Model(&model.Cert{}).Where("name = ?", name).Update("next_renew_at", time.Now().Unix()-1)
	}
	gap := func(c model.Cert) time.Duration { return time.Duration(c.NextRenewAt-c.LastRenewAt) * time.Second }
	for i, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 6 * time.Hour, 6 * time.Hour} {
		if i > 0 {
			r.renewDue()
			if c := certRow(t, r, name); c.RenewFails != i {
				t.Fatalf("退避期内不该重试: %+v", c)
			}
			retry()
		}
		r.renewDue()
		c := certRow(t, r, name)
		if c.RenewFails != i+1 || gap(c) < want || gap(c) > want+want/10+time.Second {
			t.Fatalf("第 %d 次失败后应隔 %v 再试,得 %v(%+v)", i+1, want, gap(c), c)
		}
	}
	if msgs := sent(); len(msgs) != 1 || !strings.Contains(msgs[0], "自动续期失败") || !strings.Contains(msgs[0], "Cloudflare") {
		t.Fatalf("第一次失败马上通知,之后一天一次: %v", msgs)
	}
	le90(t, filepath.Join(r.DataDir(), "cert"), name, 5*day+time.Hour)
	retry()
	r.renewDue()
	if c := certRow(t, r, name); c.RenewFails != 6 || gap(c) < 2*time.Hour || gap(c) > 2*time.Hour+12*time.Minute+time.Second {
		t.Fatalf("剩不到 7 天时最长隔 2 小时: %v %+v", gap(c), c)
	}
	if msgs := sent(); len(msgs) != 2 || !strings.Contains(msgs[1], "6 次") {
		t.Fatalf("剩不到 7 天时每次失败都要通知: %v", msgs)
	}
	if err := r.SetSettings(map[string]string{"acmeCfToken": "tok"}); err != nil { // 修好了
		t.Fatal(err)
	}
	retry()
	r.renewDue()
	if c := certRow(t, r, name); c.RenewFails != 0 || c.LastRenewErr != "" || c.NextRenewAt != 0 || c.LastRenewOK == 0 {
		t.Fatalf("续上之后要清零: %+v", c)
	}
	if msgs := sent(); len(msgs) != 3 || !strings.Contains(msgs[2], "已恢复") {
		t.Fatalf("从失败中恢复要说一声: %v", msgs)
	}
}

// 退避的时间表:抖动只往后加,间隔不低于一小时。
func TestRenewBackoffSchedule(t *testing.T) {
	for _, c := range []struct {
		fails int
		left  time.Duration
		want  time.Duration
	}{
		{1, 30 * day, time.Hour}, {2, 30 * day, 2 * time.Hour}, {3, 30 * day, 4 * time.Hour}, {4, 30 * day, 6 * time.Hour}, {9, 30 * day, 6 * time.Hour},
		{1, 5 * day, time.Hour}, {2, 5 * day, 2 * time.Hour}, {5, 5 * day, 2 * time.Hour}, {3, -day, 2 * time.Hour},
	} {
		for i := 0; i < 20; i++ {
			if got := renewBackoff(c.fails, c.left); got < c.want || got > c.want+c.want/10 {
				t.Fatalf("第 %d 次失败、剩 %v:应隔 %v(至多多一成),得 %v", c.fails, c.left, c.want, got)
			}
		}
	}
}

// 续期时机按有效期算:90 天的剩 30 天续,64 天的剩约 21 天,45 天的剩 15 天;几天的短证书刚续上不会马上又进窗口
// (写死"剩 30 天"的话,6 天的证书每小时都要重签一次)。
func TestRenewThresholdByLifetime(t *testing.T) {
	now := time.Now()
	cert := func(life, left time.Duration) acme.CertInfo {
		return acme.CertInfo{Exists: true, NotBefore: now.Add(left - life), NotAfter: now.Add(left)}
	}
	for _, c := range []struct{ life, before time.Duration }{
		{90 * day, 30 * day}, {64 * day, 64 * day / 3}, {45 * day, 15 * day}, {6 * day, 2 * day}, {365 * day, 30 * day},
	} {
		if got := renewBefore(cert(c.life, c.life)); got != c.before {
			t.Errorf("有效期 %v 的证书应提前 %v 续,得 %v", c.life, c.before, got)
		}
	}
	if got := renewBefore(acme.CertInfo{Exists: true, NotAfter: now.Add(day)}); got != 30*day {
		t.Errorf("有效期读不出来按 30 天,得 %v", got)
	}
	row := model.Cert{Source: "acme", Method: acme.MethodHTTP, AutoRenew: true}
	due := func(life, left time.Duration) bool { return renewStateOf(row, cert(life, left), now).Overdue }
	for _, c := range []struct {
		life, notYet, due time.Duration
	}{
		{90 * day, 31 * day, 30 * day}, {64 * day, 22 * day, 21 * day}, {45 * day, 16 * day, 15 * day}, {6 * day, 5 * day, 2 * day},
	} {
		if due(c.life, c.notYet) || !due(c.life, c.due) {
			t.Errorf("有效期 %v:剩 %v 还不用续、剩 %v 该续了", c.life, c.notYet, c.due)
		}
	}
	if due(6*day, 6*day) {
		t.Error("刚续上的 6 天证书不该马上又进续期窗口")
	}
	row.NextRenewAt = now.Add(3 * time.Hour).Unix()
	if st := renewStateOf(row, cert(90*day, 20*day), now); st.NextCheckAt != row.NextRenewAt || !st.Overdue {
		t.Errorf("退避期内下次检查时间要往后推: %+v", st)
	}
}

// 续期撞上正在进行的签发(比如有人在等手动 DNS 记录):那边一结束马上补跑,不等下一个钟头。
func TestRenewRetriesRightAfterRunningIssueEnds(t *testing.T) {
	first, every := certFirstCheck, certCheckEvery
	certFirstCheck, certCheckEvery = 50*time.Millisecond, time.Hour
	defer func() { certFirstCheck, certCheckEvery = first, every }()
	r := certTestRunner(t)
	c, k := le90(t, filepath.Join(r.DataDir(), "cert"), "hk.example.com", 23*day+time.Hour)
	if err := r.saveCertRow(model.Cert{Name: "hk.example.com", Source: "acme", CertFile: c, KeyFile: k, Method: acme.MethodCloudflare, AutoRenew: true}); err != nil {
		t.Fatal(err)
	}
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang }))
	defer srv.Close()
	defer close(hang)
	if err := r.IssueCert(IssueRequest{Domain: "other.example.com", Method: acme.MethodManual, directoryURL: srv.URL + "/dir"}); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { r.certLoop(stop); close(done) }()
	defer func() { close(stop); <-done }()
	time.Sleep(300 * time.Millisecond) // 第一轮检查撞上了那次签发
	if c := certRow(t, r, "hk.example.com"); c.RenewFails != 0 || c.LastRenewAt != 0 {
		t.Fatalf("撞上别的签发时不算一次尝试: %+v", c)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !r.CancelIssue() && time.Now().Before(deadline) { // 那边结束了
		time.Sleep(20 * time.Millisecond)
	}
	for certRow(t, r, "hk.example.com").RenewFails == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if c := certRow(t, r, "hk.example.com"); c.RenewFails != 1 {
		t.Fatalf("那边一结束就该补跑续期: %+v", c)
	}
}

// 关掉 / 打开自动续期:手动 DNS、服务器上已有的、自签的打不开,说清楚为什么;重新打开时不等退避。
func TestSetCertAutoRenew(t *testing.T) {
	r := certTestRunner(t)
	dir := filepath.Join(r.DataDir(), "cert")
	for _, row := range []model.Cert{
		{Name: "a.example.com", Source: "acme", Method: acme.MethodHTTP, AutoRenew: true, NextRenewAt: time.Now().Add(time.Hour).Unix(), RenewFails: 2},
		{Name: "*.example.com", Source: "acme", Method: acme.MethodManual},
		{Name: "ext.example.com", Source: "external"},
	} {
		row.CertFile, row.KeyFile = le90(t, dir, row.Name, 60*day)
		if err := r.saveCertRow(row); err != nil {
			t.Fatal(err)
		}
		r.db.Model(&model.Cert{}).Where("name = ?", row.Name).Updates(map[string]interface{}{"next_renew_at": row.NextRenewAt, "renew_fails": row.RenewFails})
	}
	if err := r.SetCertAutoRenew("a.example.com", false); err != nil || certRow(t, r, "a.example.com").AutoRenew {
		t.Fatalf("应能关掉: %v", err)
	}
	if err := r.SetCertAutoRenew("a.example.com", true); err != nil {
		t.Fatal(err)
	}
	if c := certRow(t, r, "a.example.com"); !c.AutoRenew || c.NextRenewAt != 0 {
		t.Fatalf("重新打开要不等退避: %+v", c)
	}
	if err := r.SetCertAutoRenew("*.example.com", true); err == nil || !strings.Contains(err.Error(), "手动 DNS") {
		t.Fatalf("手动 DNS 的打不开,要说为什么: %v", err)
	}
	if err := r.SetCertAutoRenew("ext.example.com", true); err == nil || !strings.Contains(err.Error(), "certbot") {
		t.Fatalf("服务器上已有的打不开,要说为什么: %v", err)
	}
}

// 报给主机的天数是正用着的证书里最少的那张(以前只看线路默认证书:面板、订阅、线路单独选的证书过期了主机看不出来)。
func TestCertDaysInUse(t *testing.T) {
	r := certTestRunner(t)
	dir := filepath.Join(r.DataDir(), "cert")
	dc, dk := le90(t, dir, "def.example.com", 60*day+time.Hour)
	pc, pk := le90(t, dir, "panel.example.com", 9*day+time.Hour)
	lc, lk := le90(t, dir, "line.example.com", 5*day+time.Hour)
	for _, row := range []model.Cert{
		{Name: "def.example.com", Source: "external", CertFile: dc, KeyFile: dk},
		{Name: "panel.example.com", Source: "external", CertFile: pc, KeyFile: pk},
		{Name: "line.example.com", Source: "external", CertFile: lc, KeyFile: lk},
	} {
		if err := r.saveCertRow(row); err != nil {
			t.Fatal(err)
		}
	}
	saveSettings(r.db, map[string]string{"certFile": dc, "keyFile": dk})
	if got := r.CertDaysInUse(); got != 60 {
		t.Fatalf("只有线路默认证书在用: 应是 60,得 %d", got)
	}
	saveSettings(r.db, map[string]string{"webCertFile": pc, "webKeyFile": pk})
	if got := r.CertDaysInUse(); got != 9 {
		t.Fatalf("面板用的那张更早到期: 应是 9,得 %d", got)
	}
	tls, _ := json.Marshal(map[string]string{"mode": "cert", "cert": "line.example.com"})
	r.db.Create(&model.Line{Name: "l", Protocol: "trojan", Port: 41000, Enabled: true, Tls: tls})
	if got := r.CertDaysInUse(); got != 5 {
		t.Fatalf("线路单独选的那张更早到期: 应是 5,得 %d", got)
	}
}

// 日报的证书一行、页面顶部的证书提示:本机的和副机报上来的一起算(副机一般没配通知,只能在主机上看见)。
func TestCertSummaryAndAlertsIncludeNodes(t *testing.T) {
	r := certTestRunner(t)
	c, k := le90(t, filepath.Join(r.DataDir(), "cert"), "*.example.com", 20*day+time.Hour)
	if err := r.saveCertRow(model.Cert{Name: "*.example.com", Source: "acme", CertFile: c, KeyFile: k, Method: acme.MethodManual}); err != nil {
		t.Fatal(err)
	}
	saveSettings(r.db, map[string]string{"certFile": c, "keyFile": k})
	now := time.Now()
	raw, _ := json.Marshal([]model.CertBrief{
		{Name: "sg.example.com", NotAfter: now.Add(20 * day).Unix(), Source: "acme", Method: acme.MethodHTTP, AutoRenew: true, InUse: true, RenewErr: "线路「CDN-80」占着 80 端口", RenewFails: 2},
		{Name: "ext.example.com", NotAfter: now.Add(10 * day).Unix(), Source: "external", InUse: true},
		{Name: "ok.example.com", NotAfter: now.Add(80 * day).Unix(), Source: "acme", Method: acme.MethodHTTP, AutoRenew: true, InUse: true},
	})
	r.db.Create(&model.Node{Name: "sg", ApiUrl: "https://sg:2053/x/", Token: "t", Certs: raw})
	if s := r.certSummary(); s.Expiring != 3 || s.Failing != 1 || s.Manual != 1 {
		t.Fatalf("证书汇总不对: %+v", s)
	}
	alerts := r.CertAlerts()
	got := map[string]string{}
	for _, a := range alerts {
		got[a.Node+"/"+a.Name] = a.Trouble
	}
	if len(alerts) != 3 || got["/*.example.com"] != "expiring" || got["sg/sg.example.com"] != "failing" || got["sg/ext.example.com"] != "expiring" {
		t.Fatalf("顶部提示不对: %+v", alerts)
	}
}
