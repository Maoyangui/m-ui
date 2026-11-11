//go:build !race && with_utls

// REALITY 要 with_utls 构建(CI、发布都带);和 arrival_e2e_test.go 一样起真实的数据面(只在本机回环地址),不在 -race 下跑。

package core

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// REALITY 线路(vless + REALITY,伪装目标是本机一个 TLS 1.3 服务):
//   - 连上就断(端口扫描、Globalping 的 TCP 连通探测 —— 包括面板自己每轮的大陆连通 / IPv6 入站检测)不记;
//   - 发来的不是 TLS(扫描器探 HTTP):REALITY 整条转给伪装目标,不记;
//   - 发来的是普通 TLS 的 ClientHello(不是 REALITY 客户端)、short_id 不对的 REALITY 客户端:记 tls;
//   - 凭据对的照常放行,不算失败;
//   - 伪装目标连不上:谁都连不上,连上就断的也记,原因里写着 failed to dial dest。
func TestRealityHandshakeFailuresE2E(t *testing.T) {
	certPath, keyPath := e2eSelfSigned(t)
	echoAddr := e2eEchoServer(t)
	dest := e2eTLSDest(t, certPath, keyPath)
	_, destPort, _ := net.SplitHostPort(dest)
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privKey, pubKey := base64.RawURLEncoding.EncodeToString(priv.Bytes()), base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())
	const uid, sid = "5b0e2a9c-3f1d-4c6b-8a7e-2d4c6e8f0a1b", "0123456789abcdef"
	reality := func(port string) map[string]interface{} {
		p := 0
		fmt.Sscanf(port, "%d", &p)
		return map[string]interface{}{"enabled": true, "server_name": "localhost", "reality": map[string]interface{}{
			"enabled": true, "handshake": map[string]interface{}{"server": "127.0.0.1", "server_port": p}, "private_key": privKey, "short_id": []string{sid}}}
	}
	rlPort, rdPort, rvPort := e2eFreeTCP(t), e2eFreeTCP(t), e2eFreeTCP(t)
	closedPort := fmt.Sprint(e2eFreeTCP(t)) // 没人监听:伪装目标连不上
	cfg, _ := json.Marshal(map[string]interface{}{
		"log": map[string]interface{}{"level": "error"},
		"inbounds": []map[string]interface{}{
			{"type": "vless", "tag": "rl", "listen": "127.0.0.1", "listen_port": rlPort, "users": []map[string]interface{}{{"name": "alice", "uuid": uid}}, "tls": reality(destPort)},
			{"type": "vless", "tag": "rd", "listen": "127.0.0.1", "listen_port": rdPort, "users": []map[string]interface{}{{"name": "alice", "uuid": uid}}, "tls": reality(closedPort)},
			{"type": "vless", "tag": "rv", "listen": "127.0.0.1", "listen_port": rvPort, "users": []map[string]interface{}{{"name": "alice", "uuid": uid, "flow": "xtls-rprx-vision"}}, "tls": reality(destPort)},
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
	waitFail := func(inbound string, count int) RecentConn {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			f, ok := failOf(inbound)
			if ok && f.Failed == "tls" && f.Count >= count && f.IP == "127.0.0.1" {
				return f
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s 应记一笔 tls(第 %d 次),实际 %+v(全部 %+v)", inbound, count, f, tr.Failures(0))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// drain 等服务器这头关掉连接(REALITY 转给伪装目标的那条也要等两头都结束),之后入站才会去记 / 不记
	drain := func(conn net.Conn) {
		_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
		_, _ = io.Copy(io.Discard, conn)
		conn.Close()
		time.Sleep(400 * time.Millisecond)
	}
	rl := fmt.Sprintf("127.0.0.1:%d", rlPort)

	// 1. 连上就断:不记(面板自己的连通检测就是这样:三次握手完就关,一个字节不发)
	for i := 0; i < 3; i++ {
		if conn, err := net.Dial("tcp", rl); err == nil {
			conn.Close()
		}
	}
	time.Sleep(600 * time.Millisecond)
	if f, ok := failOf("rl"); ok {
		t.Fatalf("REALITY 线路上连上就断不该记: %+v", f)
	}
	// 2. 不是 TLS(扫描器探 HTTP):转给伪装目标,不记
	if conn, err := net.Dial("tcp", rl); err == nil {
		_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		drain(conn)
	}
	if f, ok := failOf("rl"); ok {
		t.Fatalf("REALITY 线路上不是 TLS 的不该记: %+v", f)
	}
	// 3. 凭据对的 REALITY 客户端:照常放行,不算失败
	good := e2eClientBox(t, map[string]interface{}{"type": "vless", "tag": "out", "server": "127.0.0.1", "server_port": rlPort, "uuid": uid,
		"tls": map[string]interface{}{"enabled": true, "server_name": "localhost", "utls": map[string]interface{}{"enabled": true, "fingerprint": "chrome"},
			"reality": map[string]interface{}{"enabled": true, "public_key": pubKey, "short_id": sid}}})
	e2eMustEcho(t, func() (net.Conn, error) { return e2eSocksDial(good, echoAddr) }, "REALITY 凭据对应通").Close()
	time.Sleep(300 * time.Millisecond)
	if f, ok := failOf("rl"); ok {
		t.Fatalf("放行的不算失败: %+v", f)
	}
	// 3b. vision(VLESS + Reality 预设默认开):隧道里跑 TLS 1.3 时 vision 改成直接读写 REALITY 下面那层连接(就是套的那一层),
	// 要照常通、不算失败
	vis := e2eClientBox(t, map[string]interface{}{"type": "vless", "tag": "out", "server": "127.0.0.1", "server_port": rvPort, "uuid": uid, "flow": "xtls-rprx-vision",
		"tls": map[string]interface{}{"enabled": true, "server_name": "localhost", "utls": map[string]interface{}{"enabled": true, "fingerprint": "chrome"},
			"reality": map[string]interface{}{"enabled": true, "public_key": pubKey, "short_id": sid}}})
	var visErr error
	for i := 0; i < 6; i++ {
		if visErr = e2eTLSEcho(vis, dest, fmt.Sprintf("vision-%d", i)); visErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if visErr != nil {
		t.Fatalf("vision 应照常通: %v", visErr)
	}
	time.Sleep(300 * time.Millisecond)
	if f, ok := failOf("rv"); ok {
		t.Fatalf("vision 放行的不算失败: %+v", f)
	}
	// 4. 普通 TLS 的 ClientHello(不是 REALITY 客户端):握手被转给伪装目标(和它握手成功),之后关掉;记 tls
	if conn, err := tls.Dial("tcp", rl, &tls.Config{ServerName: "localhost", InsecureSkipVerify: true}); err == nil {
		conn.Close()
	} else {
		t.Logf("普通 TLS 客户端: %v", err)
	}
	if f := waitFail("rl", 1); !strings.Contains(f.Detail, "REALITY") || f.Protocol != "vless" {
		t.Fatalf("要带着 REALITY 的失败原因: %+v", f)
	}
	// 5. short_id 不对的 REALITY 客户端:记 tls
	bad := e2eClientBox(t, map[string]interface{}{"type": "vless", "tag": "out", "server": "127.0.0.1", "server_port": rlPort, "uuid": uid,
		"tls": map[string]interface{}{"enabled": true, "server_name": "localhost", "utls": map[string]interface{}{"enabled": true, "fingerprint": "chrome"},
			"reality": map[string]interface{}{"enabled": true, "public_key": pubKey, "short_id": "fedcba9876543210"}}})
	if conn, err := e2eSocksDial(bad, echoAddr); err == nil {
		if e2eEcho(conn, "x") == nil {
			t.Fatal("short_id 不对不该通")
		}
		conn.Close()
	}
	waitFail("rl", 2)

	// 6. 伪装目标连不上:连上就断的也记,原因写明
	if conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", rdPort)); err == nil {
		drain(conn)
	}
	if f := waitFail("rd", 1); !strings.Contains(f.Detail, "failed to dial dest") {
		t.Fatalf("伪装目标连不上要写明: %+v", f)
	}
}

// e2eTLSEcho 经 SOCKS5 代理连 target(本机的 TLS 1.3 回显服务),在隧道里做 TLS 握手、发一句、收回同样的一句。
func e2eTLSEcho(proxy, target, msg string) error {
	raw, err := e2eSocksDial(proxy, target)
	if err != nil {
		return err
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	c := tls.Client(raw, &tls.Config{ServerName: "localhost", InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err := c.Handshake(); err != nil {
		return err
	}
	for i := 0; i < 3; i++ { // 多来回几次:vision 在头几个包之后才切到直接读写
		if _, err := c.Write([]byte(msg)); err != nil {
			return err
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(c, buf); err != nil {
			return err
		}
		if string(buf) != msg {
			return fmt.Errorf("回显不对: %q", buf)
		}
	}
	return nil
}

// e2eTLSDest 本机的 TLS 1.3 服务,当 REALITY 的伪装目标:握手完把收到的原样写回,对端关了就关。
func e2eTLSDest(t *testing.T, certPath, keyPath string) string {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String()
}
