package render

import (
	"encoding/json"
	"net"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
	"gorm.io/gorm"
)

// 纯 IPv6 机器的直连出口连不上任何 IPv4 地址,客户端最常用的远程 DNS(1.1.1.1、8.8.8.8 ……)也在其中:
// 直连线路上客户端连域名都解析不了。这些公共 DNS 都有同一家的 IPv6 地址,证书里两个地址都有
// (DoH / DoT 按 IP 校验也能过),所以在本机把目标换成 IPv6 地址,客户端什么都不用改。
// 只对默认走直连的线路做:经 WARP 出去的本来就通 IPv4;经别的中转出去的,那台不一定有 IPv6。
var v6DNS = [][2]string{
	{"1.1.1.1", "2606:4700:4700::1111"}, {"1.0.0.1", "2606:4700:4700::1001"}, // Cloudflare
	{"1.1.1.2", "2606:4700:4700::1112"}, {"1.0.0.2", "2606:4700:4700::1002"},
	{"1.1.1.3", "2606:4700:4700::1113"}, {"1.0.0.3", "2606:4700:4700::1003"},
	{"8.8.8.8", "2001:4860:4860::8888"}, {"8.8.4.4", "2001:4860:4860::8844"}, // Google
	{"9.9.9.9", "2620:fe::fe"}, {"149.112.112.112", "2620:fe::9"}, // Quad9
	{"208.67.222.222", "2620:119:35::35"}, {"208.67.220.220", "2620:119:53::53"}, // OpenDNS
	{"94.140.14.14", "2a10:50c0::ad1:ff"}, {"94.140.15.15", "2a10:50c0::ad2:ff"}, // AdGuard
	{"223.5.5.5", "2400:3200::1"}, {"223.6.6.6", "2400:3200:baba::1"}, // 阿里
	{"119.29.29.29", "2402:4e00::"}, // 腾讯 DNSPod
}

// PureIPv6 本机只有 IPv6:探测到的公网地址(设置 publicIp,有 IPv4 时一定是 IPv4)本身是 IPv6。
func PureIPv6(db *gorm.DB) bool {
	var v string
	db.Raw("SELECT value FROM settings WHERE key = ?", "publicIp").Scan(&v)
	ip := net.ParseIP(strings.TrimSpace(v))
	return ip != nil && ip.To4() == nil
}

// v6DNSRules 纯 IPv6 机器上,默认走直连的线路把常用公共 DNS 的 IPv4 地址换成同一家的 IPv6 地址
// (route-options 只改目标、不定出口,后面的规则照常决定走哪)。
func v6DNSRules(lines []model.Line) []json.RawMessage {
	var direct []string
	for _, l := range lines {
		if l.UpstreamId == 0 {
			direct = append(direct, l.Name)
		}
	}
	if len(direct) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(v6DNS))
	for _, p := range v6DNS {
		rule, _ := json.Marshal(map[string]interface{}{
			"inbound": direct, "ip_cidr": []string{p[0] + "/32"}, "action": "route-options", "override_address": p[1],
		})
		out = append(out, rule)
	}
	return out
}
