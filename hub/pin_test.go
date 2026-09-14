package hub

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// selfSigned 生成一张自签证书。httptest 自带的那张所有服务器共用,测"证书换了"必须自己签两张不同的。
func selfSigned(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func tlsNode(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent-Token") != "tok" {
			http.Error(w, `{"error":"令牌错误"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":"1","version":"test"}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func fingerprint(c tls.Certificate) string {
	sum := sha256.Sum256(c.Certificate[0])
	return hex.EncodeToString(sum[:])
}

// 勾了"跳过证书校验"的副机:第一次连上记住指纹;证书换了就拒绝;重置指纹后重新信任。
func TestInsecureNodePinsCertificate(t *testing.T) {
	db := openDB(t, "pin.db").DB
	c1, c2 := selfSigned(t, "node-old"), selfSigned(t, "node-new")
	old, renewed := tlsNode(t, c1), tlsNode(t, c2)
	db.Create(&model.Node{Name: "az", ApiUrl: old.URL, Token: "tok", Insecure: true})
	h := New(Deps{DB: db})
	load := func() model.Node {
		var n model.Node
		if err := db.First(&n, 1).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 第一次:连上,记住指纹
	if _, err := h.Ping(load()); err != nil {
		t.Fatalf("首次连接应成功: %v", err)
	}
	if n := load(); n.CertFP != fingerprint(c1) {
		t.Fatalf("首次连接应记住证书指纹,得 %q", n.CertFP)
	}
	// 同一张证书再连:照常
	if _, err := h.Ping(load()); err != nil {
		t.Fatalf("同一张证书再连应成功: %v", err)
	}

	// 对端证书换了(重签,或者中间人):拒绝,并且指纹不被覆盖
	db.Model(&model.Node{}).Where("id = ?", 1).Update("api_url", renewed.URL)
	_, err := h.Ping(load())
	if err == nil || !strings.Contains(err.Error(), "指纹变了") {
		t.Fatalf("证书换了应被拒绝并说明原因,得 %v", err)
	}
	if n := load(); n.CertFP != fingerprint(c1) {
		t.Fatalf("被拒绝时不该改动记住的指纹,得 %q", n.CertFP)
	}

	// 「重置指纹」之后重新信任,记住新的
	db.Model(&model.Node{}).Where("id = ?", 1).Update("cert_fp", "")
	if _, err := h.Ping(load()); err != nil {
		t.Fatalf("重置指纹后应重新信任: %v", err)
	}
	if n := load(); n.CertFP != fingerprint(c2) {
		t.Fatalf("重置后应记住新证书的指纹,得 %q", n.CertFP)
	}

	// 没勾"跳过校验"的副机走正常校验:自签证书过不了,也绝不会去记指纹
	db.Model(&model.Node{}).Where("id = ?", 1).Updates(map[string]interface{}{"insecure": false, "cert_fp": ""})
	if _, err := h.Ping(load()); err == nil {
		t.Fatal("正常校验下自签证书应失败")
	}
	if n := load(); n.CertFP != "" {
		t.Fatalf("正常校验的副机不该记指纹,得 %q", n.CertFP)
	}
	h.CloseIdleConnections()
}

// 从来没连上过的副机也要告警:按"连续失败了多久"算,不看上次在线时间。
func TestAlertForNeverSeenNode(t *testing.T) {
	db := openDB(t, "alert.db").DB
	var got []string
	h := New(Deps{DB: db, Notify: func(_, text string) { got = append(got, text) }})
	n := model.Node{Id: 7, Name: "新副机"}

	h.setStatus(n, false, "dial tcp: i/o timeout", nil)
	if len(got) != 0 {
		t.Fatalf("刚失败不该立刻告警: %v", got)
	}
	h.mu.Lock()
	h.status[7].failSince -= 61 // 当作已经连续失败了一分钟
	h.mu.Unlock()
	h.setStatus(n, false, "dial tcp: i/o timeout", nil)
	if len(got) != 1 || !strings.Contains(got[0], "副机失联") {
		t.Fatalf("连续失败超过一分钟应告警一次,得 %v", got)
	}
	h.setStatus(n, false, "dial tcp: i/o timeout", nil)
	if len(got) != 1 {
		t.Fatalf("同一次失联只告警一次,得 %v", got)
	}
	h.setStatus(n, true, "", nil)
	if len(got) != 2 || !strings.Contains(got[1], "副机恢复") {
		t.Fatalf("恢复应通知一次,得 %v", got)
	}
	h.mu.Lock()
	since := h.status[7].failSince
	h.mu.Unlock()
	if since != 0 {
		t.Fatal("恢复后连续失败起点应清零")
	}
}
