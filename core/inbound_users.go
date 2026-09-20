package core

import (
	suiAnytls "github.com/Maoyangui/m-ui/core/protocol/anytls"
	suiHysteria2 "github.com/Maoyangui/m-ui/core/protocol/hysteria2"
	suiTrojan "github.com/Maoyangui/m-ui/core/protocol/trojan"
	suiTuic "github.com/Maoyangui/m-ui/core/protocol/tuic"
	suiVless "github.com/Maoyangui/m-ui/core/protocol/vless"
	suiVmess "github.com/Maoyangui/m-ui/core/protocol/vmess"
	"github.com/Maoyangui/m-ui/util/common"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/shadowsocks"
	sbCommon "github.com/sagernet/sing/common"
)

// UpdateInboundUsers 在不重建监听 socket 的前提下热替换入站用户表。
// 返回 handled=true 表示该协议支持原地更新;false 表示调用方需回退到 Remove+Add。
// closed 是随之关掉的**整条会话**数(hysteria2 / tuic / anytls 会话只鉴权一次,被移除或换了凭据的用户的会话必须整条关掉,
// 只断里面的流客户端会马上再开一条);其它协议每条连接各自鉴权,没有这个问题,恒为 0。
func (c *Core) UpdateInboundUsers(config []byte) (handled bool, closed int, err error) {
	if !c.isRunning {
		return false, 0, common.NewError("sing-box is not running")
	}
	var inboundConfig option.Inbound
	if err := inboundConfig.UnmarshalJSONContext(c.GetCtx(), config); err != nil {
		return false, 0, err
	}
	inb, found := inbound_manager.Get(inboundConfig.Tag)
	if !found {
		return false, 0, nil
	}
	switch options := inboundConfig.Options.(type) {
	case *option.Hysteria2InboundOptions:
		if in, ok := inb.(*suiHysteria2.Inbound); ok {
			n, err := in.UpdateUsers(options.Users)
			return true, n, err
		}
	case *option.AnyTLSInboundOptions:
		if in, ok := inb.(*suiAnytls.Inbound); ok {
			n, err := in.UpdateUsers(options.Users)
			return true, n, err
		}
	case *option.TUICInboundOptions:
		if in, ok := inb.(*suiTuic.Inbound); ok {
			n, err := in.UpdateUsers(options.Users)
			return true, n, err
		}
	case *option.TrojanInboundOptions:
		if in, ok := inb.(*suiTrojan.Inbound); ok {
			return true, 0, in.UpdateUsers(options.Users)
		}
	case *option.VLESSInboundOptions:
		if in, ok := inb.(*suiVless.Inbound); ok {
			return true, 0, in.UpdateUsers(options.Users)
		}
	case *option.VMessInboundOptions:
		if in, ok := inb.(*suiVmess.Inbound); ok {
			return true, 0, in.UpdateUsers(options.Users)
		}
	case *option.ShadowsocksInboundOptions:
		if options.Managed || len(options.Users) == 0 {
			return false, 0, nil
		}
		if in, ok := inb.(*shadowsocks.MultiInbound); ok {
			return true, 0, in.UpdateUsers(sbCommon.Map(options.Users, func(it option.ShadowsocksUser) string {
				return it.Name
			}), sbCommon.Map(options.Users, func(it option.ShadowsocksUser) string {
				return it.Password
			}))
		}
	}
	return false, 0, nil
}
