package runner

import (
	"context"
	"net"
	"net/url"
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
// 端口优先用部署在这台上的第一条 TCP 线路(用户实际连的就是它),没有就用订阅端口(本机)或面板 API 端口(副机)。
func (r *Runner) reachTargets(ctx context.Context) []reach.Target {
	var nodes []model.Node
	r.db.Where("enabled = ?", true).Order("sort asc, id asc").Find(&nodes)
	var lines []model.Line
	r.db.Where("enabled = ?", true).Order("sort asc, id asc").Find(&lines)
	out := make([]reach.Target, 0, len(nodes))
	for _, n := range nodes {
		t := reach.Target{NodeId: n.Id, Name: n.Name}
		if r.reachUsesIPv6(n) {
			t.Err = "订阅里这台用的是 IPv6 地址,大陆连通检测暂只支持 IPv4"
		} else if t.Host = r.reachHost(ctx, n); t.Host == "" {
			t.Err = "没有可测的地址:服务器还没上报公网 IP,也没填连接地址或域名"
		}
		for _, l := range lines {
			if l.Port > 0 && tcpProtocols[l.Protocol] && render.LineOnNode(l, n.Id) {
				t.Port, t.PortFrom, t.PortLine = l.Port, "line", l.Name
				break
			}
		}
		if t.Port == 0 {
			if n.IsLocal {
				t.Port, t.PortFrom = r.settingIntOr("subPort", 2056), "sub"
			} else if p := apiPort(n.ApiUrl); p > 0 {
				t.Port, t.PortFrom = p, "api"
			}
		}
		out = append(out, t)
	}
	return out
}

// reachUsesIPv6 订阅里这台给的是 IPv6:手填了 v6 地址,或留空而自动地址落在 IPv6(选了 IPv6 / 纯 IPv6 机器)。
// 测点不一定有 IPv6,测不通会被误判成被墙,所以这种先不测、如实标出来。
func (r *Runner) reachUsesIPv6(n model.Node) bool {
	if strings.EqualFold(r.setting("subServerAddr"), "domain") { // 订阅给域名:客户端自己解析,不看这里的地址
		return false
	}
	if addr := model.BareHost(n.Addr); addr != "" {
		ip := net.ParseIP(addr)
		return ip != nil && ip.To4() == nil
	}
	pub, pub6 := n.PublicIP, n.PublicIP6
	if n.IsLocal {
		pub, pub6 = r.setting("publicIp"), r.setting("publicIp6")
	}
	_, fam := model.AutoIP(pub, pub6, n.AddrFamily)
	return fam == "v6"
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
