package reach

import (
	"context"
	"fmt"
	"math"
	"time"
)

// IPv6 入站检测:从境外有 IPv6 的测点连服务器订阅里给的 IPv6 地址 + 线路端口(TCP 三次握手),
// 看云安全组、ip6tables / nftables / ufw 有没有放行 IPv6。
//
// 为什么真测、不看网卡:Azure 这类云的公网 IPv6 是 1:1 NAT 到私有 fd00:: 地址上的,公网地址根本不在网卡上,
// 入站却完全可用,按网卡比对会把它误判成"只能出站";反过来地址在网卡上,也不代表安全组放行了。
// 只用境外测点:这一项回答"包能不能到这台机器",大陆运营商的 IPv6 国际路由好坏不在这里判断。
// 目标写成 IPv6 地址时 Globalping 只派支持 IPv6 的测点(这时不能、也不用传 ipVersion)。
// 和大陆检测同一轮做,不另起定时、不加轮询;每台约 4 个测点次。
// hysteria2 / tuic 是 UDP,测点只能连 TCP:这台走 IPv6 的线路全是 UDP 时不发请求,如实注明只能抓包确认。

// IPv6 入站检测的结论。
const (
	V6OK     = "ok"     // 至少一个测点连上了:IPv6 入站放行了
	V6Down   = "down"   // 两个以上测点测到、全都连不上:多半是安全组没放行 ::/0,或 ip6tables 拦了
	V6Unsure = "unsure" // 只有一个测点测到且没连上,下不了结论
	V6UDP    = "udp"    // 走 IPv6 的线路全是 UDP(hysteria2 / tuic):测点测不了,只能在服务器上抓包确认
	V6None   = "none"   // 这次没有可用的 IPv6 测点
	V6Error  = "error"  // 没测成(额度、接口出错、地址不能测)
)

// v6Locations 境外测点:欧洲、北美各两个,这两处支持 IPv6 的测点最多。不用大陆测点(见上)。
var v6Locations = []location{{Continent: "EU", Limit: 2}, {Continent: "NA", Limit: 2}}

// v6Cost 测一次 IPv6 入站要用的测点次。
func v6Cost() int {
	n := 0
	for _, l := range v6Locations {
		n += l.Limit
	}
	return n
}

// V6Target 一台服务器要测的 IPv6 入站。
type V6Target struct {
	Host     string   // 订阅里给的 IPv6 地址
	Port     int      // 第一条走 IPv6 的 TCP 线路的端口;0 = 走 IPv6 的线路全是 UDP,测不了
	PortLine string   // 端口所属的线路
	UDPLines []string // 走 IPv6 的 UDP 线路(hysteria2 / tuic):只能抓包确认
	Err      string   // 测不了的原因(地址不是公网 IPv6 等);非空时不发请求
}

// V6Result 一次 IPv6 入站检测。
type V6Result struct {
	Target      string        `json:"target"`
	Port        int           `json:"port,omitempty"`
	PortLine    string        `json:"portLine,omitempty"`
	UDPLines    []string      `json:"udpLines,omitempty"`
	At          int64         `json:"at"`
	State       string        `json:"state"` // 见 V6OK 等
	OK          int           `json:"ok"`    // 连上的测点数
	Total       int           `json:"total"` // 测到的测点数(离线的不算)
	Probes      []ProbeResult `json:"probes,omitempty"`
	Error       string        `json:"error,omitempty"`
	Measurement string        `json:"measurement,omitempty"`
	// Attempt 只出现在给面板的结果里:最近一次没测成,上面显示的是再上一次测成的
	Attempt *Attempt `json:"attempt,omitempty"`
}

// v6Measured 这次 IPv6 入站检测有结论。没测成的、这次没有可用测点的都不算,不能拿它盖掉上一次的结论
// (和大陆那项的 measured 一样:VerdictError、VerdictNoProbes 都不算)。
func v6Measured(r *V6Result) bool { return r != nil && r.State != V6Error && r.State != V6None }

// viewV6Locked 面板上这台显示哪次 IPv6 入站检测:最近一轮没带(这台已经没有走 IPv6 的线路)就不显示;
// 最近一次没测成或没有可用测点,而以前测成过,显示以前那次并注明最近这次的情况(和大陆那项的 Attempt 一样)。调用方持锁。
func (s *Service) viewV6Locked(id uint, last *V6Result) *V6Result {
	if last == nil {
		return nil
	}
	if !v6Measured(last) {
		if g := s.goodV6[id]; g != nil && g != last {
			r := *g
			r.Attempt = &Attempt{At: last.At, Verdict: last.State, Error: last.Error}
			return &r
		}
	}
	r := *last
	return &r
}

// checkV6 测一台的 IPv6 入站。地址不能测、只有 UDP 线路、额度不够时不发请求。
func (s *Service) checkV6(ctx context.Context, t *V6Target) *V6Result {
	res := &V6Result{Target: t.Host, Port: t.Port, PortLine: t.PortLine, UDPLines: t.UDPLines, At: s.d.Now().Unix()}
	switch {
	case t.Err != "":
		res.State, res.Error = V6Error, t.Err
		return res
	case t.Host == "":
		res.State, res.Error = V6Error, "没有可测的地址"
		return res
	case t.Port == 0:
		res.State = V6UDP
		return res
	}
	if q := s.c.Quota(); q.At > 0 && q.Remaining < v6Cost() && s.d.Now().Unix() < q.ResetAt {
		res.State, res.Error = V6Error, (&QuotaError{Wait: time.Unix(q.ResetAt, 0).Sub(s.d.Now())}).Error()
		return res
	}
	c, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	m, err := s.c.measure(c, measureRequest{Type: "ping", Target: t.Host, Locs: v6Locations,
		Options: pingOptions{Packets: 3, Protocol: "TCP", Port: t.Port}}, s.d.Poll)
	if err != nil {
		res.State, res.Error = V6Error, err.Error()
		return res
	}
	res.Measurement = m.Id
	for _, r := range m.Results {
		pr := ProbeResult{Country: r.Probe.Country, City: r.Probe.City, ASN: r.Probe.ASN, Network: r.Probe.Network}
		st := r.Result.Stats
		// 和大陆检测一样:测点离线、还在跑、没发出包的不算;发了包的(finished,或 failed 但带着统计)才算测到
		if st == nil || st.Total == 0 || r.Result.Status == "offline" || r.Result.Status == "in-progress" {
			pr.Offline = true
			res.Probes = append(res.Probes, pr)
			continue
		}
		pr.Loss = st.Loss
		pr.OK = st.Rcv > 0
		if pr.OK && st.Avg != nil {
			pr.AvgMs = math.Round(*st.Avg*10) / 10
		}
		res.Total++
		if pr.OK {
			res.OK++
		}
		res.Probes = append(res.Probes, pr)
	}
	switch {
	case res.OK > 0:
		res.State = V6OK
	case res.Total >= 2:
		res.State = V6Down
	case res.Total == 1:
		res.State = V6Unsure
	default:
		res.State = V6None
	}
	return res
}

// v6Summary 日志里的一句话。
func v6Summary(r *V6Result) string {
	if r == nil {
		return ""
	}
	switch r.State {
	case V6UDP:
		return " · IPv6 入站:只有 UDP 线路,没测"
	case V6Error:
		return " · IPv6 入站:没测成(" + r.Error + ")"
	}
	return fmt.Sprintf(" · IPv6 入站 %s:%s %d/%d", r.Target, r.State, r.OK, r.Total)
}
