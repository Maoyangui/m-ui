//go:build !race

// 起真实的数据面(只在本机回环地址),所以不在 -race 下跑:sing-box 自己起实例时有上游的竞态(和 CI 里竞态检测
// 不跑 runner 是同一个原因),会淹掉真正要看的报告。普通测试里照跑。

package core

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// "到了但没过握手 / 认证"端到端:真实的 trojan / anytls / hysteria2 / tuic 入站,
//   - 连上就断(端口扫描、TCP 连通探测)不记;
//   - 不是 TLS 的请求打到 TLS 入站 → 记 tls;密码 / UUID 不对 → 记 auth;
//   - 凭据对的照常放行、进正常的最近连接,不算失败;放行判定一点不变。
func TestHandshakeFailuresRecordedE2E(t *testing.T) {
	certPath, keyPath := e2eSelfSigned(t)
	echoAddr := e2eEchoServer(t)
	const uid = "0e2d8bd4-6a5b-4e3a-9a6c-1c1d1e1f2a3b"
	tlsIn := map[string]interface{}{"enabled": true, "server_name": "localhost", "certificate_path": certPath, "key_path": keyPath}
	h3 := func(m map[string]interface{}) map[string]interface{} {
		out := map[string]interface{}{"alpn": []string{"h3"}}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	tlsOut := map[string]interface{}{"enabled": true, "insecure": true, "server_name": "localhost"}
	tjPort, atPort, hyPort, tuPort := e2eFreeTCP(t), e2eFreeTCP(t), e2eFreeUDP(t), e2eFreeUDP(t)
	cfg, _ := json.Marshal(map[string]interface{}{
		"log": map[string]interface{}{"level": "error"},
		"inbounds": []map[string]interface{}{
			{"type": "trojan", "tag": "tj", "listen": "127.0.0.1", "listen_port": tjPort, "users": []map[string]interface{}{{"name": "alice", "password": "pa"}}, "tls": tlsIn},
			{"type": "anytls", "tag": "at", "listen": "127.0.0.1", "listen_port": atPort, "users": []map[string]interface{}{{"name": "alice", "password": "pa"}}, "tls": tlsIn},
			{"type": "hysteria2", "tag": "hy", "listen": "127.0.0.1", "listen_port": hyPort, "users": []map[string]interface{}{{"name": "alice", "password": "pa"}}, "tls": h3(tlsIn)},
			{"type": "tuic", "tag": "tu", "listen": "127.0.0.1", "listen_port": tuPort, "users": []map[string]interface{}{{"name": "alice", "uuid": uid, "password": "pa"}}, "tls": h3(tlsIn)},
		},
		"outbounds": []map[string]interface{}{{"type": "direct", "tag": "direct"}},
	})
	c := NewCore()
	if err := c.Start(cfg); err != nil {
		t.Fatalf("数据面起不来: %v", err)
	}
	defer c.Stop()
	tr := c.GetInstance().ConnTracker()
	failOf := func(inbound string) (RecentConn, bool) {
		for _, f := range tr.Failures(0) {
			if f.Inbound == inbound {
				return f, true
			}
		}
		return RecentConn{}, false
	}
	waitFail := func(inbound, reason string, count int) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			f, ok := failOf(inbound)
			if ok && f.Failed == reason && f.Count >= count && f.IP == "127.0.0.1" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s 应记一笔 %s(第 %d 次),实际 %+v(全部 %+v)", inbound, reason, count, f, tr.Failures(0))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	tryEcho := func(proxy string) error {
		conn, err := e2eSocksDial(proxy, echoAddr)
		if err != nil {
			return err
		}
		defer conn.Close()
		return e2eEcho(conn, "ping")
	}

	// 连上就断:不记
	if conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tjPort)); err == nil {
		conn.Close()
	}
	time.Sleep(300 * time.Millisecond)
	if f, ok := failOf("tj"); ok {
		t.Fatalf("连上就断不该记: %+v", f)
	}
	// 不是 TLS 的请求打到 TLS 入站:记 tls,原因带着
	if conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tjPort)); err == nil {
		_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.Copy(io.Discard, conn)
		conn.Close()
	}
	waitFail("tj", "tls", 1)
	if f, _ := failOf("tj"); !strings.Contains(f.Detail, "tls") || f.Protocol != "trojan" {
		t.Fatalf("要带着握手失败的原因: %+v", f)
	}
	// trojan 密码不对:记 auth
	bad := e2eClientBox(t, map[string]interface{}{"type": "trojan", "tag": "out", "server": "127.0.0.1", "server_port": tjPort, "password": "wrong", "tls": tlsOut})
	if tryEcho(bad) == nil {
		t.Fatal("密码不对不该通")
	}
	waitFail("tj", "auth", 2)
	// 密码对:照常放行、进正常的最近连接,失败那条不再加
	good := e2eClientBox(t, map[string]interface{}{"type": "trojan", "tag": "out", "server": "127.0.0.1", "server_port": tjPort, "password": "pa", "tls": tlsOut})
	if err := tryEcho(good); err != nil {
		t.Fatalf("凭据对的应照常通: %v", err)
	}
	if f, _ := failOf("tj"); f.Count != 2 {
		t.Fatalf("放行的不算失败: %+v", f)
	}
	okRecent := false
	for _, r := range tr.Recent(0) {
		okRecent = okRecent || (r.Inbound == "tj" && r.User == "alice" && r.Failed == "")
	}
	if !okRecent {
		t.Fatalf("放行的进正常的最近连接: %+v", tr.Recent(0))
	}

	// anytls 密码不对:记 auth
	at := e2eClientBox(t, map[string]interface{}{"type": "anytls", "tag": "out", "server": "127.0.0.1", "server_port": atPort, "password": "wrong", "tls": tlsOut})
	_ = tryEcho(at)
	waitFail("at", "auth", 1)

	// hysteria2:密码对的照常通、不记;密码不对(落到伪装站点)记 auth。先测对的:错的客户端之后可能还在重连
	hyOK := e2eClientBox(t, map[string]interface{}{"type": "hysteria2", "tag": "out", "server": "127.0.0.1", "server_port": hyPort, "password": "pa", "tls": h3(tlsOut)})
	e2eMustEcho(t, func() (net.Conn, error) { return e2eSocksDial(hyOK, echoAddr) }, "hysteria2 密码对应通").Close()
	time.Sleep(300 * time.Millisecond)
	if f, ok := failOf("hy"); ok {
		t.Fatalf("hysteria2 放行的不算失败: %+v", f)
	}
	hy := e2eClientBox(t, map[string]interface{}{"type": "hysteria2", "tag": "out", "server": "127.0.0.1", "server_port": hyPort, "password": "wrong", "tls": h3(tlsOut)})
	_ = tryEcho(hy)
	waitFail("hy", "auth", 1)

	// tuic:凭据对的照常通、不记;密码不对(库自己关掉连接)记 auth
	tuOK := e2eClientBox(t, map[string]interface{}{"type": "tuic", "tag": "out", "server": "127.0.0.1", "server_port": tuPort, "uuid": uid, "password": "pa", "congestion_control": "cubic", "tls": h3(tlsOut)})
	e2eMustEcho(t, func() (net.Conn, error) { return e2eSocksDial(tuOK, echoAddr) }, "tuic 凭据对应通").Close()
	time.Sleep(300 * time.Millisecond)
	if f, ok := failOf("tu"); ok {
		t.Fatalf("tuic 放行的不算失败: %+v", f)
	}
	tu := e2eClientBox(t, map[string]interface{}{"type": "tuic", "tag": "out", "server": "127.0.0.1", "server_port": tuPort, "uuid": uid, "password": "wrong", "congestion_control": "cubic", "tls": h3(tlsOut)})
	_ = tryEcho(tu)
	waitFail("tu", "auth", 1)
}
