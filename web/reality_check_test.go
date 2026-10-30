package web

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func tlsTestServer(t *testing.T, configure func(*http.Server, *tls.Config), h2 bool) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.EnableHTTP2 = h2
	srv.TLS = &tls.Config{}
	if configure != nil {
		configure(srv.Config, srv.TLS)
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	realityRoots = pool
	t.Cleanup(func() { realityRoots = nil })
	return srv, srv.Listener.Addr().(*net.TCPAddr).Port
}

func TestCheckRealityTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, port := tlsTestServer(t, nil, true)
	if r := checkRealityTarget(ctx, "127.0.0.1", port); !r.OK || r.Error != "" {
		t.Fatalf("TLS 1.3 + H2 + X25519 要通过: %+v", r)
	}
	if r := checkRealityTarget(ctx, "localhost", port); r.OK || r.Error != "cert" {
		t.Fatalf("证书和域名对不上: %+v", r)
	}

	_, port = tlsTestServer(t, nil, false)
	if r := checkRealityTarget(ctx, "127.0.0.1", port); r.OK || r.Error != "h2" {
		t.Fatalf("不支持 H2: %+v", r)
	}

	_, port = tlsTestServer(t, func(_ *http.Server, c *tls.Config) { c.MaxVersion = tls.VersionTLS12 }, true)
	if r := checkRealityTarget(ctx, "127.0.0.1", port); r.OK || r.Error != "tls13" {
		t.Fatalf("只有 TLS 1.2: %+v", r)
	}

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if r := checkRealityTarget(ctx, "127.0.0.1", dead); r.OK || r.Error != "connect" {
		t.Fatalf("连不上: %+v", r)
	}

	// 超时不能说成对方不支持:连上了但一直不握手
	hang, _ := net.Listen("tcp", "127.0.0.1:0")
	defer hang.Close()
	short, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if r := checkRealityTarget(short, "127.0.0.1", hang.Addr().(*net.TCPAddr).Port); r.Error != "connect" {
		t.Fatalf("超时: %+v", r)
	}
}

func TestHandleRealityCheck(t *testing.T) {
	_, port := tlsTestServer(t, nil, true)
	s := &Server{}
	call := func(method, body string) (int, string) {
		r := httptest.NewRequest(method, "http://x/app/api/reality-check", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleRealityCheck(w, r)
		return w.Code, w.Body.String()
	}
	code, body := call("POST", `{"server":"127.0.0.1","port":`+strconv.Itoa(port)+`}`)
	var got realityResult
	json.Unmarshal([]byte(body), &got)
	if code != 200 || !got.OK {
		t.Fatalf("%d %s", code, body)
	}
	for _, bad := range []string{"https://a.com", "a.com:443", "a b", "a/b"} {
		b, _ := json.Marshal(map[string]string{"server": bad})
		if code, body := call("POST", string(b)); code != 400 || !strings.Contains(body, "握手目标") {
			t.Fatalf("%s: %d %s", bad, code, body)
		}
	}
	if code, _ := call("GET", ""); code != 405 {
		t.Fatalf("只收 POST: %d", code)
	}
}
