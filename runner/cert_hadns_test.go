package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/acme"
)

// 接任后经主机容灾的 DNS 接口给订阅域名签证书:不用证书页的 Cloudflare 令牌,用容灾设置里的服务商与凭据
// (演练时换成测试地址);签好用于订阅,证书库记为 hadns、能自动续期;面板域名空着也不拿订阅域名去填;
// 凭据没了报错说清去哪儿填。
func TestIssueViaHADNS(t *testing.T) {
	r := certTestRunner(t)
	ca := newTestCA(t)
	var mu sync.Mutex
	var txt, auth []string
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth = append(auth, q.Header.Get("Authorization"))
		switch {
		case q.Method == http.MethodGet && q.URL.Path == "/zones":
			if q.URL.Query().Get("name") == "example.com" {
				w.Write([]byte(`{"success":true,"result":[{"id":"z1","name":"example.com"}]}`))
			} else {
				w.Write([]byte(`{"success":true,"result":[]}`))
			}
		case q.Method == http.MethodPost:
			var b map[string]interface{}
			json.NewDecoder(q.Body).Decode(&b)
			txt = append(txt, fmt.Sprint(b["name"]))
			w.Write([]byte(`{"success":true,"result":{"id":"rec1"}}`))
		default:
			w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	t.Cleanup(cf.Close)
	r.ha.SetDNSAPI(cf.URL)
	r.cert.acmeHook = func(c *acme.Config) {
		c.DirectoryURL = ca.url("/dir")
		c.TXTLookup = func(context.Context, string, string) bool { return true }
	}
	if err := saveSettings(r.db, map[string]string{"haDnsProvider": "cloudflare", "haDnsToken": "ha-tok", "acmeCfToken": "", "webDomain": ""}); err != nil {
		t.Fatal(err)
	}
	req := IssueRequest{Domain: "sub.example.com", Method: MethodHADNS, AutoRenew: true, Renew: true, Targets: CertTargets{Sub: true, keepWeb: true}}
	if err := r.runIssue(req); err != nil {
		t.Fatal(err)
	}
	c := certRow(t, r, "sub.example.com")
	if c.Method != MethodHADNS || !c.AutoRenew || c.Source != "acme" {
		t.Fatalf("证书库记为经容灾 DNS 接口签的、自动续期: %+v", c)
	}
	if r.setting("subCertFile") != c.CertFile || r.setting("subKeyFile") != c.KeyFile {
		t.Fatalf("签好用于订阅: %s %s", r.setting("subCertFile"), r.setting("subKeyFile"))
	}
	if r.setting("webDomain") != "" {
		t.Fatalf("面板域名空着也不拿订阅域名去填: %q", r.setting("webDomain"))
	}
	mu.Lock()
	if len(txt) != 1 || txt[0] != "_acme-challenge.sub.example.com" {
		t.Fatalf("TXT 经容灾的 DNS 接口写: %v", txt)
	}
	for _, a := range auth {
		if a != "Bearer ha-tok" {
			t.Fatalf("用的是容灾的凭据: %v", auth)
		}
	}
	mu.Unlock()
	if st := r.renewState(c, acme.Info(c.CertFile), time.Now()); !st.Renewable {
		t.Fatalf("经容灾 DNS 接口签的能自动续期: %+v", st)
	}
	if err := saveSettings(r.db, map[string]string{"haDnsToken": ""}); err != nil {
		t.Fatal(err)
	}
	if err := r.runIssue(req); err == nil || !strings.Contains(err.Error(), "容灾设置") {
		t.Fatalf("凭据没了:报错说清去容灾设置里填: %v", err)
	}
}
