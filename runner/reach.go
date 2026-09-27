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
		t.Host = r.reachHost(ctx, n)
		if t.Host == "" {
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

func (r *Runner) reachHost(ctx context.Context, n model.Node) string {
	if ip := net.ParseIP(strings.TrimSpace(n.Addr)); ip != nil && ip.To4() != nil {
		return ip.String()
	}
	pub := n.PublicIP
	if n.IsLocal && pub == "" {
		pub = r.setting("publicIp")
	}
	if ip := net.ParseIP(strings.TrimSpace(pub)); ip != nil && ip.To4() != nil {
		return ip.String()
	}
	host := strings.TrimSpace(n.Addr)
	if host == "" {
		host = strings.TrimSpace(n.Domain)
	}
	if host == "" && n.IsLocal {
		host = strings.TrimSpace(r.setting("webDomain"))
	}
	if host == "" {
		return ""
	}
	// 域名在主机上解析成 IPv4 再测:让大陆测点自己解析会撞上 DNS 污染,测出来的就不是这台服务器了
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(c, "ip4", host)
	if err != nil || len(ips) == 0 {
		return ""
	}
	return ips[0].String()
}

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
