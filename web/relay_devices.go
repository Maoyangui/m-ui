package web

import (
	"net/netip"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 中转入口的来源(中转机的地址)不是用户的设备:数据面不把它算进设备数(见 core.Limiter.SetRelaySources),
// 面板上数「在线几台」「池用了几台」时同样不算 —— 不然限 1 台的人直连 + 中转一起用会显示成 2 台。

// relayIPs 现在的中转入口来源地址。几秒内重复问的用同一份:概况每几秒拉一次,代理列表一行算一次。
func (s *Server) relayIPs() map[string]bool {
	s.relayMu.Lock()
	defer s.relayMu.Unlock()
	if !s.relayAt.IsZero() && time.Since(s.relayAt) < 5*time.Second {
		return s.relaySet
	}
	s.relaySet, s.relayAt = s.run.RelayIPs(), time.Now()
	return s.relaySet
}

// deviceSources 在线来源里去掉中转机的地址(relay 为空就是原样)。
func deviceSources(ips []string, relay map[string]bool) []string {
	if len(relay) == 0 || len(ips) == 0 {
		return ips
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		key := ip
		if strings.Contains(ip, "::ffff:") || strings.Contains(ip, "%") { // 4in6、带 zone 的写法:规整后再比
			if a, err := netip.ParseAddr(ip); err == nil {
				key = a.Unmap().WithZone("").String()
			}
		}
		if !relay[key] {
			out = append(out, ip)
		}
	}
	return out
}

// onlineDeviceCount 这个人在线几台设备(IPv4 一个地址一台、IPv6 按 /64,和设备数限制同一口径):中转机的地址不算;
// 只经中转在线的算 1 —— 人是在线的,只是出口这边认不出是几台。没有任何来源的是 0。
func onlineDeviceCount(ips []string, relay map[string]bool) int {
	if n := model.DeviceCount(deviceSources(ips, relay)); n > 0 {
		return n
	}
	if len(ips) > 0 {
		return 1
	}
	return 0
}
