package acme

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/certutil"
)

func TestCloudflareZoneAndTXT(t *testing.T) {
	var created, deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"success":false,"errors":[{"message":"bad token"}]}`, 403)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "joinvip.vip" {
				w.Write([]byte(`{"success":true,"result":[{"id":"z1","name":"joinvip.vip"}]}`))
			} else {
				w.Write([]byte(`{"success":true,"result":[]}`))
			}
		case r.Method == "POST" && r.URL.Path == "/zones/z1/dns_records":
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			created = body["name"].(string) + "=" + body["content"].(string)
			w.Write([]byte(`{"success":true,"result":{"id":"r9"}}`))
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/zones/z1/dns_records/"):
			deleted = strings.TrimPrefix(r.URL.Path, "/zones/z1/dns_records/")
			w.Write([]byte(`{"success":true,"result":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cf := &Cloudflare{Token: "tok", BaseURL: srv.URL}
	ctx := context.Background()
	id, name, err := cf.ZoneFor(ctx, "hk.joinvip.vip")
	if err != nil || id != "z1" || name != "joinvip.vip" {
		t.Fatalf("zone: %v %s %s", err, id, name)
	}
	rid, err := cf.CreateTXT(ctx, id, "_acme-challenge.hk.joinvip.vip", "abc")
	if err != nil || rid != "r9" || created != "_acme-challenge.hk.joinvip.vip=abc" {
		t.Fatalf("create: %v %s %s", err, rid, created)
	}
	if err := cf.DeleteRecord(ctx, id, rid); err != nil || deleted != "r9" {
		t.Fatalf("delete: %v %s", err, deleted)
	}
	bad := &Cloudflare{Token: "nope", BaseURL: srv.URL}
	if _, _, err := bad.ZoneFor(ctx, "hk.joinvip.vip"); err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("坏 token 应报错: %v", err)
	}
}

func TestInfoSelfSigned(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	if err := certutil.GenerateSelfSigned([]string{"hk.example.com", "1.2.3.4"}, crt, key, 30); err != nil {
		t.Fatal(err)
	}
	ci := Info(crt)
	if !ci.Exists || !ci.SelfSigned || ci.DaysLeft < 28 || ci.DaysLeft > 30 {
		t.Fatalf("自签信息不符: %+v", ci)
	}
	if len(ci.DNSNames) != 1 || ci.DNSNames[0] != "hk.example.com" || len(ci.IPs) != 1 {
		t.Fatalf("SAN 不符: %+v", ci)
	}
	if missing := Info(filepath.Join(dir, "none.crt")); missing.Exists || missing.Error == "" {
		t.Fatalf("不存在的证书应报错: %+v", missing)
	}
}

func TestIssueRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	if _, err := Issue(ctx, Config{Domain: "1.2.3.4", CertPath: "a", KeyPath: "b"}); err == nil || !strings.Contains(err.Error(), "IP") {
		t.Fatalf("IP 应被拒: %v", err)
	}
	if _, err := Issue(ctx, Config{Domain: "x.com", Method: "cloudflare", CertPath: "a", KeyPath: "b"}); err == nil || !strings.Contains(err.Error(), "Token") {
		t.Fatalf("缺 Token 应被拒: %v", err)
	}
	if _, err := Issue(ctx, Config{Domain: "x.com"}); err == nil || !strings.Contains(err.Error(), "路径") {
		t.Fatalf("缺路径应被拒: %v", err)
	}
}
