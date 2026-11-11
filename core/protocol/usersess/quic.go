package usersess

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"

	"github.com/sagernet/quic-go"
	"github.com/sagernet/quic-go/http3"
	qtls "github.com/sagernet/sing-quic"
	aTLS "github.com/sagernet/sing/common/tls"
)

// hy2 / TUIC 怎么拿到 QUIC 连接:sing-quic 把 *quic.Conn 藏在库内部,交给入站的只有流。
// 但它的 ListenWithOptions / ListenEarlyWithOptions / ConfigureHTTP3 都会先看 TLS 配置有没有实现 qtls.ServerConfig,
// 有就把"建 QUIC 监听"这件事交给它。sing-box 自己没有任何实现,这条路是空的 —— 我们在这里实现它:
// 自己建 quic.Transport(挂上 ConnContext 放槽位),Accept 到连接就登记会话,之后按用户关连接。
// 不改库、不复制库代码;握手、传输参数、socket 优化与库的默认路径逐项相同(下面逐条照抄)。
//
// 上游哪天删掉这个扩展点,编译不会报错、只会悄悄退回默认路径:所以有编译期断言守接口签名,
// 入站启动后再查一次 Listened(),没生效就打 Error 日志(见各入站的 Start)。

var (
	_ qtls.ServerConfig  = (*ServerTLS)(nil)
	_ qtls.Listener      = (*trackingListener)(nil)
	_ qtls.EarlyListener = (*trackingListener)(nil)
)

// ServerTLS 包在入站的 TLS 配置外面,实现 qtls.ServerConfig。
// 内嵌原配置:NextProtos / SetNextProtos / HandshakeTimeout / STDConfig / Start / Close 全部透传。
type ServerTLS struct {
	aTLS.ServerConfig
	reg      *Registry
	hy2      bool // hysteria2:库的默认路径按混淆与否设版本协商与 stateless reset;TUIC 两者都不设
	obfs     bool
	listened atomic.Bool
	unauthed func(netip.AddrPort) // 见 OnUnauthed
}

// OnUnauthed 握手完成、却没通过协议认证就被服务端关掉的 QUIC 连接回调一次(给"最近入站连接"记"到了但认证没过")。
// 只给 TUIC 用:sing-quic 的 TUIC 认证失败(UUID / 密码不对)或超时没认证时,自己关掉连接、不经过入站;
// 我们能看到的是连接结束时 —— 从来没开过流(登记表里没绑上用户)、不是我们撤销的、也不是对端关的(本端 ApplicationError)。
// hysteria2 认证失败不关连接(交给伪装站点),在入站里另外认。要在开始监听之前设。
func (t *ServerTLS) OnUnauthed(fn func(netip.AddrPort)) { t.unauthed = fn }

// NewServerTLS inner 是入站原来的 TLS 配置(入站自己仍要持有它做 Start / Close)。
func NewServerTLS(inner aTLS.ServerConfig, reg *Registry, hy2, obfs bool) *ServerTLS {
	return &ServerTLS{ServerConfig: inner, reg: reg, hy2: hy2, obfs: obfs}
}

// Listened 扩展点被调用过(QUIC 监听确实经我们的手建立)。
func (t *ServerTLS) Listened() bool { return t.listened.Load() }

// transport 逐行照抄 sing-quic quic.go 的默认路径(quic.Transport + SetSingleUse + 按需的 StatelessResetKey),
// 只多一项 ConnContext。
func (t *ServerTLS) transport(conn net.PacketConn) (*quic.Transport, *aTLS.STDConfig, error) {
	tr := &quic.Transport{
		Conn:                             conn,
		DisableVersionNegotiationPackets: t.hy2 && t.obfs,
		ConnContext: func(ctx context.Context, _ *quic.ClientInfo) (context.Context, error) {
			return withSlot(ctx), nil
		},
	}
	tr.SetSingleUse(true)
	if t.hy2 && !t.obfs {
		tr.StatelessResetKey = new(quic.StatelessResetKey)
		if _, err := rand.Read(tr.StatelessResetKey[:]); err != nil {
			return nil, nil, err
		}
	}
	std, err := t.ServerConfig.STDConfig()
	if err != nil {
		return nil, nil, err
	}
	return tr, std, nil
}

func (t *ServerTLS) Listen(conn net.PacketConn, config *quic.Config) (qtls.Listener, error) {
	tr, std, err := t.transport(conn)
	if err != nil {
		return nil, err
	}
	l, err := tr.Listen(std, config)
	if err != nil {
		return nil, err
	}
	t.listened.Store(true)
	t.reg.hookOK.Store(true)
	return &trackingListener{accept: l.Accept, close: l.Close, addr: l.Addr, reg: t.reg, unauthed: t.unauthed}, nil
}

func (t *ServerTLS) ListenEarly(conn net.PacketConn, config *quic.Config) (qtls.EarlyListener, error) {
	tr, std, err := t.transport(conn)
	if err != nil {
		return nil, err
	}
	l, err := tr.ListenEarly(std, config)
	if err != nil {
		return nil, err
	}
	t.listened.Store(true)
	t.reg.hookOK.Store(true)
	return &trackingListener{accept: l.Accept, close: l.Close, addr: l.Addr, reg: t.reg, unauthed: t.unauthed}, nil
}

// ConfigureHTTP3 和默认路径一样只为它的副作用(会话票据);NextProtos 已由 qtls.ConfigureHTTP3 预先设好。
func (t *ServerTLS) ConfigureHTTP3() {
	if std, err := t.ServerConfig.STDConfig(); err == nil {
		http3.ConfigureTLSConfig(std)
	}
}

// trackingListener 每 Accept 到一条 QUIC 连接就登记一条会话,连接结束时注销。
// 错误原样返回:sing-quic 靠 errors.Is(err, quic.ErrServerClosed) 判断监听是否关闭。
type trackingListener struct {
	accept   func(context.Context) (*quic.Conn, error)
	close    func() error
	addr     func() net.Addr
	reg      *Registry
	unauthed func(netip.AddrPort)
}

func (l *trackingListener) Accept(ctx context.Context) (*quic.Conn, error) {
	conn, err := l.accept(ctx)
	if err != nil {
		return nil, err
	}
	// closer 不能阻塞(Accept 与流的处理协程里也会调),只起协程发 CloseWithError;连接真正关掉
	// (全部流断开、CONNECTION_CLOSE 已发出)以连接的 ctx 结束为准,换表 / 踢线靠它等到断开再返回。
	sess := l.reg.begin(func() { go conn.CloseWithError(0, "") }, conn.Context().Done(), func() netip.AddrPort { return addrPortOf(conn.RemoteAddr()) })
	if sl := slotOf(conn.Context()); sl != nil {
		sl.Store(sess)
	}
	if !sess.Closed() {
		context.AfterFunc(conn.Context(), func() {
			if l.unauthed != nil && neverAdmitted(sess, l.reg) && closedLocally(context.Cause(conn.Context())) {
				l.unauthed(addrPortOf(conn.RemoteAddr()))
			}
			l.reg.End(sess)
		})
	}
	return conn, nil
}

// neverAdmitted 这条会话从头到尾没开过流(没绑上用户),也不是我们关的(撤销 / 入站正在关)。
func neverAdmitted(sess *Session, reg *Registry) bool {
	return !sess.Closed() && sess.User() == "" && !reg.isClosed()
}

// closedLocally 连接是本端用 CloseWithError 关的(不是对端关、不是空闲超时、不是握手失败)。
func closedLocally(err error) bool {
	var ae *quic.ApplicationError
	return errors.As(err, &ae) && !ae.Remote
}

func (l *trackingListener) Close() error   { return l.close() }
func (l *trackingListener) Addr() net.Addr { return l.addr() }

func addrPortOf(a net.Addr) netip.AddrPort {
	if u, ok := a.(*net.UDPAddr); ok {
		return u.AddrPort()
	}
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.AddrPort{}
	}
	return ap
}
