package runner

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/reach"
	"github.com/Maoyangui/m-ui/render"
)

// Reach 暴露大陆连通检测(服务器页展示与手动检测)。
func (r *Runner) Reach() *reach.Service { return r.reach }

// 这些协议的入站是 TCP,能直接拿线路端口去连;hysteria2、tuic 是 UDP,测不了端口。
var tcpProtocols = map[string]bool{"anytls": true, "trojan": true, "vless": true, "vmess": true, "shadowsocks": true, "socks": true, "http": true, "mixed": true}

// reachTargets 列出要测的服务器:地址优先用手填的连接地址(IP),其次探测到的公网 IP,最后解析域名;
// 端口优先用部署在这台上、订阅里走 IPv4 的第一条 TCP 线路(用户实际连的就是它),没有就用订阅端口(本机)或面板 API 端口(副机)。
// 订阅里走 IPv6 的线路(含自定义地址写成 IPv6 的)不拿来测 IPv4 地址(端口是同一个、IPv4 照样通,测出"通"却不是用户连的那条路),
// 而是另测一项 IPv6 入站(Target.V6):测它在订阅里的 IPv6 地址 + 第一条走 IPv6 的 TCP 线路的端口。
// 这台的线路在订阅里都不走 IPv4 时大陆那项不测(NoPort:灰色、不告警,会盖掉以前测出的结论)。
func (r *Runner) reachTargets(ctx context.Context) []reach.Target {
	var nodes []model.Node
	r.db.Where("enabled = ?", true).Order("sort asc, id asc").Find(&nodes)
	var lines []model.Line
	r.db.Where("enabled = ?", true).Order("sort asc, id asc").Find(&lines)
	domainMode := strings.EqualFold(r.setting("subServerAddr"), "domain")
	enabled := make([]uint, 0, len(nodes))
	for _, n := range nodes {
		enabled = append(enabled, n.Id)
	}
	out := make([]reach.Target, 0, len(nodes))
	for _, n := range nodes {
		t := reach.Target{NodeId: n.Id, Name: n.Name}
		a := r.nodeSubAddrs(n, domainMode)
		anyV4, anyV6, anyV6TCP := false, false, false
		var v6 *reach.V6Target // 第一条走 IPv6 的 TCP 线路:先用订阅给这台自己 IPv6 的,其次是自定义地址里写明属于这台的 IPv6
		v6Custom := false
		var v6UDP []string
		for _, l := range lines {
			if l.Port <= 0 || !render.LineOnNode(l, n.Id) {
				continue
			}
			u := a.lineUse(l, n.Id, onlyOn(l, n.Id, enabled))
			if u.v6 {
				anyV6 = true
				switch {
				case render.UDPOnly(l.Protocol):
					v6UDP = append(v6UDP, l.Name)
				case tcpProtocols[l.Protocol]:
					anyV6TCP = true
					custom := hasCustomAddrs(l)
					if u.host6 != "" && (v6 == nil || (v6Custom && !custom)) {
						v6, v6Custom = &reach.V6Target{Host: u.host6, Port: u.port6, PortLine: l.Name}, custom
					}
				}
			}
			if !u.v4 {
				continue
			}
			anyV4 = true
			if t.Port == 0 && tcpProtocols[l.Protocol] {
				t.Port, t.PortFrom, t.PortLine = l.Port, "line", l.Name
			}
		}
		switch {
		case a.defV6 && !anyV4:
			t.NoPort = "订阅里这台用的是 IPv6 地址,大陆连通检测暂只支持 IPv4"
		case anyV6 && !anyV4:
			t.NoPort = "这台的线路在订阅里都走 IPv6,大陆连通检测暂只支持 IPv4"
		default:
			if t.Host = r.reachHost(ctx, n); t.Host == "" {
				t.Err = "没有可测的地址:服务器还没上报公网 IP,也没填连接地址或域名"
			}
		}
		if t.Port == 0 && t.NoPort == "" {
			// 走 IPv4 的线路没有 TCP 的(只有 hysteria2 / tuic,或 TCP 线路都走 IPv6):借订阅 / 面板 API 端口看 IPv4 地址有没有被墙。
			// 面板 API 端口常常只对主机放行,香港也连不上时那一项记成"无可测端口"(见 reach.Service.check),不报"境外也不通"
			t.TCPv6Only = anyV6TCP && t.Err == "" // 大陆那项不测时不用解释端口
			if n.IsLocal {
				t.Port, t.PortFrom = r.settingIntOr("subPort", 2056), "sub"
			} else if p := apiPort(n.ApiUrl); p > 0 {
				t.Port, t.PortFrom = p, "api"
			}
		}
		if v6 != nil || len(v6UDP) > 0 {
			if v6 == nil {
				v6 = &reach.V6Target{Host: a.ip6}
			}
			v6.UDPLines = v6UDP
			if !publicIPv6(v6.Host) {
				v6.Err = "这台订阅里的 IPv6 地址不是公网地址(内网或保留段),测点连不到"
			}
			t.V6 = v6
		}
		out = append(out, t)
	}
	return out
}

// subAddrs 订阅里这台服务器给的地址:没单独选 IP 版本的线路用哪一族(defV6),以及两族各自的地址(和 sub.EntriesFromNodes 一致)。
type subAddrs struct {
	defV6    bool   // 没单独选的线路给 IPv6:手填了 v6 地址,或留空而自动地址落在 IPv6(选了 IPv6 / 纯 IPv6 机器)
	ip4, ip6 string // 单独选了 IPv4 / IPv6 的线路用的地址(model.FamilyIPs)
}

// nodeSubAddrs 算出这台的 subAddrs。订阅给域名时客户端自己解析,没单独选的线路算不出地址族,按 IPv4 处理
// (和以前一样测解析出的 IPv4);单独选了 IP 版本的线路在域名模式下照样直接给 IP。
func (r *Runner) nodeSubAddrs(n model.Node, domainMode bool) subAddrs {
	pub, pub6 := n.PublicIP, n.PublicIP6
	if n.IsLocal {
		pub, pub6 = r.setting("publicIp"), r.setting("publicIp6")
	}
	var a subAddrs
	a.ip4, a.ip6 = model.FamilyIPs(n.Addr, pub, pub6)
	if domainMode {
		return a
	}
	if addr := model.BareHost(n.Addr); addr != "" {
		ip := net.ParseIP(addr)
		a.defV6 = ip != nil && ip.To4() == nil
		return a
	}
	_, fam := model.AutoIP(pub, pub6, n.AddrFamily)
	a.defV6 = fam == "v6"
	return a
}

// lineFamily 这条线路在这台服务器上,订阅里给的是哪一族(和 sub.resolveAddrs 一致):单独选了、且这台有那一族的地址就用它,
// 否则跟这台的默认。不看自定义地址(见 lineUse)。
func (a subAddrs) lineFamily(l model.Line, nodeID uint) string {
	switch model.ParseNodeFamily(l.NodeFamily)[nodeID] {
	case "v6":
		if a.ip6 != "" {
			return "v6"
		}
	case "v4":
		if a.ip4 != "" {
			return "v4"
		}
	}
	if a.defV6 {
		return "v6"
	}
	return "v4"
}

// lineUse 这条线路在这台服务器上,订阅里给出的地址是哪一族(和 sub.resolveAddrs 一致),以及走 IPv6 时测哪个地址、哪个端口。
type lineUse struct {
	v4, v6 bool   // 给 IPv4(或域名:由客户端解析,和以前一样按 IPv4 一侧算)/ 给 IPv6
	host6  string // 走 IPv6 时测的地址:这台在订阅里的 IPv6;自定义地址里写明属于这台的 IPv6。认不出属于这台(可能是中转)就空,不测
	port6  int
}

// lineUse 见 lineUse 类型。自定义地址整体替换服务器地址:这台能拿到的是 node_id 写这台的、和没写 node_id 的(总是给);
// 写成 IPv6 字面量的按 IPv6 算(端口不拿去测这台的 IPv4)。只有 node_id 写明是这台、或线路只部署在这台时,
// 才把那个 IPv6 当成这台的入站地址去测(没写 node_id 的可能是中转、CDN 的地址,测出来也不是这台)。
// 自定义地址都写给了别的服务器时这台在订阅里没有这条线路的入口,和以前一样只借它的端口测这台的 IPv4。
func (a subAddrs) lineUse(l model.Line, nodeID uint, onlyHere bool) lineUse {
	custom := customAddrs(l)
	if len(custom) == 0 {
		if a.lineFamily(l, nodeID) == "v6" {
			return lineUse{v6: true, host6: a.ip6, port6: l.Port}
		}
		return lineUse{v4: true}
	}
	var u lineUse
	mine := false
	for _, c := range custom {
		if c.NodeId != 0 && c.NodeId != nodeID {
			continue
		}
		mine = true
		ip, err := netip.ParseAddr(model.BareHost(c.Server))
		if err != nil || ip.Unmap().Is4() {
			u.v4 = true // IPv4 或域名
			continue
		}
		u.v6 = true
		if u.host6 == "" && (c.NodeId == nodeID || onlyHere) {
			u.host6, u.port6 = ip.String(), c.ServerPort
			if u.port6 <= 0 {
				u.port6 = l.Port
			}
		}
	}
	if !mine {
		u.v4 = true
	}
	return u
}

// customAddr 线路自定义地址里的一条(和 sub.resolveAddrs 读法一致:键名不分大小写,读不出的字段当没写)。
type customAddr struct {
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
	NodeId     uint   `json:"node_id"`
}

func customAddrs(l model.Line) []customAddr {
	if !hasCustomAddrs(l) {
		return nil
	}
	var list []customAddr
	_ = json.Unmarshal(l.Addrs, &list)
	return list
}

func hasCustomAddrs(l model.Line) bool {
	s := strings.TrimSpace(string(l.Addrs))
	return s != "" && s != "null" && s != "[]"
}

// onlyOn 这条线路是不是只部署在这一台上(勾了这一台,或"全部服务器"而启用的只有这一台)。
func onlyOn(l model.Line, nodeID uint, enabled []uint) bool {
	var ids []uint
	if len(l.NodeIds) > 0 {
		_ = json.Unmarshal(l.NodeIds, &ids)
	}
	if len(ids) == 0 {
		ids = enabled
	}
	n := 0
	for _, id := range ids {
		if slices.Contains(enabled, id) {
			if id != nodeID {
				return false
			}
			n++
		}
	}
	return n > 0
}

// publicIPv6 是公网 IPv6(全局单播,不是内网 fc00::/7、链路本地、回环)。
func publicIPv6(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	return ip != nil && ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate()
}

// 连接地址可能故意填成 127.0.0.1(不做入站的主机让订阅里它那组节点指向本机),
// 这种地址测不了、也不代表这台服务器,跳过它去用探测到的公网 IP。
func (r *Runner) reachHost(ctx context.Context, n model.Node) string {
	if ip := publicIPv4(n.Addr); ip != "" {
		return ip
	}
	pub := n.PublicIP
	if n.IsLocal && publicIPv4(pub) == "" {
		pub = r.setting("publicIp")
	}
	if ip := publicIPv4(pub); ip != "" {
		return ip
	}
	// 域名在主机上解析成 IPv4 再测:让大陆测点自己解析会撞上 DNS 污染,测出来的就不是这台服务器了
	for _, host := range []string{n.Addr, n.Domain, localDomain(r, n)} {
		host = strings.TrimSpace(host)
		if host == "" || net.ParseIP(host) != nil {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		ips, err := net.DefaultResolver.LookupIP(c, "ip4", host)
		cancel()
		if err != nil {
			continue
		}
		for _, ip := range ips {
			if s := publicIPv4(ip.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

func localDomain(r *Runner, n model.Node) string {
	if n.IsLocal {
		return r.setting("webDomain")
	}
	return ""
}

// publicIPv4 是公网 IPv4 才原样返回;回环、内网、链路本地、运营商级 NAT、组播、未指定地址一律当没有。
func publicIPv4(s string) string {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || ip.To4() == nil {
		return ""
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || cgnat.Contains(ip) {
		return ""
	}
	return ip.To4().String()
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func (r *Runner) settingIntOr(key string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(r.setting(key))); err == nil && n > 0 && n < 65536 {
		return n
	}
	return def
}

// apiPort 副机 API 地址里的端口;没写端口按协议默认。
func apiPort(raw string) int {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return 0
	}
	if p, err := strconv.Atoi(u.Port()); err == nil && p > 0 && p < 65536 {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return 443
	case "http":
		return 80
	}
	return 0
}
