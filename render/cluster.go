package render

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"gorm.io/gorm"
)

// 集群里别的服务器(主机看副机,副机看主机和别的副机)的管理端口,私网屏蔽开着时同样不让用户经代理碰。
//
// 只拦本机公网地址不够:用户经主机代理去连副机的副机接口 / 面板 / 订阅端口,来源正是主机的 IP —— 副机按来源
// 只放行主机的副机接口、云防火墙只放行集群里几台机器的面板端口,都被绕过了。
//
// 拦哪些端口:只拦管理端口 —— 副机接口(API 地址里的端口,没写按 https 443 / http 80)、面板、订阅(副机随报告上报,
// 老副机没报按安装默认 2053 / 2056)、代理面板(主机在快照里带上自己的)。不像本机那样拦全部端口:别的服务器上的
// 代理线路端口是用户正常要用的(经一台机器的代理去连另一台的线路、测速、链式代理),整台拦掉会误伤;
// 而线路端口不能和那台服务器的面板 / 订阅端口相同(保存线路时就校验),所以只拦管理端口不会拦到任何线路。
//
// 拦哪些地址:那台服务器探测到的公网 IPv4 / IPv6(主机从报告里记、随快照发给副机)、API 地址与连接地址里直接写的 IP,
// 以及 API 地址的域名解析出来的 IPv4 / IPv6。域名解析出来、又不是那台服务器自己报的地址的,不拦 80 / 443:
// API 地址套了 CDN 时解析出来的是 CDN 的地址,拦了它的 443 用户经代理就上不了这家 CDN 后面的所有网站;
// 真是那台服务器自己的地址,它报上来的公网地址里就有,照样全拦。
//
// 面板自己的流量不受影响:主机同步副机、健康检查、线路作上游 / 上游组拨到别的服务器,都是面板进程或出站自己发起的连接,
// 不经过路由规则;路由规则只管用户经入站进来的连接。
// 只拦 TCP:面板、订阅、副机接口都是 HTTP(S);别的服务器上和管理端口同号的 UDP 线路(hy2 / tuic 只占 UDP)照常可用。

// lookupIP 解析 API 地址的域名(测试里替换)。
var lookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// clusterDNSTTL 解析结果缓存多久:BuildConfig 每次热更新用户都要跑,不能每次都查 DNS(慢,而且轮询解析每次顺序 / 结果
// 不同会让配置文本跟着变,被当成"除了用户还有别的变化"去重启数据面)。
const clusterDNSTTL = 30 * time.Minute

var clusterDNS = struct {
	sync.Mutex
	m map[string]clusterDNSEntry
}{m: map[string]clusterDNSEntry{}}

type clusterDNSEntry struct {
	ips []net.IP
	at  time.Time
}

func resolveClusterHost(host string) []net.IP {
	clusterDNS.Lock()
	e, ok := clusterDNS.m[host]
	clusterDNS.Unlock()
	if ok && time.Since(e.at) < clusterDNSTTL {
		return e.ips
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := lookupIP(ctx, host)
	if err != nil && ok {
		ips = e.ips // 查不到先沿用上次的,别让一次 DNS 抖动把规则撤掉
	}
	clusterDNS.Lock()
	clusterDNS.m[host] = clusterDNSEntry{ips: ips, at: time.Now()}
	clusterDNS.Unlock()
	return ips
}

// defaultClusterPorts 服务器没上报面板 / 订阅端口(老副机)时按安装默认值拦。
var defaultClusterPorts = []int{2053, 2056}

// publicCIDR 公网单播地址的 /32、/128;内网、回环、链路本地已经由 ip_is_private 管着,返回空。
func publicCIDR(ip net.IP) string {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String() + "/32"
	}
	return ip.String() + "/128"
}

// clusterNodes 本机以外的集群服务器(停用的也算:它照样在跑面板和副机接口)。
func clusterNodes(db *gorm.DB) []model.Node {
	var nodes []model.Node
	db.Where("is_local = ?", false).Order("id asc").Find(&nodes)
	return nodes
}

// nodePorts 一台服务器的管理端口:API 地址里的端口 + 上报的服务端口(没上报按默认)。
func nodePorts(n model.Node) []int {
	set := map[int]bool{}
	if u, err := url.Parse(strings.TrimSpace(n.ApiUrl)); err == nil && u.Host != "" {
		p, _ := strconv.Atoi(u.Port())
		if p == 0 {
			p = 443
			if strings.EqualFold(u.Scheme, "http") {
				p = 80
			}
		}
		set[p] = true
	}
	var sps []model.ServicePort
	if len(n.ServicePorts) == 0 || json.Unmarshal(n.ServicePorts, &sps) != nil || len(sps) == 0 {
		for _, p := range defaultClusterPorts {
			set[p] = true
		}
	}
	for _, sp := range sps {
		if sp.Port > 0 && sp.Port < 65536 {
			set[sp.Port] = true
		}
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// clusterBlockRules 别的集群服务器管理端口的屏蔽规则:按端口集合合并,同一组端口一条规则;顺序稳定。
func clusterBlockRules(db *gorm.DB) []json.RawMessage {
	type group struct {
		ports []int
		cidrs []string
		seen  map[string]bool
	}
	groups := map[string]*group{}
	var keys []string
	add := func(ports []int, cidr string) {
		if cidr == "" || len(ports) == 0 {
			return
		}
		b, _ := json.Marshal(ports)
		k := string(b)
		g := groups[k]
		if g == nil {
			g = &group{ports: ports, seen: map[string]bool{}}
			groups[k] = g
			keys = append(keys, k)
		}
		if !g.seen[cidr] {
			g.seen[cidr] = true
			g.cidrs = append(g.cidrs, cidr)
		}
	}
	for _, n := range clusterNodes(db) {
		ports := nodePorts(n)
		own := map[string]bool{} // 这台服务器自己报的 / 直接写成 IP 的地址:全部管理端口都拦
		for _, s := range []string{n.PublicIP, n.PublicIP6, n.Addr} {
			if c := publicCIDR(net.ParseIP(strings.Trim(strings.TrimSpace(s), "[]"))); c != "" {
				own[c] = true
				add(ports, c)
			}
		}
		u, err := url.Parse(strings.TrimSpace(n.ApiUrl))
		if err != nil || u.Hostname() == "" {
			continue
		}
		host := u.Hostname()
		var resolved []net.IP
		literal := false
		if ip := net.ParseIP(host); ip != nil {
			resolved, literal = []net.IP{ip}, true
		} else {
			resolved = resolveClusterHost(host)
		}
		var dnsPorts []int // 域名解析出来的别人家地址(可能是 CDN):不拦 80 / 443
		for _, p := range ports {
			if p != 80 && p != 443 {
				dnsPorts = append(dnsPorts, p)
			}
		}
		for _, ip := range resolved {
			c := publicCIDR(ip)
			if c == "" || own[c] {
				continue
			}
			if literal {
				add(ports, c)
			} else {
				add(dnsPorts, c)
			}
		}
	}
	sort.Strings(keys)
	out := make([]json.RawMessage, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		sort.Strings(g.cidrs)
		rule, _ := json.Marshal(map[string]interface{}{"ip_cidr": g.cidrs, "network": []string{"tcp"}, "port": g.ports, "action": "reject"})
		out = append(out, rule)
	}
	return out
}

// ClusterSig 决定集群屏蔽规则的那几项(别的服务器的 API 地址、连接地址、公网地址、服务端口,外加私网开关)的摘要。
// runner 定期比对它,变了(加了副机、副机换了 IP、改了端口)就重新渲染;不含 DNS 结果,解析结果随下一次渲染更新。
func ClusterSig(db *gorm.DB) string {
	var b strings.Builder
	b.WriteString(strconv.FormatBool(AllowPrivate(db)))
	for _, n := range clusterNodes(db) {
		b.WriteString("|" + strconv.FormatUint(uint64(n.Id), 10) + "," + n.ApiUrl + "," + n.Addr + "," + n.PublicIP + "," + n.PublicIP6 + "," + string(n.ServicePorts))
	}
	return b.String()
}
