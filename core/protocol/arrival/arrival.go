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
