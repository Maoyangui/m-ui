package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCA 一个最小的 ACME 服务:不验签名,够走完 下单 → 验证 → 签发,并记下客户端做了什么。
type fakeCA struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex

	wildcard bool     // 是否泛域名订单
	accepted []string // Accept 过的挑战(按 authz 域名)
	lookedUp func() bool // 验证时看 TXT 是否都已写好
	failAuth bool     // 验证失败
	hang     bool     // 下单时一直挂着(模拟限流)
	csr      *x509.CertificateRequest
	order    []string
}

func newFakeCA(t *testing.T) *fakeCA {
	ca := &fakeCA{t: t}
	ca.srv = httptest.NewServer(http.HandlerFunc(ca.handle))
	t.Cleanup(ca.srv.Close)
	return ca
}

func (ca *fakeCA) url(p string) string { return ca.srv.URL + p }

func decodeJWS(r *http.Request) []byte {
	var env struct {
		Payload string `json:"payload"`
	}
	_ = json.NewDecoder(r.Body).Decode(&env)
	b, _ := base64.RawURLEncoding.DecodeString(env.Payload)
	return b
}

func (ca *fakeCA) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Replay-Nonce", "nonce")
	ca.mu.Lock()
	defer ca.mu.Unlock()
	switch {
	case r.URL.Path == "/dir":
		json.NewEncoder(w).Encode(map[string]string{
			"newNonce": ca.url("/nonce"), "newAccount": ca.url("/acct"), "newOrder": ca.url("/order"),
		})
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
		json.Unmarshal(decodeJWS(r), &body)
		ca.order = nil
		for _, id := range body.Identifiers {
			ca.order = append(ca.order, id.Value)
		}
		if ca.hang {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Header().Set("Retry-After", "1")
			w.Write([]byte(`{"type":"urn:ietf:params:acme:error:rateLimited","detail":"too many"}`))
			return
		}
		auths := []string{}
		for i := range ca.order {
			auths = append(auths, ca.url(fmt.Sprintf("/authz/%d", i)))
		}
		w.Header().Set("Location", ca.url("/order/1"))
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "pending", "authorizations": auths, "finalize": ca.url("/finalize"),
			"identifiers": body.Identifiers,
		})
	case strings.HasPrefix(r.URL.Path, "/authz/"):
		var i int
		fmt.Sscanf(r.URL.Path, "/authz/%d", &i)
		value := ca.order[i]
		wild := strings.HasPrefix(value, "*.")
		chals := []map[string]string{{"type": "dns-01", "url": ca.url(fmt.Sprintf("/chal/%d", i)), "token": fmt.Sprintf("tok%d", i), "status": "pending"}}
		if !wild { // CA 不给泛域名 http-01
			chals = append(chals, map[string]string{"type": "http-01", "url": ca.url(fmt.Sprintf("/chal/%d", i)), "token": fmt.Sprintf("tok%d", i), "status": "pending"})
		}
		status := "pending"
		if contains(ca.accepted, value) {
			status = "valid"
			if ca.failAuth {
				status = "invalid"
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": status, "wildcard": wild, "challenges": chals,
			"identifier": map[string]string{"type": "dns", "value": strings.TrimPrefix(value, "*.")},
		})
	case strings.HasPrefix(r.URL.Path, "/chal/"):
		var i int
		fmt.Sscanf(r.URL.Path, "/chal/%d", &i)
		if ca.lookedUp != nil && !ca.lookedUp() {
			ca.t.Errorf("TXT 还没全写好就让 CA 验证了")
		}
		ca.accepted = append(ca.accepted, ca.order[i])
		json.NewEncoder(w).Encode(map[string]string{"type": "dns-01", "status": "pending", "url": ca.url(r.URL.Path), "token": "t"})
	case r.URL.Path == "/finalize":
		var body struct {
			CSR string `json:"csr"`
		}
		json.Unmarshal(decodeJWS(r), &body)
		der, _ := base64.RawURLEncoding.DecodeString(body.CSR)
		ca.csr, _ = x509.ParseCertificateRequest(der)
		w.Header().Set("Location", ca.url("/order/1"))
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "valid", "certificate": ca.url("/cert"), "finalize": ca.url("/finalize"), "authorizations": []string{}, "identifiers": []string{}})
	case r.URL.Path == "/order/1":
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "valid", "certificate": ca.url("/cert"), "finalize": ca.url("/finalize")})
	case r.URL.Path == "/cert":
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now(), NotAfter: time.Now().Add(90 * 24 * time.Hour), DNSNames: ca.csr.DNSNames}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
		fmt.Fprintf(w, "-----BEGIN CERTIFICATE-----\n%s\n-----END CERTIFICATE-----\n", base64.StdEncoding.EncodeToString(der))
	default:
		http.NotFound(w, r)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func issueConfig(t *testing.T, ca *fakeCA, domain, method string) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{Domain: domain, Method: method, CertPath: filepath.Join(dir, "c.crt"), KeyPath: filepath.Join(dir, "c.key"),
		DirectoryURL: ca.url("/dir"), TXTLookup: func(context.Context, string, string) bool { return true }}
}

// 泛域名手动验证:记录名不带 *,连裸域名两条记录都给用户,都生效了才让 CA 验;证书里两个名字都有。
// 这几条就是 issue 里"申请泛域名卡住"的根因:记录名错了(_acme-challenge.*.example.com),CA 永远查不到。
func TestIssueWildcardManual(t *testing.T) {
	ca := newFakeCA(t)
	cfg := issueConfig(t, ca, "*.example.com", MethodManual)
	var shown []TXTRecord
	visible := map[string]bool{}
	cfg.OnTXT = func(recs []TXTRecord) {
		if recs != nil {
			shown = recs
		}
	}
	// 用户先加了一条,过一会儿才加第二条:两条都能查到之前不能继续
	calls := 0
	cfg.TXTLookup = func(_ context.Context, name, want string) bool {
		calls++
		if calls > 2 {
			for _, r := range shown {
				visible[r.Value] = true
			}
		}
		return visible[want]
	}
	ca.lookedUp = func() bool {
		for _, r := range shown {
			if !visible[r.Value] {
				return false
			}
		}
		return len(shown) == 2
	}
	res, err := Issue(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.NotAfter.IsZero() {
		t.Fatal("没拿到证书")
	}
	if len(shown) != 2 || shown[0].Name != "_acme-challenge.example.com" || shown[1].Name != "_acme-challenge.example.com" {
		t.Fatalf("记录名不对: %+v", shown)
	}
	if shown[0].Value == shown[1].Value {
		t.Fatalf("泛域名和裸域名的值应该不同: %+v", shown)
	}
	if fmt.Sprint(ca.order) != "[*.example.com example.com]" {
		t.Fatalf("订单要同时有泛域名和裸域名: %v", ca.order)
	}
	if ca.csr == nil || fmt.Sprint(ca.csr.DNSNames) != "[*.example.com example.com]" {
		t.Fatalf("CSR 要同时有两个名字: %+v", ca.csr)
	}
	if len(ca.accepted) != 2 {
		t.Fatalf("两个授权都要验: %v", ca.accepted)
	}
}

// 泛域名 + HTTP 验证:CA 根本不给 http-01,在发请求之前就要说清楚,不能等到超时。
func TestIssueWildcardRefusesHTTP(t *testing.T) {
	ca := newFakeCA(t)
	_, err := Issue(context.Background(), issueConfig(t, ca, "*.example.com", MethodHTTP))
	if err == nil || !strings.Contains(err.Error(), "泛域名只能用 DNS 验证") {
		t.Fatalf("应该直接拒绝: %v", err)
	}
	if len(ca.order) != 0 {
		t.Fatal("不该向 CA 下单")
	}
}

// 泛域名 + Cloudflare:记录写在根区域下,验完清理。
func TestIssueWildcardCloudflare(t *testing.T) {
	ca := newFakeCA(t)
	var mu sync.Mutex
	var created, deleted []string
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "example.com" {
				w.Write([]byte(`{"success":true,"result":[{"id":"z1","name":"example.com"}]}`))
			} else {
				w.Write([]byte(`{"success":true,"result":[]}`))
			}
		case r.Method == "POST":
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			created = append(created, body["name"].(string))
			fmt.Fprintf(w, `{"success":true,"result":{"id":"r%d"}}`, len(created))
		case r.Method == "DELETE":
			deleted = append(deleted, r.URL.Path)
			w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	defer cf.Close()
	cfg := issueConfig(t, ca, "*.example.com", MethodCloudflare)
	cfg.CFToken, cfg.CFBaseURL = "tok", cf.URL
	if _, err := Issue(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || created[0] != "_acme-challenge.example.com" || created[1] != "_acme-challenge.example.com" {
		t.Fatalf("TXT 记录不对: %v", created)
	}
	if len(deleted) != 2 {
		t.Fatalf("验完要清理: %v", deleted)
	}
}

// 手动验证等不到记录:到点要报错说清楚,不能一直转。
func TestIssueManualTimesOut(t *testing.T) {
	ca := newFakeCA(t)
	cfg := issueConfig(t, ca, "*.example.com", MethodManual)
	cfg.TXTLookup = func(context.Context, string, string) bool { return false }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Issue(ctx, cfg)
	if err == nil || !strings.Contains(err.Error(), "TXT") {
		t.Fatalf("等不到记录要报错: %v", err)
	}
}

// CA 一直限流:单步超时后报人话。
func TestIssueStepTimeoutMessage(t *testing.T) {
	err := stepErr(fmt.Errorf("创建订单: %w", context.DeadlineExceeded))
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("超时要说人话: %v", err)
	}
	plain := fmt.Errorf("创建订单: %w", fmt.Errorf("boom"))
	if stepErr(plain) != plain {
		t.Fatal("别的错误原样返回")
	}
}

// 普通域名 HTTP 验证照旧(订单只有一个名字)。
func TestIsWildcardAndChallengeName(t *testing.T) {
	if !IsWildcard("*.a.com") || IsWildcard("a.com") {
		t.Fatal("IsWildcard")
	}
	if BaseDomain("*.a.com") != "a.com" || BaseDomain("a.com") != "a.com" {
		t.Fatal("BaseDomain")
	}
	if challengeName("*.a.com") != "_acme-challenge.a.com" || challengeName("b.a.com") != "_acme-challenge.b.a.com" {
		t.Fatal("challengeName")
	}
}
