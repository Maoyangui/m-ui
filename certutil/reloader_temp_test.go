package certutil

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func servedLeaf(t *testing.T, r *Reloader) *x509.Certificate {
	t.Helper()
	c, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// 证书文件不在:先用临时自签证书顶着(写进给的域名 / IP),说清楚为什么;文件放回去之后不用重启,几秒内换回来。
func TestReloaderOrTempFallsBackAndRecovers(t *testing.T) {
	old := reloadEvery
	reloadEvery = 50 * time.Millisecond
	t.Cleanup(func() { reloadEvery = old })
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "live", "fullchain.pem"), filepath.Join(dir, "live", "privkey.pem")

	r := NewReloaderOrTemp(crt, key, "hk.example.com", "", "1.2.3.4,hk.example.com", "[2001:db8::1]")
	err := r.Temp()
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("应报告在用临时证书、以及为什么: %v", err)
	}
	leaf := servedLeaf(t, r)
	if leaf.Issuer.String() != leaf.Subject.String() || len(leaf.Subject.Organization) == 0 || leaf.Subject.Organization[0] != "m-ui temporary" {
		t.Fatalf("顶上的应是临时自签证书: %s", leaf.Subject)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "hk.example.com" || len(leaf.IPAddresses) != 2 {
		t.Fatalf("临时证书里的地址不对: %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if NewReloaderOrTemp(crt, key).Temp() == nil || servedLeaf(t, NewReloaderOrTemp(crt, key)).Subject.CommonName != "localhost" {
		t.Fatal("没给地址时用 localhost")
	}

	if err := GenerateSelfSigned([]string{"hk.example.com"}, crt, key, 30); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := r.Temp(); err != nil {
		t.Fatalf("文件放回去以后应换回原来的证书: %v", err)
	}
	if leaf := servedLeaf(t, r); leaf.Subject.Organization[0] != "m-ui self-signed" {
		t.Fatalf("握手拿到的还是临时证书: %s", leaf.Subject)
	}
}

// 证书读得出来时和 NewReloader 一样;私钥配不上也算读不出来。
func TestReloaderOrTempUsesGoodFiles(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	if err := GenerateSelfSigned([]string{"a.example.com"}, crt, key, 30); err != nil {
		t.Fatal(err)
	}
	r := NewReloaderOrTemp(crt, key, "x.example.com")
	if err := r.Temp(); err != nil {
		t.Fatal(err)
	}
	if leaf := servedLeaf(t, r); leaf.DNSNames[0] != "a.example.com" {
		t.Fatalf("应该用配置的证书: %v", leaf.DNSNames)
	}
	other := filepath.Join(dir, "b.key")
	if err := GenerateSelfSigned([]string{"b.example.com"}, filepath.Join(dir, "b.crt"), other, 30); err != nil {
		t.Fatal(err)
	}
	if NewReloaderOrTemp(crt, other).Temp() == nil {
		t.Fatal("私钥和证书配不上,应当顶临时证书")
	}
}

func certFP(t *testing.T, r *Reloader) string {
	t.Helper()
	c, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(c.Certificate[0])
	return hex.EncodeToString(sum[:])
}

// 面板、订阅、代理面板的临时证书存进 TempDir、重启沿用同一张:副机面板顶着临时证书时,主机钉住的指纹以前每次重启都对不上,
// 照提示「重置指纹」后副机一重启又失联。各服务各一张;存着的不是临时证书、配不上、快过期的重新生成;
// 没设 TempDir(或 nil 的 Watch)只在内存里;证书读得出来时什么都不写。
func TestWatchReloaderKeepsTempCert(t *testing.T) {
	dir := t.TempDir()
	tempDir := filepath.Join(dir, "cert", "temp")
	gone, goneKey := filepath.Join(dir, "live", "fullchain.pem"), filepath.Join(dir, "live", "privkey.pem")
	w := &Watch{TempDir: tempDir}

	first := w.Reloader("panel", gone, goneKey, "a.example.com")
	if first.Temp() == nil {
		t.Fatal("证书文件不在,应顶着临时证书")
	}
	fp := certFP(t, first)
	if again := certFP(t, w.Reloader("panel", gone, goneKey, "a.example.com")); again != fp {
		t.Fatal("重启后临时证书换了,主机钉住的指纹会对不上")
	}
	if again := certFP(t, w.Reloader("panel", gone, goneKey, "b.example.com")); again != fp {
		t.Fatal("面板域名改了也沿用同一张:临时证书本来就不受信任,指纹不变更要紧")
	}
	if other := certFP(t, w.Reloader("sub", gone, goneKey, "a.example.com")); other == fp {
		t.Fatal("各服务各一张")
	}
	leaf := servedLeaf(t, first)
	if !IsTempCert(leaf) || leaf.DNSNames[0] != "a.example.com" || time.Until(leaf.NotAfter) < 300*24*time.Hour {
		t.Fatalf("存下来的应是写着面板域名、一年有效的临时证书: %s %v %v", leaf.Subject, leaf.DNSNames, leaf.NotAfter)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(tempDir, "panel.key")); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("临时证书的私钥应是 0600: %v %v", st, err)
		}
	}

	// 存着的被换成了别的证书(不是临时证书)、或者快过期了:重新生成一张临时证书,不拿它顶
	if err := GenerateSelfSigned([]string{"x.example.com"}, filepath.Join(tempDir, "panel.crt"), filepath.Join(tempDir, "panel.key"), 30); err != nil {
		t.Fatal(err)
	}
	if l := servedLeaf(t, w.Reloader("panel", gone, goneKey, "a.example.com")); !IsTempCert(l) {
		t.Fatalf("不是临时证书的不能拿来顶: %s", l.Subject)
	}
	certPEM, keyPEM, err := selfSignedPEM([]string{"a.example.com"}, 3, TempOrg)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveTemp(filepath.Join(tempDir, "panel"), certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	if l := servedLeaf(t, w.Reloader("panel", gone, goneKey, "a.example.com")); time.Until(l.NotAfter) < 300*24*time.Hour {
		t.Fatalf("只剩 3 天的临时证书应换一张新的: %v", l.NotAfter)
	}

	// 没设 TempDir、nil 的 Watch:只在内存里,不写文件
	other := filepath.Join(dir, "other")
	var nilWatch *Watch
	for _, r := range []*Reloader{nilWatch.Reloader("panel", gone, goneKey), (&Watch{}).Reloader("panel", gone, goneKey)} {
		if r.Temp() == nil || !IsTempCert(servedLeaf(t, r)) {
			t.Fatal("没设 TempDir 也照样顶临时证书")
		}
	}
	// 证书读得出来:和 NewReloader 一样,什么都不写
	good, goodKey := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	if err := GenerateSelfSigned([]string{"a.example.com"}, good, goodKey, 30); err != nil {
		t.Fatal(err)
	}
	if r := (&Watch{TempDir: other}).Reloader("panel", good, goodKey); r.Temp() != nil || IsTempCert(servedLeaf(t, r)) {
		t.Fatal("证书读得出来就用它")
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("用不着临时证书时不该建目录、写文件")
	}
	if IsTempCert(nil) {
		t.Fatal("nil 不是临时证书")
	}
}

// Watch:登记的服务里谁在顶临时证书,按面板、订阅、代理面板的顺序列出;停掉(登记 nil)就不再列;
// 登记时正在顶的,OnTemp 收到一次。nil 的 Watch 什么都不做。
func TestWatchListsTempCerts(t *testing.T) {
	dir := t.TempDir()
	good, goodKey := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	if err := GenerateSelfSigned([]string{"a.example.com"}, good, goodKey, 30); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "gone.crt")
	var mu sync.Mutex
	var got []string
	done := make(chan struct{}, 4)
	w := &Watch{OnTemp: func(tc TempCert) {
		mu.Lock()
		got = append(got, tc.Service)
		mu.Unlock()
		done <- struct{}{}
	}}
	w.Set("reseller", NewReloaderOrTemp(missing, goodKey))
	w.Set("sub", NewReloaderOrTemp(good, goodKey))
	w.Set("panel", NewReloaderOrTemp(missing, goodKey))
	list := w.Temps()
	if len(list) != 2 || list[0].Service != "panel" || list[1].Service != "reseller" || list[0].CertFile != missing || list[0].Err == "" || list[0].Since.IsZero() {
		t.Fatalf("应列出面板与代理面板: %+v", list)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("OnTemp 没被调用")
		}
	}
	mu.Lock()
	if len(got) != 2 {
		t.Fatalf("OnTemp 只该为顶着临时证书的服务调: %v", got)
	}
	mu.Unlock()
	w.Set("panel", nil)
	if list := w.Temps(); len(list) != 1 || list[0].Service != "reseller" {
		t.Fatalf("停掉的不再列: %+v", list)
	}
	var nilWatch *Watch
	nilWatch.Set("panel", NewReloaderOrTemp(missing, goodKey))
	if nilWatch.Temps() != nil {
		t.Fatal("nil 的 Watch 什么都不做")
	}
}
