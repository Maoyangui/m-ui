package core

import (
	"context"

	sb "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
)

// ParseConfig 用 m-ui 启用的协议注册表解析一份 sing-box 配置,不启动任何监听。
// 渲染器的正确性闸门:解析通过即代表每条线路/上游都是合法的 sing-box 配置。
func ParseConfig(raw []byte) error {
	ctx := sb.Context(context.Background(),
		InboundRegistry(), OutboundRegistry(), EndpointRegistry(),
		DNSTransportRegistry(), ServiceRegistry(), CertificateProviderRegistry())
	var opt option.Options
	return opt.UnmarshalJSONContext(ctx, raw)
}
