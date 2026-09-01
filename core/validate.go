package core

import (
	"context"
	"encoding/json"
	"fmt"

	sb "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
)

func validationCtx() context.Context {
	return sb.Context(context.Background(),
		InboundRegistry(), OutboundRegistry(), EndpointRegistry(),
		DNSTransportRegistry(), ServiceRegistry(), CertificateProviderRegistry())
}

// ParseConfig 只做解析(结构/字段名/类型),不构造任何对象。
func ParseConfig(raw []byte) error {
	var opt option.Options
	return opt.UnmarshalJSONContext(validationCtx(), raw)
}

// ValidateConfig 做"干跑":解析 + 构造整个 Box(创建全部入站/出站对象)但不启动、
// 不监听端口,随即关闭。能抓到解析层抓不到的错误(如 tuic 的 uuid 格式、证书文件缺失),
// 且对运行中的数据面无副作用。
func ValidateConfig(raw []byte) error {
	ctx := validationCtx()
	var opt option.Options
	if err := opt.UnmarshalJSONContext(ctx, raw); err != nil {
		return err
	}
	// NewBox 会覆盖包级日志工厂;干跑后恢复,避免影响运行中实例的热更新日志。
	savedFactory := factory
	defer func() { factory = savedFactory }()

	box, err := NewBox(Options{Context: ctx, Options: opt})
	if err != nil {
		return err
	}
	_ = box.Close()
	return nil
}

// ValidateOutbound 隔离校验单个出站:只把它和 direct 放进一份最小配置干跑,
// 不受其他对象或证书状态影响。用于保存上游前拦住会拖垮数据面的坏配置。
func ValidateOutbound(outbound json.RawMessage) error {
	cfg, _ := json.Marshal(map[string]interface{}{
		"log": map[string]interface{}{"level": "error"},
		"outbounds": []json.RawMessage{
			json.RawMessage(`{"type":"direct","tag":"direct"}`),
			outbound,
		},
	})
	if err := ValidateConfig(cfg); err != nil {
		return fmt.Errorf("sing-box 拒绝该上游配置: %w", err)
	}
	return nil
}

// ValidateInbound 结构校验单个入站的用户可编辑参数(不含 TLS 与用户)。
// hy2/anytls 入站离开 TLS 无法构造,故这里只做解析层校验:字段名、类型、枚举值。
func ValidateInbound(inbound json.RawMessage) error {
	cfg, _ := json.Marshal(map[string]interface{}{
		"inbounds":  []json.RawMessage{inbound},
		"outbounds": []json.RawMessage{json.RawMessage(`{"type":"direct","tag":"direct"}`)},
	})
	if err := ParseConfig(cfg); err != nil {
		return fmt.Errorf("sing-box 拒绝该线路参数: %w", err)
	}
	return nil
}
