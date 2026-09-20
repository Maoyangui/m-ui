package hysteria2

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/tls"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	qtls "github.com/sagernet/sing-quic"
	"github.com/sagernet/sing-quic/hysteria"
	"github.com/sagernet/sing-quic/hysteria2"
	"github.com/sagernet/sing-quic/hysteria2/realm"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/auth"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"

	"github.com/Maoyangui/m-ui/core/protocol/usersess"
)

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[option.Hysteria2InboundOptions](registry, C.TypeHysteria2, NewInbound)
}

type Inbound struct {
	inbound.Adapter
	router    adapter.Router
	logger    log.ContextLogger
	listener  *listener.Listener
	tlsConfig tls.ServerConfig
	service   *hysteria2.Service[string]
	sessions  *usersess.Registry  // 用户 → 会话登记表:停用 / 换凭据 / 踢线时按用户关整条 QUIC 会话
	wrap      *usersess.ServerTLS // 包在 tlsConfig 外面的监听扩展点,靠它拿到 QUIC 连接
	set       *usersess.Set       // 本数据面全部入站的登记表(踢线时一次扫全部)
}

func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.Hysteria2InboundOptions) (adapter.Inbound, error) {
	options.UDPFragmentDefault = true
	if options.TLS == nil || !options.TLS.Enabled {
		return nil, C.ErrTLSRequired
	}
	tlsConfig, err := tls.NewServer(ctx, logger, common.PtrValueOrDefault(options.TLS))
	if err != nil {
		return nil, err
	}
	var salamanderPassword string
	var geckoPassword string
	var geckoMinPacketSize, geckoMaxPacketSize int
	if options.Obfs != nil {
		if options.Obfs.Password == "" {
			return nil, E.New("missing obfs password")
		}
		switch options.Obfs.Type {
		case hysteria2.ObfsTypeSalamander:
			salamanderPassword = options.Obfs.Password
		case hysteria2.ObfsTypeGecko:
			geckoPassword = options.Obfs.Password
			geckoMinPacketSize = options.Obfs.GeckoOptions.MinPacketSize
			geckoMaxPacketSize = options.Obfs.GeckoOptions.MaxPacketSize
		default:
			return nil, E.New("unknown obfs type: ", options.Obfs.Type)
		}
	}
	var masqueradeHandler http.Handler
	if options.Masquerade != nil && options.Masquerade.Type != "" {
		switch options.Masquerade.Type {
		case C.Hysterai2MasqueradeTypeFile:
			masqueradeDirectory := filemanager.BasePath(ctx, os.ExpandEnv(options.Masquerade.FileOptions.Directory))
			_, err = filemanager.ReadDir(ctx, masqueradeDirectory)
			if err != nil && !os.IsNotExist(err) {
				return nil, E.Cause(err, "read masquerade directory")
			}
			masqueradeHandler = http.FileServer(http.Dir(masqueradeDirectory))
		case C.Hysterai2MasqueradeTypeProxy:
			masqueradeURL, err := url.Parse(options.Masquerade.ProxyOptions.URL)
			if err != nil {
				return nil, E.Cause(err, "parse masquerade URL")
			}
			masqueradeHandler = &httputil.ReverseProxy{
				Rewrite: func(r *httputil.ProxyRequest) {
					r.SetURL(masqueradeURL)
					if !options.Masquerade.ProxyOptions.RewriteHost {
						r.Out.Host = r.In.Host
					}
				},
				ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
					w.WriteHeader(http.StatusBadGateway)
				},
			}
		case C.Hysterai2MasqueradeTypeString:
			masqueradeHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if options.Masquerade.StringOptions.StatusCode != 0 {
					w.WriteHeader(options.Masquerade.StringOptions.StatusCode)
				}
				for key, values := range options.Masquerade.StringOptions.Headers {
					for _, value := range values {
						w.Header().Add(key, value)
					}
				}
				w.Write([]byte(options.Masquerade.StringOptions.Content))
			})
		default:
			return nil, E.New("unknown masquerade type: ", options.Masquerade.Type)
		}
	}
	inbound := &Inbound{
		Adapter: inbound.NewAdapter(C.TypeHysteria2, tag),
		router:  router,
		logger:  logger,
		listener: listener.New(listener.Options{
			Context: ctx,
			Logger:  logger,
			Listen:  options.ListenOptions,
		}),
		tlsConfig: tlsConfig,
	}
	inbound.sessions = usersess.New(hysteriaCreds(options.Users))
	inbound.wrap = usersess.NewServerTLS(tlsConfig, inbound.sessions, true, salamanderPassword != "" || geckoPassword != "")
	inbound.set = usersess.SetFromContext(ctx)
	var udpTimeout time.Duration
	if options.UDPTimeout != 0 {
		udpTimeout = time.Duration(options.UDPTimeout)
	} else {
		udpTimeout = C.UDPTimeout
	}
	var realmOptions *realm.Options
	if options.Realm != nil {
		if options.Realm.IPVersion != 0 && options.ListenOptions.Listen != nil {
			listenAddr := netip.Addr(*options.ListenOptions.Listen).Unmap()
			if options.Realm.IPVersion == 6 && listenAddr.Is4() {
				return nil, E.New("realm.ip_version 6 conflicts with listen address ", listenAddr)
			}
			if options.Realm.IPVersion == 4 && listenAddr.Is6() && !listenAddr.IsUnspecified() {
				return nil, E.New("realm.ip_version 4 conflicts with listen address ", listenAddr)
			}
		}
		queryOptions, err := adapter.DNSQueryOptionsFrom(ctx, options.Realm.STUNDomainResolver)
		if err != nil {
			return nil, err
		}
		httpClientTransport, err := service.FromContext[adapter.HTTPClientManager](ctx).ResolveTransport(ctx, logger, common.PtrValueOrDefault(options.Realm.HTTPClient))
		if err != nil {
			return nil, E.Cause(err, "create realm http client")
		}
		dnsRouter := service.FromContext[adapter.DNSRouter](ctx)
		realmOptions = &realm.Options{
			ServerURL:   options.Realm.ServerURL,
			Token:       options.Realm.Token,
			RealmID:     options.Realm.RealmID,
			STUNServers: options.Realm.STUNServers,
			HTTPClient:  &http.Client{Transport: httpClientTransport},
			Resolver: func(ctx context.Context, host string, ipv4, ipv6 bool) ([]netip.Addr, error) {
				dnsOptions := queryOptions
				switch {
				case ipv4 && !ipv6:
					dnsOptions.Strategy = C.DomainStrategyIPv4Only
				case !ipv4 && ipv6:
					dnsOptions.Strategy = C.DomainStrategyIPv6Only
				}
				return dnsRouter.Lookup(ctx, host, dnsOptions)
			},
			Logger:    logger,
			IPVersion: options.Realm.IPVersion,
		}
		if options.Realm.PortMapping != nil && options.Realm.PortMapping.Enabled {
			realmOptions.PortMapping = &realm.PortMappingOptions{
				Timeout:  time.Duration(options.Realm.PortMapping.Timeout),
				Lifetime: time.Duration(options.Realm.PortMapping.Lifetime),
			}
		}
	}
	hysteriaService, err := hysteria2.NewService[string](hysteria2.ServiceOptions{
		Context:            ctx,
		Logger:             logger,
		BrutalDebug:        options.BrutalDebug,
		SendBPS:            uint64(options.UpMbps * hysteria.MbpsToBps),
		ReceiveBPS:         uint64(options.DownMbps * hysteria.MbpsToBps),
		SalamanderPassword: salamanderPassword,
		GeckoPassword:      geckoPassword,
		GeckoMinPacketSize: geckoMinPacketSize,
		GeckoMaxPacketSize: geckoMaxPacketSize,
		TLSConfig:          inbound.wrap, // 登记表的监听扩展点;入站自己仍持有 tlsConfig 做 Start / Close
		QUICOptions: qtls.QUICOptions{
			IdleTimeout:             options.IdleTimeout.Build(),
			KeepAlivePeriod:         options.KeepAlivePeriod.Build(),
			StreamReceiveWindow:     options.StreamReceiveWindow.Value(),
			ConnectionReceiveWindow: options.ConnectionReceiveWindow.Value(),
			MaxConcurrentStreams:    options.MaxConcurrentStreams,
			InitialPacketSize:       options.InitialPacketSize,
			DisablePathMTUDiscovery: options.DisablePathMTUDiscovery,
		},
		IgnoreClientBandwidth: options.IgnoreClientBandwidth,
		UDPTimeout:            udpTimeout,
		Handler:               inbound,
		MasqueradeHandler:     masqueradeHandler,
		BBRProfile:            options.BBRProfile,
		RealmOptions:          realmOptions,
	})
	if err != nil {
		return nil, err
	}
	userList := make([]string, 0, len(options.Users))
	userPasswordList := make([]string, 0, len(options.Users))
	for _, user := range options.Users {
		userList = append(userList, user.Name)
		userPasswordList = append(userPasswordList, user.Password)
	}
	hysteriaService.UpdateUsers(userList, userPasswordList)
	inbound.service = hysteriaService
	return inbound, nil
}

func (h *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	ctx = log.ContextWithNewID(ctx)
	var metadata adapter.InboundContext
	metadata.Inbound = h.Tag()
	metadata.InboundType = h.Type()
	//nolint:staticcheck
	metadata.InboundDetour = h.listener.ListenOptions().Detour
	//nolint:staticcheck
	metadata.OriginDestination = h.listener.UDPAddr()
	metadata.Source = source
	metadata.Destination = destination
	h.logger.InfoContext(ctx, "inbound connection from ", metadata.Source)
	if userName, _ := auth.UserFromContext[string](ctx); userName != "" {
		metadata.User = userName
		h.logger.InfoContext(ctx, "[", userName, "] inbound connection to ", metadata.Destination)
	} else {
		h.logger.InfoContext(ctx, "inbound connection to ", metadata.Destination)
	}
	if metadata.User != "" {
		// 会话已被撤销(用户被停用 / 换了凭据 / 被踢)的流不再路由:关掉整条会话,客户端得重新认证
		sess := usersess.FromQUICStream(conn)
		if !h.sessions.Admit(sess, metadata.User) {
			sess.Close()
			_ = conn.Close()
			h.logger.DebugContext(ctx, "[", metadata.User, "] session revoked, connection rejected")
			return
		}
	}
	h.router.RouteConnectionEx(ctx, conn, metadata, onClose)
}

func (h *Inbound) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	ctx = log.ContextWithNewID(ctx)
	var metadata adapter.InboundContext
	metadata.Inbound = h.Tag()
	metadata.InboundType = h.Type()
	//nolint:staticcheck
	metadata.InboundDetour = h.listener.ListenOptions().Detour
	//nolint:staticcheck
	metadata.OriginDestination = h.listener.UDPAddr()
	metadata.Source = source
	metadata.Destination = destination
	h.logger.InfoContext(ctx, "inbound packet connection from ", metadata.Source)
	if userName, _ := auth.UserFromContext[string](ctx); userName != "" {
		metadata.User = userName
		h.logger.InfoContext(ctx, "[", userName, "] inbound packet connection to ", metadata.Destination)
	} else {
		h.logger.InfoContext(ctx, "inbound packet connection to ", metadata.Destination)
	}
	if metadata.User != "" {
		// UDP 流拿不到 QUIC 流的上下文,按对端地址找会话;找不到只查名字
		sess := h.sessions.FindByRemote(metadata.User, source.AddrPort())
		if !h.sessions.Admit(sess, metadata.User) {
			sess.Close()
			_ = conn.Close()
			h.logger.DebugContext(ctx, "[", metadata.User, "] session revoked, packet connection rejected")
			return
		}
	}
	h.router.RoutePacketConnectionEx(ctx, conn, metadata, onClose)
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
	packetConn, err := h.listener.ListenUDP()
	if err != nil {
		return err
	}
	if err := h.service.Start(packetConn); err != nil {
		return err
	}
	if !h.wrap.Listened() {
		// 上游改了扩展点、编译不会报错:这时候登记表永远是空的,停用 / 踢线只能断内层的流。要让人看见。
		h.logger.Error("会话级断线未生效:QUIC 监听没有经过登记表,停用 / 踢线只能断内层连接(升级内核时要核对 sing-quic 的 ServerConfig 扩展点)")
	}
	h.set.Add(h.sessions) // 起来了才登记:构造成功但没起来的入站不会被 Close,加早了就永远留在里面
	return nil
}

func (h *Inbound) InterfaceUpdated(ctx context.Context) {
	h.service.Reset()
}

func (h *Inbound) Close() error {
	h.sessions.Clear() // QUIC 连接由传输层随 socket 一起销毁,登记表只关门清空
	h.set.Remove(h.sessions)
	return common.Close(
		h.listener,
		h.tlsConfig,
		common.PtrOrNil(h.service),
	)
}
