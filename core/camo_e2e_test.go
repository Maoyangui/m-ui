//go:build !race

// 起真实的数据面(只在本机回环地址),和 arrival_e2e_test.go 一样不在 -race 下跑。

package core

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/quic-go/http3"

	"github.com/Maoyangui/m-ui/camo"
)

// 页面伪装(用户 N2)端到端:真实的 anytls / trojan / hysteria2 入站。
//   - 没开:浏览器式的请求(TLS 握手后发 HTTP)和以前一样被断开,记的认证失败原因和以前一字不差;
//   - 开着:同样的请求拿到伪装站(200、站名在页面里),记的原因是 answered with the camouflage site;
//     密码不对的真客户端照旧记原来的原因;Hysteria2 没填伪装站点时 HTTP/3 请求拿到伪装站;
//   - 密码对的真客户端开着伪装照常通。
func TestCamoFallbackE2E(t *testing.T) {
	certPath, keyPath := e2eSelfSigned(t)
	echoAddr := e2eEchoServer(t)
	tlsIn := map[string]interface{}{"enabled": true, "server_name": "localhost", "certificate_path": certPath, "key_path": keyPath}
	h3In := map[string]interface{}{"enabled": true, "server_name": "localhost", "certificate_path": certPath, "key_path": keyPath, "alpn": []string{"h3"}}
	tlsOut := map[string]interface{}{"enabled": true, "insecure": true, "server_name": "localhost"}
	tjPort, atPort, hyPort := e2eFreeTCP(t), e2eFreeTCP(t), e2eFreeUDP(t)
	cfg, _ := json.Marshal(map[string]interface{}{
		"log": map[string]interface{}{"level": "error"},
		"inbounds": []map[string]interface{}{
			{"type": "trojan", "tag": "tj", "listen": "127.0.0.1", "listen_port": tjPort, "users": []map[string]interface{}{{"name": "alice", "password": "pa"}}, "tls": tlsIn},
			{"type": "anytls", "tag": "at", "listen": "127.0.0.1", "listen_port": atPort, "users": []map[string]interface{}{{"name": "alice", "password": "pa"}}, "tls": tlsIn},
			{"type": "hysteria2", "tag": "hy", "listen": "127.0.0.1", "listen_port": hyPort, "users": []map[string]interface{}{{"name": "alice", "password": "pa"}}, "tls": h3In},
		},
		"outbounds": []map[string]interface{}{{"type": "direct", "tag": "direct"}},
	})
	site := camo.New("blog", 42, time.Now())
	var on atomic.Bool
	camo.SetSource(func() http.Handler {
		if on.Load() {
			return site
		}
		return nil
	})
	t.Cleanup(func() { camo.SetSource(nil) })
	c := NewCore()
	if err := c.Start(cfg); err != nil {
		t.Fatalf("数据面起不来: %v", err)
	}
	defer c.Stop()
	tr := c.GetInstance().ConnTracker()
	lastFail := func(inbound string) RecentConn {
		for _, f := range tr.Failures(0) {
			if f.Inbound == inbound {
				return f
			}
		}
		return RecentConn{}
	}
	waitFail := func(inbound string, count int, detail string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			f := lastFail(inbound)
			if f.Count >= count && f.Failed == "auth" && strings.Contains(f.Detail, detail) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s 应记第 %d 次认证失败,原因含 %q,实际 %+v", inbound, count, detail, f)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// browser 像浏览器一样:TLS 握手(ALPN http/1.1)后发一个 GET,读到对方关连接为止
	browser := func(port int) string {
		t.Helper()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", fmt.Sprintf("127.0.0.1:%d", port),
			&tls.Config{InsecureSkipVerify: true, ServerName: "localhost", NextProtos: []string{"http/1.1"}})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
		_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\nUser-Agent: Mozilla/5.0\r\nAccept: text/html\r\nConnection: close\r\n\r\n"))
		b, _ := io.ReadAll(conn)
		return string(b)
	}

	// 没开:断开,原因和以前一字不差
	if got := browser(atPort); got != "" {
		t.Fatalf("没开时 anytls 应直接断开,却回了 %.80q", got)
	}
	waitFail("at", 1, "unknown user password: fallback disabled")
	if got := browser(tjPort); got != "" {
		t.Fatalf("没开时 trojan 应直接断开,却回了 %.80q", got)
	}
	waitFail("tj", 1, "bad request: fallback disabled")

	// 开着:回应伪装站,记成「回应了伪装站」
	on.Store(true)
	for _, c := range []struct {
		port    int
		inbound string
		why     string
	}{{atPort, "at", "not an AnyTLS client: answered with the camouflage site"}, {tjPort, "tj", "not a Trojan client: answered with the camouflage site"}} {
		got := browser(c.port)
		if !strings.HasPrefix(got, "HTTP/1.1 200") || !strings.Contains(got, site.Name()) {
			t.Fatalf("%s 开着伪装时浏览器应看到伪装站,实际 %.120q", c.inbound, got)
		}
		waitFail(c.inbound, 2, c.why)
	}
	// 密码不对的真客户端:照旧记原来的原因,不会误导排查
	bad := e2eClientBox(t, map[string]interface{}{"type": "anytls", "tag": "out", "server": "127.0.0.1", "server_port": atPort, "password": "wrong", "tls": tlsOut})
	if conn, err := e2eSocksDial(bad, echoAddr); err == nil {
		_ = e2eEcho(conn, "ping")
		conn.Close()
	}
	waitFail("at", 3, "unknown user password")
	if f := lastFail("at"); strings.Contains(f.Detail, "camouflage") {
		t.Fatalf("密码不对的客户端不该记成回应了伪装站: %+v", f)
	}
	// 密码对的真客户端照常通
	for _, out := range []map[string]interface{}{
		{"type": "anytls", "tag": "out", "server": "127.0.0.1", "server_port": atPort, "password": "pa", "tls": tlsOut},
		{"type": "trojan", "tag": "out", "server": "127.0.0.1", "server_port": tjPort, "password": "pa", "tls": tlsOut},
	} {
		good := e2eClientBox(t, out)
		e2eMustEcho(t, func() (net.Conn, error) { return e2eSocksDial(good, echoAddr) }, out["type"].(string)+" 密码对的开着伪装照常通").Close()
	}

	// Hysteria2 没填伪装站点:HTTP/3 请求开着伪装时拿到伪装站,关掉是 404
	h3 := &http3.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "localhost", NextProtos: []string{"h3"}}}
	defer h3.Close()
	hc := &http.Client{Transport: h3, Timeout: 8 * time.Second}
	get := func() (int, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://127.0.0.1:%d/", hyPort), nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("HTTP/3 请求失败: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(); code != 200 || !strings.Contains(body, site.Name()) {
		t.Fatalf("hysteria2 开着伪装时应是伪装站: %d", code)
	}
	on.Store(false)
	if code, body := get(); code != 404 || strings.Contains(body, site.Name()) {
		t.Fatalf("hysteria2 没开伪装时应是 404: %d", code)
	}
}
