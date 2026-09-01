package core

import (
	suiAnytls "github.com/fangjunsheng555/m-ui/core/protocol/anytls"
	suiHysteria2 "github.com/fangjunsheng555/m-ui/core/protocol/hysteria2"
	"github.com/fangjunsheng555/m-ui/util/common"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/shadowsocks"
	sbCommon "github.com/sagernet/sing/common"
)

// UpdateInboundUsers 在不重建监听 socket 的前提下热替换入站用户表。
// 返回 handled=true 表示该协议支持原地更新;false 表示调用方需回退到 Remove+Add。
func (c *Core) UpdateInboundUsers(config []byte) (bool, error) {
	if !c.isRunning {
		return false, common.NewError("sing-box is not running")
	}
	var inboundConfig option.Inbound
	err := inboundConfig.UnmarshalJSONContext(c.GetCtx(), config)
	if err != nil {
		return false, err
	}
	inb, found := inbound_manager.Get(inboundConfig.Tag)
	if !found {
		return false, nil
	}
	switch options := inboundConfig.Options.(type) {
	case *option.Hysteria2InboundOptions:
		if in, ok := inb.(*suiHysteria2.Inbound); ok {
			return true, in.UpdateUsers(options.Users)
		}
	case *option.AnyTLSInboundOptions:
		if in, ok := inb.(*suiAnytls.Inbound); ok {
			return true, in.UpdateUsers(options.Users)
		}
	case *option.ShadowsocksInboundOptions:
		if options.Managed || len(options.Users) == 0 {
			return false, nil
		}
		if in, ok := inb.(*shadowsocks.MultiInbound); ok {
			return true, in.UpdateUsers(sbCommon.Map(options.Users, func(it option.ShadowsocksUser) string {
				return it.Name
			}), sbCommon.Map(options.Users, func(it option.ShadowsocksUser) string {
				return it.Password
			}))
		}
	}
	return false, nil
}
