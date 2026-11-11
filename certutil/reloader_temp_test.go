package certutil

import (
	"crypto/x509"
	"path/filepath"
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
