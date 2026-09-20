package anytls

import (
	"context"
	"net"
	"strings"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/common/uot"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/auth"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	anytls "github.com/anytls/sing-anytls"
	"github.com/anytls/sing-anytls/padding"

	"github.com/Maoyangui/m-ui/core/protocol/usersess"
)

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.AnyTLSInboundOptions](registry, C.TypeAnyTLS, NewInbound)
}

type Inbound struct {
	inbound.Adapter
	tlsConfig tls.ServerConfig
	router    adapter.ConnectionRouterEx
	logger    logger.ContextLogger
	listener  *listener.Listener
	service   *anytls.Service
	sessions  *usersess.Registry // 用户 → 会话登记表:停用 / 换密码 / 踢线时按用户关整条 TLS 会话
	set       *usersess.Set      // 本数据面全部入站的登记表(踢线时一次扫全部)
}

func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.AnyTLSInboundOptions) (adapter.Inbound, error) {
	inbound := &Inbound{
		Adapter: inbound.NewAdapter(C.TypeAnyTLS, tag),
		router:  uot.NewRouter(router, logger),
		logger:  logger,
	}

	if options.TLS != nil && options.TLS.Enabled {
		tlsConfig, err := tls.NewServer(ctx, logger, common.PtrValueOrDefault(options.TLS))
		if err != nil {
			return nil, err
		}
		inbound.tlsConfig = tlsConfig
	}

	paddingScheme := padding.DefaultPaddingScheme
	if len(options.PaddingScheme) > 0 {
		paddingScheme = []byte(strings.Join(options.PaddingScheme, "\n"))
	}

	service, err := anytls.NewService(anytls.ServiceConfig{
		Users: common.Map(options.Users, func(it option.AnyTLSUser) anytls.User {
			return anytls.User(it)
		}),
		PaddingScheme: paddingScheme,
		Handler:       (*inboundHandler)(inbound),
		Logger:        logger,
	})
	if err != nil {
		return nil, err
	}
	inbound.service = service
	inbound.sessions = usersess.New(anytlsCreds(options.Users))
	inbound.set = usersess.SetFromContext(ctx)
	inbound.listener = listener.New(listener.Options{
		Context:           ctx,
		Logger:            logger,
		Network:           []string{N.NetworkTCP},
		Listen:            options.ListenOptions,
		ConnectionHandler: inbound,
	})
	return inbound, nil
}

func (h *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	if h.tlsConfig != nil {
		err := h.tlsConfig.Start()
		if err != nil {
			return err
		}
	}
	if err := h.listener.Start(); err != nil {
		return err
	}
	h.set.Add(h.sessions) // 起来了才登记:构造成功但没起来的入站不会被 Close
	return nil
}

// Close 先关监听再关会话:关监听之后 accept 不到新连接,再把已有的会话全关掉、登记表关门 ——
// 之后才握完手的连接登记时会被立刻关掉,不会留在已经关闭的旧数据面上继续用旧的用户表。
// (以前全量重载只关监听,已建立的 anytls 会话留在旧数据面上跑着,既不记账也踢不掉。)
func (h *Inbound) Close() error {
	err := common.Close(h.listener, h.tlsConfig)
	h.sessions.CloseAll()
	h.set.Remove(h.sessions)
	return err
}

func (h *Inbound) NewConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	raw := conn // 握手前的原始连接:关会话就是关它;不包装 conn,保住 kTLS 与各处的类型断言
	if h.tlsConfig != nil {
		tlsConn, err := tls.ServerHandshake(ctx, conn, h.tlsConfig)
		if err != nil {
			N.CloseOnHandshakeFailure(conn, onClose, err)
			h.logger.ErrorContext(ctx, E.Cause(err, "process connection from ", metadata.Source, ": TLS handshake"))
			return
		}
		conn = tlsConn
	}
	// 登记会话:service.NewConnection 会阻塞到整条会话结束,所以 End 的时机是准的。
	// 不能拿 onClose 当会话结束的信号 —— 所有流共用同一个 onClose。
	sess := h.sessions.Begin(func() { _ = raw.Close() }, nil)
	if sess.Closed() {
		return // 入站正在关:登记表已关门,会话已被关掉
	}
	defer h.sessions.End(sess)
	err := h.service.NewConnection(usersess.WithSession(adapter.WithContext(ctx, &metadata), sess), conn, metadata.Source, onClose)
	if err != nil {
		N.CloseOnHandshakeFailure(conn, onClose, err)
		h.logger.ErrorContext(ctx, E.Cause(err, "process connection from ", metadata.Source))
	}
}

type inboundHandler Inbound

func (h *inboundHandler) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	var metadata adapter.InboundContext
	metadata.Inbound = h.Tag()
	metadata.InboundType = h.Type()
	//nolint:staticcheck
	metadata.InboundDetour = h.listener.ListenOptions().Detour
	//nolint:staticcheck
	metadata.Source = source
	metadata.Destination = destination.Unwrap()
	if userName, _ := auth.UserFromContext[string](ctx); userName != "" {
		metadata.User = userName
		h.logger.InfoContext(ctx, "[", userName, "] inbound connection to ", metadata.Destination)
	} else {
		h.logger.InfoContext(ctx, "inbound connection to ", metadata.Destination)
	}
	if metadata.User != "" {
		// 会话已被撤销(用户被停用 / 换了凭据 / 被踢)的流不再路由:关掉整条会话,客户端得重新认证
		sess := usersess.FromContext(ctx)
		if !h.sessions.Admit(sess, metadata.User) {
			sess.Close()
			_ = conn.Close()
			h.logger.DebugContext(ctx, "[", metadata.User, "] session revoked, connection rejected")
			return
		}
	}
	h.router.RouteConnectionEx(ctx, conn, metadata, onClose)
}
