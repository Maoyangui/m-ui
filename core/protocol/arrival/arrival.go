// Package arrival 入站"包到了、却没过握手或认证"的记录口子。
//
// 概览的"最近入站连接"由数据面的路由追踪器记:只有通过协议握手与认证、被路由的连接才进得去。
// 证书 / SNI / REALITY 参数不对、密码或 UUID 不对的连接一条都不留,面板上看起来和"包没到服务器"一模一样,
// 用户只会去查防火墙。入站在这几处失败时经这里记一笔,数据面的连接追踪器单独存放(独立上限、新来源限频,
// 扫描刷不掉正常记录)。只记,不改任何放行判定。
//
// 记录器在 NewBox 时注册进服务注册表(和 usersess.Set 一样是进程级共享的槽位,先停旧数据面再起新的),
// 入站构造时取一次;干跑、测试里没有,Note 什么都不做。
package arrival

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"syscall"

	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

// 失败原因。
const (
	TLS  = "tls"  // TLS / REALITY 握手没过:证书、SNI、REALITY 公钥或 short_id 不对,或客户端没用 TLS
	Auth = "auth" // 握手过了、协议认证没过:密码 / UUID 不对,或者不是这个协议的客户端
)

// Recorder 由数据面的连接追踪器实现。
type Recorder interface {
	NoteFailure(ip, inbound, protocol, reason, detail string)
}

// FromContext 取当前数据面的记录器;没有返回 nil。
func FromContext(ctx context.Context) Recorder { return service.FromContext[Recorder](ctx) }

// Register NewBox 里注册(要在入站构造之前)。
func Register(ctx context.Context, r Recorder) { service.MustRegister[Recorder](ctx, r) }

// Note 记一笔。r 为 nil、来源无效时什么都不做;客户端什么都没发就断开(端口扫描、TCP 连通探测 —— 包括面板自己的
// 大陆连通 / IPv6 入站检测)、握手中途被重置或超时,多半不是用户的客户端,也不记。
// 普通 TLS 什么都没收到时库报的就是 EOF / 重置 / 超时,这里按 quiet 认;REALITY 不是这样,握手前要用 Watch 套一层、失败时走 NoteTLS。
func Note(r Recorder, source M.Socksaddr, inbound, protocol, reason string, err error) {
	if r == nil || !source.IsIP() || quiet(err) {
		return
	}
	detail := ""
	if err != nil {
		detail = err.Error()
		if len(detail) > 160 {
			detail = detail[:160] + "…"
		}
	}
	r.NoteFailure(source.Addr.Unmap().String(), inbound, protocol, reason, detail)
}

// NoteAddr 同 Note,来源是 "ip:port" 字符串(HTTP/3 请求的 RemoteAddr)。
func NoteAddr(r Recorder, remote, inbound, protocol, reason, detail string) {
	if r == nil {
		return
	}
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return
	}
	r.NoteFailure(ap.Addr().Unmap().String(), inbound, protocol, reason, detail)
}

// TLSRecord TLS 记录的第一个字节(握手记录,ClientHello 就在里面)。
const TLSRecord = 0x16

// Watch 入站在 TLS / REALITY 握手前调:REALITY 时给连接套一层,记下客户端发来的第一个字节,握手失败时交给 NoteTLS 判断记不记;
// 普通 TLS 原样返回、观察器为 nil(什么都没收到时库自己报 EOF / 重置 / 超时,Note 认得出)。
//
// 为什么 REALITY 要单独看:它把任何没过 REALITY 认证的连接整条转给伪装目标,最后一律返回 "REALITY: processed invalid connection",
// 连上就断(端口扫描、Globalping 的 TCP 连通探测 —— 包括面板自己每轮的大陆连通 / IPv6 入站检测)也一样,错误里看不出对端发没发过东西。
// 包装只看读到的第一个字节,不改读到的内容、不改时序;对 sing 的拆包(UnwrapReader / CastReader / UnwrapWriter)透明,
// kTLS、vision 这类按类型找底层 TCP 连接的地方照样找得到;转给伪装目标时的 io.Copy 也照样走底层连接的 splice。
func Watch(conn net.Conn, reality bool) (net.Conn, *Watched) {
	if !reality {
		return conn, nil
	}
	w := &Watched{Conn: conn}
	return w, w
}

// Watched 见 Watch。
type Watched struct {
	net.Conn
	first atomic.Uint32 // 0 = 还没收到任何字节;否则 0x100 | 第一个字节
}

func (c *Watched) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.first.Load() == 0 {
		c.first.CompareAndSwap(0, 0x100|uint32(p[0]))
	}
	return n, err
}

// First 客户端发来的第一个字节;ok = false 表示一个字节都没收到。
func (c *Watched) First() (b byte, ok bool) {
	v := c.first.Load()
	return byte(v), v != 0
}

// WriteTo / ReadFrom:REALITY 把连接转给伪装目标时两头 io.Copy,交给底层连接(Linux 上是 splice),和不包装时一样。
// 走到这一步时 ClientHello 那几个字节已经经 Read 读过、看过了。
func (c *Watched) WriteTo(w io.Writer) (int64, error) {
	if wt, ok := c.Conn.(io.WriterTo); ok {
		return wt.WriteTo(w)
	}
	return io.Copy(w, struct{ io.Reader }{c})
}

func (c *Watched) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := c.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{c.Conn}, r)
}

func (c *Watched) Upstream() any           { return c.Conn }
func (c *Watched) ReaderReplaceable() bool { return true }
func (c *Watched) WriterReplaceable() bool { return true }

// NoteTLS TLS / REALITY 握手失败时调(w 是 Watch 返回的观察器,普通 TLS 为 nil,就是 Note(…, TLS, err))。REALITY 线路上:
//   - 一个字节都没收到就断的不记(连上就断的探测,包括面板自己的检测);
//   - 收到的第一个字节不是 TLS 记录(0x16)的不记:REALITY 把它整条转给伪装目标,是扫描器在探 HTTP 一类,不是配错了的客户端;
//   - 收到了 TLS 记录还没过(REALITY 公钥 / short_id / SNI 不对,或者就是普通 TLS 客户端)才记;
//   - 伪装目标(dest)连不上的照记:这时谁都连不上,原因里写着 failed to dial dest,是服务器这边要修的。
func NoteTLS(r Recorder, w *Watched, source M.Socksaddr, inbound, protocol string, err error) {
	if w != nil && !strings.HasPrefix(errText(err), "REALITY: failed to dial dest") {
		if b, ok := w.First(); !ok || b != TLSRecord {
			return
		}
	}
	Note(r, source, inbound, protocol, TLS, err)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// quiet 对端没发东西就走了、或握手被掐断:不是"配置不对",不记。
func quiet(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout()) || E.IsClosed(err)
}

type markKey struct{}

// Mark 一条连接有没有进到路由(协议认证过了)。服务层在认证之后才调入站的处理函数,处理函数里 Pass。
type Mark struct{ passed atomic.Bool }

// WithMark 交给协议服务的 ctx 里放一个标记;服务返回错误时看标记,没进到路由的才算"认证没过"
// (trojan 的 mux、anytls 的会话在认证之后也可能返回错误,那些不算)。
func WithMark(ctx context.Context) (context.Context, *Mark) {
	m := &Mark{}
	return context.WithValue(ctx, markKey{}, m), m
}

// Pass 入站的处理函数(认证之后)调:标记这条连接进到了路由。
func Pass(ctx context.Context) {
	if m, _ := ctx.Value(markKey{}).(*Mark); m != nil {
		m.passed.Store(true)
	}
}

// Passed 认证过了没有。
func (m *Mark) Passed() bool { return m != nil && m.passed.Load() }
