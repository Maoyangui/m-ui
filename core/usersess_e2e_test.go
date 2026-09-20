package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sb "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
)

// 真实入站的端到端核验:起一个带 hysteria2 / tuic / anytls 入站的数据面,再起一个 sing-box 客户端 Box
// (mixed 入站 → 对应协议出站),经隧道连回本机的 echo 服务;然后逐条核对 usersess 的承诺:
//
//  1. 表没变(只换顺序)热更新 → 一条会话都不关,老连接照常 echo;
//  2. 踢线 CloseUserSessions → 整条会话关掉(不只是里面的流),老连接读到错误;凭据没变,客户端能重连;
//  3. 移除用户 → 会话关掉,旧凭据连不上;再加回来 → 不关任何会话,又能连;
//  4. 换密码 → 会话关掉,旧凭据连不上。
//
// 要开真实端口跑 QUIC,默认跳过:
//
//	M_UI_E2E=1 go test -tags with_utls ./core/ -run E2E -v
//
// 升级 sing-box / sing-quic / quic-go 后必须跑一遍:会话登记靠的是 sing-quic 的 ServerConfig 扩展点,
// 上游改了扩展点编译不会报错,只有这里的 CloseUserSessions 会从 1 变成 0。
func TestUserSessionsE2E(t *testing.T) {
	if os.Getenv("M_UI_E2E") != "1" {
		t.Skip("需要真实端口与 QUIC:M_UI_E2E=1 时才跑")
	}
	certPath, keyPath := e2eSelfSigned(t)
	echoAddr := e2eEchoServer(t)

	const u1, u2 = "0e2d8bd4-6a5b-4e3a-9a6c-1c1d1e1f2a3b", "7b1c9c2e-3d4f-4a5b-8c6d-9e0f1a2b3c4d"
	type proto struct {
		name, tag string
		port      int
		users     func(alicePassword string) []map[string]interface{}
		outbound  func(port int, password string) map[string]interface{}
	}
	tlsIn := map[string]interface{}{"enabled": true, "server_name": "localhost", "certificate_path": certPath, "key_path": keyPath}
	tlsOut := map[string]interface{}{"enabled": true, "insecure": true, "server_name": "localhost"}
	withALPN := func(m map[string]interface{}) map[string]interface{} {
		out := map[string]interface{}{}
		for k, v := range m {
			out[k] = v
		}
		out["alpn"] = []string{"h3"}
		return out
	}
	protos := []proto{
		{name: "hysteria2", tag: "hy2", port: e2eFreeUDP(t),
			users: func(pw string) []map[string]interface{} {
				return []map[string]interface{}{{"name": "alice", "password": pw}, {"name": "bob", "password": "pb"}}
			},
			outbound: func(port int, pw string) map[string]interface{} {
				return map[string]interface{}{"type": "hysteria2", "tag": "out", "server": "127.0.0.1", "server_port": port, "password": pw, "tls": withALPN(tlsOut)}
			}},
		{name: "tuic", tag: "tuic", port: e2eFreeUDP(t),
			users: func(pw string) []map[string]interface{} {
				return []map[string]interface{}{{"name": "alice", "uuid": u1, "password": pw}, {"name": "bob", "uuid": u2, "password": "pb"}}
			},
			outbound: func(port int, pw string) map[string]interface{} {
				return map[string]interface{}{"type": "tuic", "tag": "out", "server": "127.0.0.1", "server_port": port, "uuid": u1, "password": pw, "congestion_control": "cubic", "tls": withALPN(tlsOut)}
			}},
		{name: "anytls", tag: "anytls", port: e2eFreeTCP(t),
			users: func(pw string) []map[string]interface{} {
				return []map[string]interface{}{{"name": "alice", "password": pw}, {"name": "bob", "password": "pb"}}
			},
			outbound: func(port int, pw string) map[string]interface{} {
				return map[string]interface{}{"type": "anytls", "tag": "out", "server": "127.0.0.1", "server_port": port, "password": pw, "tls": tlsOut}
			}},
	}
	inbound := func(p proto, users []map[string]interface{}) map[string]interface{} {
		tls := tlsIn
		if p.name != "anytls" {
			tls = withALPN(tlsIn)
		}
		return map[string]interface{}{"type": p.name, "tag": p.tag, "listen": "127.0.0.1", "listen_port": p.port, "users": users, "tls": tls}
	}
	var inbounds []map[string]interface{}
	for _, p := range protos {
		inbounds = append(inbounds, inbound(p, p.users("pa")))
	}
	serverCfg, _ := json.Marshal(map[string]interface{}{
		"log":       map[string]interface{}{"level": "error"},
		"inbounds":  inbounds,
		"outbounds": []map[string]interface{}{{"type": "direct", "tag": "direct"}},
	})
	c := NewCore()
	if err := c.Start(serverCfg); err != nil {
		t.Fatalf("数据面起不来: %v", err)
	}
	defer c.Stop()
	updateUsers := func(t *testing.T, p proto, users []map[string]interface{}) int {
		t.Helper()
		raw, _ := json.Marshal(inbound(p, users))
		handled, closed, err := c.UpdateInboundUsers(raw)
		if err != nil || !handled {
			t.Fatalf("热换用户表失败: handled=%v err=%v", handled, err)
		}
		return closed
	}
	reverse := func(users []map[string]interface{}) []map[string]interface{} {
		out := make([]map[string]interface{}, 0, len(users))
		for i := len(users) - 1; i >= 0; i-- {
			out = append(out, users[i])
		}
		return out
	}

	for _, p := range protos {
		p := p
		t.Run(p.name, func(t *testing.T) {
			proxy := e2eClientBox(t, p.outbound(p.port, "pa"))
			dial := func() (net.Conn, error) { return e2eSocksDial(proxy, echoAddr) }

			c1 := e2eMustEcho(t, dial, "隧道应通")
			defer c1.Close()

			// 1. 表没变、只换顺序:一条会话都不关
			if n := updateUsers(t, p, reverse(p.users("pa"))); n != 0 {
				t.Fatalf("表没变却关了 %d 条会话", n)
			}
			if err := e2eEcho(c1, "after-reorder"); err != nil {
				t.Fatalf("表没变热更新后老连接应照常: %v", err)
			}

			// 2. 踢线:整条会话关掉,老连接读到错误;凭据没变,能重连
			if n := c.GetInstance().CloseUserSessions([]string{"alice"}); n != 1 {
				t.Fatalf("踢线应关掉 1 条会话,实际 %d(0 = 会话登记没生效,查 sing-quic 的 ServerConfig 扩展点)", n)
			}
			e2eMustFail(t, c1, "踢线后老连接应断开")
			c2 := e2eMustEcho(t, dial, "凭据没变,踢线后应能重连")
			defer c2.Close()

			// 3. 移除用户:会话关掉,旧凭据连不上;加回来又能连,且不关任何会话
			if n := updateUsers(t, p, p.users("pa")[1:]); n != 1 {
				t.Fatalf("移除 alice 应关掉她 1 条会话,实际 %d", n)
			}
			e2eMustFail(t, c2, "被移除后老连接应断开")
			e2eMustNotConnect(t, dial, "被移除后旧凭据不该再连上")
			if n := updateUsers(t, p, p.users("pa")); n != 0 {
				t.Fatalf("加回 alice 不该关任何会话,实际关了 %d", n)
			}
			c3 := e2eMustEcho(t, dial, "加回之后应能连")
			defer c3.Close()

			// 4. 换密码:会话关掉,旧凭据连不上
			if n := updateUsers(t, p, p.users("pa2")); n != 1 {
				t.Fatalf("换密码应关掉 1 条会话,实际 %d", n)
			}
			e2eMustFail(t, c3, "换密码后老连接应断开")
			e2eMustNotConnect(t, dial, "换密码后旧凭据不该再连上")
		})
	}
}

// e2eClientBox 起一个客户端 Box:mixed 入站(SOCKS5)→ 给定出站,返回 SOCKS5 地址。
func e2eClientBox(t *testing.T, outbound map[string]interface{}) string {
	t.Helper()
	port := e2eFreeTCP(t)
	raw, _ := json.Marshal(map[string]interface{}{
		"log":       map[string]interface{}{"level": "error"},
		"inbounds":  []map[string]interface{}{{"type": "mixed", "tag": "in", "listen": "127.0.0.1", "listen_port": port}},
		"outbounds": []map[string]interface{}{outbound},
	})
	ctx := sb.Context(context.Background(), InboundRegistry(), OutboundRegistry(), EndpointRegistry(), DNSTransportRegistry(), ServiceRegistry(), CertificateProviderRegistry())
	var opt option.Options
	if err := opt.UnmarshalJSONContext(ctx, raw); err != nil {
		t.Fatalf("客户端配置: %v", err)
	}
	box, err := NewBox(Options{Context: ctx, Options: opt})
	if err != nil {
		t.Fatalf("客户端 Box: %v", err)
	}
	if err := box.Start(); err != nil {
		t.Fatalf("客户端起不来: %v", err)
	}
	t.Cleanup(func() { _ = box.Close() })
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// e2eSocksDial 走 SOCKS5(无认证)连到 target。sing-box 的 SOCKS 入站在拨上游之前就回成功,
// 所以"连不上"要靠之后的读写来判断,不是看这里的应答码。
func e2eSocksDial(proxy, target string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		conn.Close()
		return nil, err
	}
	rep := make([]byte, 2)
	if _, err := io.ReadFull(conn, rep); err != nil || rep[1] != 0 {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 握手失败: %v %v", rep, err)
	}
	host, portStr, _ := net.SplitHostPort(target)
	ip := net.ParseIP(host).To4()
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	req := append([]byte{5, 1, 0, 1}, ip...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}
	rep = make([]byte, 10)
	if _, err := io.ReadFull(conn, rep); err != nil || rep[1] != 0 {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 CONNECT 失败: %v %v", rep, err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func e2eEcho(conn net.Conn, msg string) error {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetDeadline(time.Time{})
	if _, err := conn.Write([]byte(msg)); err != nil {
		return err
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != msg {
		return fmt.Errorf("回显不对: %q", buf)
	}
	return nil
}

// e2eMustEcho 反复拨号直到隧道通(客户端出站刚被关掉的会话要重建,允许几次尝试)。
func e2eMustEcho(t *testing.T, dial func() (net.Conn, error), why string) net.Conn {
	t.Helper()
	var last error
	for i := 0; i < 6; i++ {
		conn, err := dial()
		if err == nil {
			if err = e2eEcho(conn, fmt.Sprintf("ping-%d", i)); err == nil {
				return conn
			}
			conn.Close()
		}
		last = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s: %v", why, last)
	return nil
}

// e2eMustFail 老连接必须在几秒内读到错误(会话被关 → 流跟着断)。
func e2eMustFail(t *testing.T, conn net.Conn, why string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 16)
	_, _ = conn.Write([]byte("x"))
	if _, err := conn.Read(buf); err == nil {
		t.Fatalf("%s:连接还活着", why)
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("%s:10 秒内没断(只关了流、会话还在?)", why)
	}
}

// e2eMustNotConnect 用旧凭据拨号,每次都必须失败:握手被拒时 SOCKS 那头会直接关掉,读到 EOF。
func e2eMustNotConnect(t *testing.T, dial func() (net.Conn, error), why string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		conn, err := dial()
		if err != nil {
			continue
		}
		err = e2eEcho(conn, "should-fail")
		conn.Close()
		if err == nil {
			t.Fatalf("%s(第 %d 次却通了)", why, i+1)
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("%s:10 秒既不通也不断", why)
		}
	}
}

func e2eEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	return ln.Addr().String()
}

func e2eFreeTCP(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func e2eFreeUDP(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// e2eSelfSigned 给 localhost 签一张自签证书,写到临时目录。
func e2eSelfSigned(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	// sing-box 在 Windows 上也按正斜杠路径读文件没问题;这里只是避免 JSON 里出现反斜杠转义
	return strings.ReplaceAll(certPath, `\`, "/"), strings.ReplaceAll(keyPath, `\`, "/")
}
