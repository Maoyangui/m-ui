package core

import (
	suiAnytls "github.com/Maoyangui/m-ui/core/protocol/anytls"
	suiHysteria2 "github.com/Maoyangui/m-ui/core/protocol/hysteria2"
	suiTrojan "github.com/Maoyangui/m-ui/core/protocol/trojan"
	suiTuic "github.com/Maoyangui/m-ui/core/protocol/tuic"
	suiVless "github.com/Maoyangui/m-ui/core/protocol/vless"
	suiVmess "github.com/Maoyangui/m-ui/core/protocol/vmess"

	sbCertificate "github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/protocol/anytls"
	"github.com/sagernet/sing-box/protocol/block"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing-box/protocol/group"
	"github.com/sagernet/sing-box/protocol/http"
	"github.com/sagernet/sing-box/protocol/hysteria2"
	"github.com/sagernet/sing-box/protocol/mixed"
	"github.com/sagernet/sing-box/protocol/shadowsocks"
	"github.com/sagernet/sing-box/protocol/socks"
	"github.com/sagernet/sing-box/protocol/trojan"
	"github.com/sagernet/sing-box/protocol/tuic"
	"github.com/sagernet/sing-box/protocol/vless"
	"github.com/sagernet/sing-box/protocol/vmess"
	_ "github.com/sagernet/sing-box/transport/v2rayquic"
)

// m-ui 启用的协议:
//   入站:hysteria2 / anytls / tuic / trojan / vless / vmess(带热换用户的 fork)、
//         shadowsocks(原生多用户)、socks / http / mixed(原生,换用户需重建入站)
//   出站:direct / block / socks / http / shadowsocks / tuic / hysteria2 / trojan / vless / vmess / anytls
//         + selector / urltest(多入口自适应用)
// 其余 sing-box 协议、endpoints、services 不注册,保持依赖树最小、无 CGO。

func InboundRegistry() *inbound.Registry {
	registry := inbound.NewRegistry()
	direct.RegisterInbound(registry)
	socks.RegisterInbound(registry)
	http.RegisterInbound(registry)
	mixed.RegisterInbound(registry)
	shadowsocks.RegisterInbound(registry)
	suiAnytls.RegisterInbound(registry)
	suiHysteria2.RegisterInbound(registry)
	suiTuic.RegisterInbound(registry)
	suiTrojan.RegisterInbound(registry)
	suiVless.RegisterInbound(registry)
	suiVmess.RegisterInbound(registry)
	return registry
}

func OutboundRegistry() *outbound.Registry {
	registry := outbound.NewRegistry()
	direct.RegisterOutbound(registry)
	block.RegisterOutbound(registry)
	group.RegisterSelector(registry)
	group.RegisterURLTest(registry)
	socks.RegisterOutbound(registry)
	http.RegisterOutbound(registry)
	shadowsocks.RegisterOutbound(registry)
	tuic.RegisterOutbound(registry)
	hysteria2.RegisterOutbound(registry)
	trojan.RegisterOutbound(registry)
	vless.RegisterOutbound(registry)
	vmess.RegisterOutbound(registry)
	anytls.RegisterOutbound(registry)
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
