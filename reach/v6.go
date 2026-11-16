package reach

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// IPv6 入站检测:从测点连服务器订阅里给的 IPv6 地址 + 线路端口(TCP 三次握手)。
//
// 先用大陆的测点(家宽 / 手机网络里有 IPv6 的):用户在大陆,首先要回答的是"大陆走 IPv6 连不连得上这台"。
//   - 大陆测点连上了 → 通;
//   - 大陆测点都连不上 → 再用境外测点测一次,分清是哪一头的问题:境外也连不上 = 入站没放行(云安全组没放 ::/0、
//     ip6tables / nftables / ufw 拦了,或这个 IPv6 只能出站);境外连得上 = 入站是放行的,这个 IPv6 在大陆方向不通
//     (被墙,或运营商的 IPv6 到不了);
//   - 这次没有大陆的 IPv6 测点(Globalping 上大陆有 IPv6 的家宽测点时有时无)→ 只用境外测点,结论注明是境外测点得出的。
// 以前只用境外测点:能看出入站放没放行,看不出大陆连不连得上 —— 而"小火箭走 IPv6 连不上"这类问题恰恰出在后者。
//
// 为什么真测、不看网卡:Azure 这类云的公网 IPv6 是 1:1 NAT 到私有 fd00:: 地址上的,公网地址根本不在网卡上,
// 入站却完全可用,按网卡比对会把它误判成"只能出站";反过来地址在网卡上,也不代表安全组放行了。
// 目标写成 IPv6 地址时 Globalping 只派支持 IPv6 的测点(这时不能、也不用传 ipVersion)。
// 和大陆检测同一轮做,不另起定时、不加轮询;通常约 4 个测点次(大陆那一步),要用境外测点时再加 4 个。
// hysteria2 / tuic 是 UDP,测点只能连 TCP:这台走 IPv6 的线路全是 UDP 时不发请求,如实注明只能抓包确认。

// IPv6 入站检测的结论。
const (
	V6OK     = "ok"     // 连上了(Scope 说明是大陆测点还是境外测点连上的)
	V6CNDown = "cndown" // 大陆测点都连不上、境外测点连得上:入站是放行的,大陆方向不通
	V6Down   = "down"   // 两个以上测点测到、全都连不上:多半是安全组没放行 ::/0,或 ip6tables 拦了
	V6Unsure = "unsure" // 测到的测点太少(只有一个且没连上),下不了结论
	V6UDP    = "udp"    // 走 IPv6 的线路全是 UDP(hysteria2 / tuic):测点测不了,只能在服务器上抓包确认
	V6None   = "none"   // 这次没有可用的 IPv6 测点
	V6Error  = "error"  // 没测成(额度、接口出错、地址不能测)
)

// 结论是哪里的测点得出的(V6Result.Scope)。
const (
	V6ScopeCN     = "cn"     // 大陆测点(优先)
	V6ScopeAbroad = "abroad" // 境外测点:这次没有大陆的 IPv6 测点
	V6ScopeBoth   = "both"   // 大陆测点连不上之后又问了境外测点
)

// v6CNLocations 大陆测点:家宽 / 手机网络的(机房测点不算,理由同大陆连通检测),有 IPv6 的由 Globalping 自己挑。
var v6CNLocations = []location{{Country: "CN", Tags: []string{eyeballTag}, Limit: 4}}

// v6AbroadLocations 境外测点:欧洲、北美各两个,这两处支持 IPv6 的测点最多。
var v6AbroadLocations = []location{{Continent: "EU", Limit: 2}, {Continent: "NA", Limit: 2}}

func locCost(locs []location) int {
	n := 0
	for _, l := range locs {
		n += l.Limit
	}
	return n
}

// v6Cost 测一次 IPv6 入站通常要用的测点次(大陆那一步);要问境外测点时另加 v6AbroadCost。
func v6Cost() int { return locCost(v6CNLocations) }

// v6AbroadCost 境外那一步要用的测点次。
func v6AbroadCost() int { return locCost(v6AbroadLocations) }

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
	Target   string   `json:"target"`
	Port     int      `json:"port,omitempty"`
	PortLine string   `json:"portLine,omitempty"`
	UDPLines []string `json:"udpLines,omitempty"`
	At       int64    `json:"at"`
	State    string   `json:"state"` // 见 V6OK 等
	// Scope 结论是哪里的测点得出的(见 V6ScopeCN 等);旧版存下来的结果没有这一项,都是境外测点
	Scope string `json:"scope,omitempty"`
	OK    int    `json:"ok"`    // 连上的测点数(大陆、境外合计)
	Total int    `json:"total"` // 测到的测点数(离线的不算)
	// 大陆测点那一步:连上的、测到的个数
	CnOK    int `json:"cnOk,omitempty"`
	CnTotal int `json:"cnTotal,omitempty"`
	// AbroadSkip 该问境外测点却没问成的原因(额度不够、接口出错):这时分不清是入站没放行还是大陆方向不通
	AbroadSkip  string        `json:"abroadSkip,omitempty"`
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

// v6Probe 一步测量:从 locs 这批测点连 host:port,返回各测点的结果、连上的与测到的个数。
// noProbes:这批条件下现在一个有 IPv6 的测点都没有(Globalping 直接拒收请求,不扣额度)。
func (s *Service) v6Probe(ctx context.Context, host string, port int, locs []location) (probes []ProbeResult, ok, total int, id string, noProbes bool, err error) {
	c, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	m, err := s.c.measure(c, measureRequest{Type: "ping", Target: host, Locs: locs,
		Options: pingOptions{Packets: 3, Protocol: "TCP", Port: port}}, s.d.Poll)
	if errors.Is(err, errNoProbes) {
		return nil, 0, 0, "", true, nil
	}
	if err != nil {
		return nil, 0, 0, "", false, err
	}
	for _, r := range m.Results {
		pr := ProbeResult{Country: r.Probe.Country, City: r.Probe.City, ASN: r.Probe.ASN, Network: r.Probe.Network}
		if r.Probe.Country == "CN" {
			pr.Group = carrierOf(r.Probe.ASN, r.Probe.Network)
		}
		st := r.Result.Stats
		// 和大陆检测一样:测点离线、还在跑、没发出包的不算;发了包的(finished,或 failed 但带着统计)才算测到
		if st == nil || st.Total == 0 || r.Result.Status == "offline" || r.Result.Status == "in-progress" {
			pr.Offline = true
			probes = append(probes, pr)
			continue
		}
		pr.Loss = st.Loss
		pr.OK = st.Rcv > 0
		if pr.OK && st.Avg != nil {
			pr.AvgMs = math.Round(*st.Avg*10) / 10
		}
		total++
		if pr.OK {
			ok++
		}
		probes = append(probes, pr)
	}
	return probes, ok, total, m.Id, false, nil
}

// quotaShort 额度已知、还没到恢复时间、剩下的不够 need 个测点次时,返回要等多久的那个错误;够用返回 nil。
func (s *Service) quotaShort(need int) error {
	if q := s.c.Quota(); q.At > 0 && q.Remaining < need && s.d.Now().Unix() < q.ResetAt {
		return &QuotaError{Wait: time.Unix(q.ResetAt, 0).Sub(s.d.Now())}
	}
	return nil
}

// checkV6 测一台的 IPv6 入站:先大陆测点,连不上或没有大陆测点时再问境外测点(见文件开头)。
// 地址不能测、只有 UDP 线路、额度不够时不发请求。
// 只有 UDP 线路时本来就不发请求,地址空着也照样是"只有 UDP 线路",不报"没有可测的地址"。
func (s *Service) checkV6(ctx context.Context, t *V6Target) *V6Result {
	res := &V6Result{Target: t.Host, Port: t.Port, PortLine: t.PortLine, UDPLines: t.UDPLines, At: s.d.Now().Unix()}
	switch {
	case t.Err != "":
		res.State, res.Error = V6Error, t.Err
		return res
	case t.Port == 0:
		res.State = V6UDP
		return res
	case t.Host == "":
		res.State, res.Error = V6Error, "没有可测的地址"
		return res
	}
	if err := s.quotaShort(v6Cost()); err != nil {
		res.State, res.Error = V6Error, err.Error()
		return res
	}

	// 第一步:大陆测点
	probes, ok, total, id, _, err := s.v6Probe(ctx, t.Host, t.Port, v6CNLocations)
	if err != nil {
		res.State, res.Error = V6Error, err.Error()
		return res
	}
	res.Probes, res.Measurement = probes, id
	res.CnOK, res.CnTotal, res.OK, res.Total = ok, total, ok, total
	if ok > 0 {
		res.State, res.Scope = V6OK, V6ScopeCN
		return res
	}

	// 第二步:境外测点。大陆测点都没连上时用来分清是哪一头的问题;这次没有大陆测点(一个都没派到,或派到的全离线)时就靠它下结论
	res.Scope = V6ScopeBoth
	if total == 0 {
		res.Scope = V6ScopeAbroad
	}
	abroadOK := 0
	if err := s.quotaShort(v6AbroadCost()); err != nil {
		res.AbroadSkip = err.Error()
	} else if ap, aok, atotal, aid, _, err := s.v6Probe(ctx, t.Host, t.Port, v6AbroadLocations); err != nil {
		res.AbroadSkip = err.Error()
	} else {
		res.Probes = append(res.Probes, ap...)
		abroadOK = aok
		res.OK, res.Total = res.OK+aok, res.Total+atotal
		if res.Measurement == "" {
			res.Measurement = aid
		}
	}
	switch {
	case total == 0 && res.AbroadSkip != "": // 大陆没有测点,境外又没问成
		res.State, res.Error = V6Error, res.AbroadSkip
	case abroadOK > 0 && total == 0: // 没有大陆测点,境外连得上
		res.State = V6OK
	case abroadOK > 0 && total >= 2: // 境外通、大陆(两个以上测点)都不通
		res.State = V6CNDown
	case abroadOK > 0: // 境外通,大陆只有一个测点测到且没连上:说不准是那个测点的问题还是大陆方向不通
		res.State = V6Unsure
	case res.Total >= 2: // 合起来两个以上测点测到、全都连不上
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
	from := map[string]string{V6ScopeCN: "大陆测点", V6ScopeAbroad: "境外测点(没有大陆测点)", V6ScopeBoth: "大陆 + 境外测点"}[r.Scope]
	return fmt.Sprintf(" · IPv6 入站 %s:%s %d/%d %s", r.Target, r.State, r.OK, r.Total, from)
}
