package render

import (
	"net"
	"strings"

	"gorm.io/gorm"
)

// localAddrs 本机网卡上的地址(测试里替换)。
var localAddrs = func() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out
}

// ownPublicCIDRs 这台服务器自己的公网地址(/32、/128):探测到的公网 IPv4 / IPv6(设置 publicIp / publicIp6,
// 主副机各自探测、各自渲染),加上网卡上的公网地址(多 IP、IPv6 一个网卡多地址;云上 NAT 的机器网卡上只有内网地址,
// 靠探测值)。内网、回环、链路本地不在内 —— 它们已经由 ip_is_private 管着。
//
// 私网屏蔽规则只拦 ip_is_private 时,用户经代理连这台服务器自己的公网 IP 仍能摸到面板、代理面板、订阅、副机接口端口,
// 而且来源是服务器自己:管理员按来源 IP 限制面板访问、云防火墙只放行自己的 IP,都被绕过。
// 拦的是全部端口而不只是这几个管理端口:和 127.0.0.1 一样,"这台服务器自己"整体不让经代理碰
// (同机的 SSH、数据库、别的程序的管理页、WARP 的 socks 口如果监听在公网地址上,同样只靠来源 IP 挡着);
// 用户本来就直连得到这台服务器的公网 IP(他就是连它来用代理的),经代理不能访问它不损失什么。
// 需要经代理访问本机的,打开「允许访问内网」(和以前一样整段不加)。
func ownPublicCIDRs(db *gorm.DB) []string {
	var ips []net.IP
	for _, k := range []string{"publicIp", "publicIp6"} {
		var v string
		db.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
		if ip := net.ParseIP(strings.TrimSpace(v)); ip != nil {
			ips = append(ips, ip)
		}
	}
	ips = append(ips, localAddrs()...)
	seen := map[string]bool{}
	var out []string
	for _, ip := range ips {
		if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
			continue
		}
		cidr := ip.String() + "/128"
		if v4 := ip.To4(); v4 != nil {
			cidr = v4.String() + "/32"
		}
		if !seen[cidr] {
			seen[cidr] = true
			out = append(out, cidr)
		}
	}
	return out
}
