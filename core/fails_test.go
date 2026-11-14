package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/Maoyangui/m-ui/core/protocol/arrival"
)

// "到了但没过握手 / 认证"单独存:不进正常的最近连接、不挤掉它;同一来源再来只累加;新来源限频、有上限,满了丢最旧的。
func TestHandshakeFailuresSeparateAndLimited(t *testing.T) {
	c := NewConnTracker(nil)
	c.noteRecent("203.0.113.1", "alice", "tj", "trojan", "")
	c.NoteFailure("203.0.113.9", "tj", "trojan", arrival.TLS, "remote error: tls: bad certificate")
	c.NoteFailure("203.0.113.9", "tj", "trojan", arrival.Auth, "bad request")
	if r := c.Recent(0); len(r) != 1 || r[0].IP != "203.0.113.1" || r[0].Failed != "" {
		t.Fatalf("失败记录不能混进正常的最近连接: %+v", r)
	}
	f := c.Failures(0)
	if len(f) != 1 || f[0].Count != 2 || f[0].Failed != arrival.Auth || f[0].Detail != "bad request" || f[0].Inbound != "tj" {
		t.Fatalf("同一来源应累加、原因取最近一次: %+v", f)
	}

	// 新来源限频:桶里还剩 failBurst-1 个令牌,再来一大批只进得去这么多
	for i := 0; i < 200; i++ {
		c.NoteFailure(fmt.Sprintf("198.51.100.%d", i), "tj", "trojan", arrival.TLS, "x")
	}
	if n := len(c.Failures(0)); n != failBurst {
		t.Fatalf("新来源应被限频到 %d 个,实际 %d", failBurst, n)
	}
	// 令牌用完时,已在表里的来源照样累加(用户自己反复重试的那条一直在)
	c.NoteFailure("203.0.113.9", "tj", "trojan", arrival.TLS, "again")
	if f := c.Failures(1); len(f) != 1 || f[0].IP != "203.0.113.9" || f[0].Count != 3 {
		t.Fatalf("已有来源不占令牌、应排到最前: %+v", f)
	}
	// 过一阵补满令牌,一直有新来源:不超过上限,挤掉的是最久没动的
	for round := 0; round < 10; round++ {
		c.failMu.Lock()
		c.failAt = c.failAt.Add(-time.Duration(failBurst) * failRefill)
		c.failMu.Unlock()
		for i := 0; i < failBurst; i++ {
			c.NoteFailure(fmt.Sprintf("192.0.2.%d", round*failBurst+i), "tj", "trojan", arrival.TLS, "x")
		}
	}
	f = c.Failures(0)
	if len(f) != failCap {
		t.Fatalf("应封顶 %d 条,实际 %d", failCap, len(f))
	}
	for _, r := range f {
		if r.IP == "198.51.100.0" {
			t.Fatal("满了应先挤掉最久没动的")
		}
	}
	if r := c.Recent(0); len(r) != 1 {
		t.Fatalf("扫描刷不掉正常记录: %+v", r)
	}
}

type fakeRecorder struct{ got []string }

func (f *fakeRecorder) NoteFailure(ip, inbound, protocol, reason, detail string) {
	f.got = append(f.got, ip+"|"+inbound+"|"+protocol+"|"+reason+"|"+detail)
}

// 什么都没发就断(端口扫描、TCP 连通探测,包括面板自己的大陆 / IPv6 入站检测)、被重置、超时:不记。
// 真正的握手 / 认证错误照记,IPv4 映射地址按 IPv4 记;记录器为空(干跑)什么都不做。
func TestArrivalNoteSkipsQuietErrors(t *testing.T) {
	rec := &fakeRecorder{}
	src := M.SocksaddrFrom(netip.MustParseAddr("::ffff:203.0.113.5"), 443)
	for _, err := range []error{io.EOF, fmt.Errorf("tls: %w", io.ErrUnexpectedEOF), syscall.ECONNRESET, os.ErrDeadlineExceeded, context.Canceled} {
		arrival.Note(rec, src, "tj", "trojan", arrival.TLS, err)
	}
	if len(rec.got) != 0 {
		t.Fatalf("这些不该记: %v", rec.got)
	}
	arrival.Note(rec, src, "tj", "trojan", arrival.TLS, errors.New("remote error: tls: bad certificate"))
	arrival.Note(rec, M.Socksaddr{Fqdn: "example.com", Port: 443}, "tj", "trojan", arrival.TLS, errors.New("x"))
	arrival.NoteAddr(rec, "[2001:db8::7]:4433", "hy", "hysteria2", arrival.Auth, "")
	arrival.NoteAddr(rec, "not-an-addr", "hy", "hysteria2", arrival.Auth, "")
	arrival.Note(nil, src, "tj", "trojan", arrival.TLS, errors.New("x"))
	want := []string{"203.0.113.5|tj|trojan|tls|remote error: tls: bad certificate", "2001:db8::7|hy|hysteria2|auth|"}
	if fmt.Sprint(rec.got) != fmt.Sprint(want) {
		t.Fatalf("记录 = %v,应为 %v", rec.got, want)
	}
}

// 标记:协议服务认证之后才调入站的处理函数,处理函数里 Pass;服务返回错误时没 Pass 的才算认证没过。
func TestArrivalMark(t *testing.T) {
	ctx, m := arrival.WithMark(context.Background())
	if m.Passed() {
		t.Fatal("还没进到路由")
	}
	arrival.Pass(context.WithValue(ctx, struct{}{}, 1)) // 服务层派生的 ctx 照样找得到
	if !m.Passed() {
		t.Fatal("Pass 之后应为已通过")
	}
	arrival.Pass(context.Background()) // 没有标记时什么都不做
	var nilMark *arrival.Mark
	if nilMark.Passed() {
		t.Fatal("nil 标记")
	}
}

// REALITY 线路(arrival.Watch 套一层,只看客户端发来的第一个字节):
//   - 连上就断(一个字节不发,面板自己的大陆 / IPv6 入站检测就是这样)不记,哪怕 REALITY 报的是 processed invalid connection;
//   - 发来的不是 TLS 记录(HTTP 一类的扫描)不记:REALITY 把它整条转给了伪装目标;
//   - 发来的是 TLS 记录(0x16:普通 TLS 客户端、公钥 / short_id 不对的 REALITY 客户端)才记;
//   - 伪装目标连不上的照记(一个字节没收到也记:谁都连不上,是服务器这边要修的);
//   - 普通 TLS(观察器为 nil)照旧按错误类型认:EOF / 重置 / 超时不记,其它照记。
func TestArrivalRealityNeedsTLSRecord(t *testing.T) {
	src := M.SocksaddrFrom(netip.MustParseAddr("198.51.100.7"), 50000)
	invalid := errors.New("REALITY: processed invalid connection")
	try := func(send []byte, err error) []string {
		t.Helper()
		rec := &fakeRecorder{}
		srv, cli := net.Pipe()
		hs, w := arrival.Watch(srv, true)
		if hs == srv || w == nil {
			t.Fatal("REALITY 要套一层")
		}
		go func() {
			if len(send) > 0 {
				_, _ = cli.Write(send)
			}
			cli.Close()
		}()
		buf := make([]byte, 64)
		for {
			if _, err := hs.Read(buf); err != nil {
				break
			}
		}
		arrival.NoteTLS(rec, w, src, "rl", "vless", err)
		return rec.got
	}
	if got := try(nil, invalid); len(got) != 0 {
		t.Fatalf("连上就断不该记: %v", got)
	}
	if got := try([]byte("GET / HTTP/1.1\r\n\r\n"), invalid); len(got) != 0 {
		t.Fatalf("不是 TLS 的不该记: %v", got)
	}
	if got := try([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 1, 2, 3, 4, 5}, invalid); len(got) != 1 || got[0] != "198.51.100.7|rl|vless|tls|REALITY: processed invalid connection" {
		t.Fatalf("发来 TLS 记录的要记: %v", got)
	}
	if got := try(nil, errors.New("REALITY: failed to dial dest: dial tcp 127.0.0.1:1: connect: connection refused")); len(got) != 1 {
		t.Fatalf("伪装目标连不上要记: %v", got)
	}

	// 普通 TLS:不套,按错误类型认
	srv, _ := net.Pipe()
	if hs, w := arrival.Watch(srv, false); hs != srv || w != nil {
		t.Fatal("普通 TLS 不该套")
	}
	rec := &fakeRecorder{}
	arrival.NoteTLS(rec, nil, src, "tj", "trojan", io.EOF)
	arrival.NoteTLS(rec, nil, src, "tj", "trojan", errors.New("tls: first record does not look like a TLS handshake"))
	if len(rec.got) != 1 || !strings.Contains(rec.got[0], "first record") {
		t.Fatalf("普通 TLS 按错误类型认: %v", rec.got)
	}
}

// 套的那一层对 sing 的拆包透明(kTLS、vision 按类型找底层 TCP 连接),转给伪装目标时的 io.Copy 交给底层连接。
func TestArrivalWatchTransparent(t *testing.T) {
	base := &copyConn{}
	hs, w := arrival.Watch(base, true)
	if N.UnwrapReader(hs) != net.Conn(base) || N.UnwrapWriter(hs) != net.Conn(base) {
		t.Fatal("拆包要拆到底层连接")
	}
	if c, ok := N.CastReader[*copyConn](hs); !ok || c != base {
		t.Fatal("按类型要找得到底层连接")
	}
	if _, err := io.Copy(io.Discard, hs); err != nil || !base.wroteTo {
		t.Fatalf("io.Copy 读这一头要交给底层的 WriteTo: %v %v", err, base.wroteTo)
	}
	if _, err := io.Copy(hs, struct{ io.Reader }{strings.NewReader("x")}); err != nil || !base.readFrom {
		t.Fatalf("io.Copy 写这一头要交给底层的 ReadFrom: %v %v", err, base.readFrom)
	}
	if _, ok := w.First(); ok {
		t.Fatal("没经 Read 读过,不该当成收到了")
	}
}

// copyConn 带 WriteTo / ReadFrom 的假连接(像 *net.TCPConn)。
type copyConn struct {
	net.Conn
	wroteTo, readFrom bool
}

func (c *copyConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *copyConn) WriteTo(io.Writer) (int64, error) {
	c.wroteTo = true
	return 0, nil
}
func (c *copyConn) ReadFrom(r io.Reader) (int64, error) {
	c.readFrom = true
	return io.Copy(io.Discard, r)
}
