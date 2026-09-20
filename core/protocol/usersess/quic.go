package usersess

import (
	"context"
	"crypto/rand"
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
}

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
	return &trackingListener{accept: l.Accept, close: l.Close, addr: l.Addr, reg: t.reg}, nil
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
	return &trackingListener{accept: l.Accept, close: l.Close, addr: l.Addr, reg: t.reg}, nil
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
	accept func(context.Context) (*quic.Conn, error)
	close  func() error
	addr   func() net.Addr
	reg    *Registry
}

func (l *trackingListener) Accept(ctx context.Context) (*quic.Conn, error) {
	conn, err := l.accept(ctx)
	if err != nil {
		return nil, err
	}
	sess := l.reg.Begin(func() { go conn.CloseWithError(0, "") }, func() netip.AddrPort { return addrPortOf(conn.RemoteAddr()) })
	if sl := slotOf(conn.Context()); sl != nil {
		sl.Store(sess)
	}
	if !sess.Closed() {
		context.AfterFunc(conn.Context(), func() { l.reg.End(sess) })
	}
	return conn, nil
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
