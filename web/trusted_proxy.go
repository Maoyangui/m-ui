package web

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// 受信任的反向代理(设置项 trustedProxies,本机设置):面板 / 代理面板 / 外部 API 挂在反代后面时,TCP 对端都是反代的地址,
// 登录与外部 API 的失败限流按对端计数,一个来源猜错 10 次就把所有人一起挡住。填了反代的地址(IP 或网段)后,
// 只有对端在名单里时才按转发头认来源:X-Forwarded-For 从右往左第一个不在名单里的地址(右边是各层反代自己追加的,
// 左边的段客户端能随便写),没有 X-Forwarded-For 时用 X-Real-IP。留空(默认)= 和以前一样只看对端,转发头一概不认。

// parseTrustedProxies 解析名单:逗号、空格、换行分隔的 IP 或 CIDR。返回规范化后的写法(逗号分隔)。
// 0.0.0.0/0、::/0 这种"谁都信"的不收:那样任何人都能自己写转发头,限流形同虚设。
func parseTrustedProxies(v string) ([]*net.IPNet, string, error) {
	var nets []*net.IPNet
	var norm []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
		var n *net.IPNet
		if strings.Contains(f, "/") {
			_, c, err := net.ParseCIDR(f)
			if err != nil {
				return nil, "", fmt.Errorf("受信任的反向代理里 %q 不是有效的 IP 或网段", f)
			}
			n = c
		} else {
			ip := net.ParseIP(f)
			if ip == nil {
				return nil, "", fmt.Errorf("受信任的反向代理里 %q 不是有效的 IP 或网段", f)
			}
			bits := 128
			if ip4 := ip.To4(); ip4 != nil {
				ip, bits = ip4, 32
			}
			n = &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
		}
		if ones, _ := n.Mask.Size(); ones == 0 {
			return nil, "", errors.New("受信任的反向代理不能写 0.0.0.0/0 或 ::/0:那样谁都能自己写转发头,限流就没用了")
		}
		nets = append(nets, n)
		if ones, bits := n.Mask.Size(); ones == bits {
			norm = append(norm, n.IP.String())
		} else {
			norm = append(norm, n.String())
		}
	}
	return nets, strings.Join(norm, ", "), nil
}

type trustedProxyCache struct {
	raw  string
	nets []*net.IPNet
}

var trustedProxyParsed atomic.Pointer[trustedProxyCache]

// trustedProxies 当前名单(按设置原文缓存解析结果;存进库的都校验过,解析不了的按空名单处理 = 只看对端)。
func (s *Server) trustedProxies() []*net.IPNet {
	raw := strings.TrimSpace(s.setting("trustedProxies"))
	if raw == "" {
		return nil
	}
	if c := trustedProxyParsed.Load(); c != nil && c.raw == raw {
		return c.nets
	}
	nets, _, err := parseTrustedProxies(raw)
	if err != nil {
		nets = nil
	}
	trustedProxyParsed.Store(&trustedProxyCache{raw: raw, nets: nets})
	return nets
}

func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseHop 转发头里的一段:纯 IP,或带端口 / 方括号的写法(有的反代会带上端口)。
func parseHop(h string) net.IP {
	h = strings.TrimSpace(h)
	if ip := net.ParseIP(h); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(strings.Trim(h, "[]"))
}

// sourceIP 限流计数用的来源地址:没设受信任的反代,或对端不在名单里 → TCP 对端(peerIP);
// 对端是受信任的反代 → X-Forwarded-For 从右往左第一个不受信任的地址(全是受信任的就取最左边那个),
// 没有 X-Forwarded-For 时用 X-Real-IP;转发头缺失或写坏了 → 对端。
func (s *Server) sourceIP(r *http.Request) string {
	peer := peerIP(r)
	nets := s.trustedProxies()
	if len(nets) == 0 || !ipInNets(net.ParseIP(peer), nets) {
		return peer
	}
	if vals := r.Header.Values("X-Forwarded-For"); len(vals) > 0 {
		hops := strings.Split(strings.Join(vals, ","), ",")
		last := "" // 已经走过的最左边一个受信任的地址
		for i := len(hops) - 1; i >= 0; i-- {
			if strings.TrimSpace(hops[i]) == "" {
				continue
			}
			ip := parseHop(hops[i])
			if ip == nil { // 写坏的一段:它左边的都不可信,停在已知的最后一个受信任地址上
				break
			}
			if !ipInNets(ip, nets) {
				return ip.String()
			}
			last = ip.String()
		}
		if last != "" {
			return last
		}
	}
	if ip := parseHop(r.Header.Get("X-Real-IP")); ip != nil {
		return ip.String()
	}
	return peer
}
