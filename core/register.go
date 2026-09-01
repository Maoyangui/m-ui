package core

import (
	suiAnytls "github.com/fangjunsheng555/m-ui/core/protocol/anytls"
	suiHysteria2 "github.com/fangjunsheng555/m-ui/core/protocol/hysteria2"

	sbCertificate "github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/protocol/block"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing-box/protocol/group"
	"github.com/sagernet/sing-box/protocol/hysteria2"
	"github.com/sagernet/sing-box/protocol/shadowsocks"
	"github.com/sagernet/sing-box/protocol/socks"
	"github.com/sagernet/sing-box/protocol/tuic"
)

// m-ui 只启用实际用到的协议:
//   入站:hysteria2 / anytls / shadowsocks(前两者用带热更新的 fork)
//   出站:direct / block / socks(warp 本地代理) / shadowsocks / tuic,外加 selector/urltest(双入口自适应)
// 其余 sing-box 协议、endpoints、services、DNS 高级传输一律不注册,
// 使依赖树最小、无 CGO、单静态二进制。

func InboundRegistry() *inbound.Registry {
	registry := inbound.NewRegistry()
	direct.RegisterInbound(registry)
	shadowsocks.RegisterInbound(registry)
	suiAnytls.RegisterInbound(registry)
	suiHysteria2.RegisterInbound(registry)
	return registry
}

func OutboundRegistry() *outbound.Registry {
	registry := outbound.NewRegistry()
	direct.RegisterOutbound(registry)
	block.RegisterOutbound(registry)
	group.RegisterSelector(registry)
	group.RegisterURLTest(registry)
	socks.RegisterOutbound(registry)
	shadowsocks.RegisterOutbound(registry)
	tuic.RegisterOutbound(registry)
	hysteria2.RegisterOutbound(registry)
	return registry
}

func EndpointRegistry() *endpoint.Registry {
	return endpoint.NewRegistry()
}

func DNSTransportRegistry() *dns.TransportRegistry {
	registry := dns.NewTransportRegistry()
	transport.RegisterTCP(registry)
	transport.RegisterUDP(registry)
	transport.RegisterTLS(registry)
	transport.RegisterHTTPS(registry)
	local.RegisterTransport(registry)
	return registry
}

func ServiceRegistry() *service.Registry {
	return service.NewRegistry()
}

func CertificateProviderRegistry() *sbCertificate.Registry {
	return sbCertificate.NewRegistry()
}
