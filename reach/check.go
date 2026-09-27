package reach

import "math"

// 判定结果。
const (
	VerdictOK          = "ok"          // 三网都通
	VerdictPartial     = "partial"     // 有运营商整家不通
	VerdictBlocked     = "blocked"     // 三网都不通、香港通:疑似被墙(这次分不清是 IP 还是端口)
	VerdictIPBlocked   = "ipBlocked"   // 端口和 ping 都不通、香港通:整个 IP 被墙,换 IP 才行
	VerdictPortBlocked = "portBlocked" // ping 通、端口不通:这个端口被封,换端口即可
	VerdictAbroadDown  = "abroadDown"  // 香港也连不上:服务器、端口或安全组的问题,不是墙
	VerdictNoProbes    = "noProbes"    // 这次没有可用的大陆测点
	VerdictError       = "error"       // 检测没做成(接口出错、额度用完、没有可测的地址)
)

// IsBlocked 是否属于"被墙"一类。
func IsBlocked(v string) bool {
	return v == VerdictBlocked || v == VerdictIPBlocked || v == VerdictPortBlocked
}

// 分组状态。
const (
	StateOK   = "ok"   // 过半测点通
	StateWeak = "weak" // 通的不到一半
	StateDown = "down" // 一个都不通
	StateNone = "none" // 没有可用测点
)

// ProbeResult 一个测点的结果。
type ProbeResult struct {
	City    string  `json:"city"`
	ASN     int     `json:"asn"`
	Network string  `json:"network"`
	OK      bool    `json:"ok"`
	AvgMs   float64 `json:"avgMs,omitempty"`
	Loss    float64 `json:"loss"`
	Offline bool    `json:"offline,omitempty"` // 测点离线或测量失败:不计入
}

// Group 一家运营商(或香港对照组)的汇总。
type Group struct {
	Key    string        `json:"key"`
	State  string        `json:"state"`
	OK     int           `json:"ok"`
	Total  int           `json:"total"`
	AvgMs  float64       `json:"avgMs,omitempty"`
	Probes []ProbeResult `json:"probes"`
}

// ICMPCheck 大陆连端口全失败后,用同一批测点补测的 ping:分清是 IP 被墙还是端口被封。
type ICMPCheck struct {
	CNOK    int `json:"cnOk"`
	CNTotal int `json:"cnTotal"`
	HKOK    int `json:"hkOk"`
	HKTotal int `json:"hkTotal"`
}

// Result 一台服务器的一次检测。
type Result struct {
	NodeId       uint       `json:"nodeId"`
	Name         string     `json:"name"`
	Target       string     `json:"target"`
	Port         int        `json:"port"`     // 0 = 没有可连的 TCP 端口,只做了 ping
	PortFrom     string     `json:"portFrom"` // line / sub / api;为空表示只做了 ping
	PortLine     string     `json:"portLine,omitempty"`
	At           int64      `json:"at"`
	Auto         bool       `json:"auto"`
	Verdict      string     `json:"verdict"`
	Groups       []Group    `json:"groups"`
	ICMP         *ICMPCheck `json:"icmp,omitempty"`
	ICMPError    string     `json:"icmpError,omitempty"` // 该补测 ping 却没做成(额度、接口出错):这次分不清 IP 还是端口
	Rechecked    bool       `json:"rechecked,omitempty"` // 定时巡检发现异常后隔一会儿又测了一遍
	Error        string     `json:"error,omitempty"`
	Measurements []string   `json:"measurements,omitempty"`
	// Attempt 只出现在给面板的结果里:最近一次没测成(额度用完、接口出错),上面显示的是再上一次测成的结果
	Attempt *Attempt `json:"attempt,omitempty"`
}

// Attempt 一次没测成的检测。
type Attempt struct {
	At      int64  `json:"at"`
	Verdict string `json:"verdict"`
	Error   string `json:"error,omitempty"`
}

// measured 这次检测有没有测成(测成了才能拿来下结论、比较、告警)。
func measured(v string) bool { return v != VerdictError && v != VerdictNoProbes }

// summarize 把一次测量按运营商与香港对照组归类。
func summarize(m *measurement) map[string]*Group {
	gs := map[string]*Group{}
	for _, k := range append(append([]string{}, carrierGroups...), GroupAbroad) {
		gs[k] = &Group{Key: k, Probes: []ProbeResult{}}
	}
	if m == nil {
		return gs
	}
	for _, r := range m.Results {
		var g string
		switch r.Probe.Country {
		case "CN":
			g = carrierOf(r.Probe.ASN, r.Probe.Network)
		case "HK":
			g = GroupAbroad
		}
		if g == "" {
			continue
		}
		pr := ProbeResult{City: r.Probe.City, ASN: r.Probe.ASN, Network: r.Probe.Network}
		if r.Result.Status != "finished" || r.Result.Stats == nil || r.Result.Stats.Total == 0 {
			pr.Offline = true
			gs[g].Probes = append(gs[g].Probes, pr)
			continue
		}
		st := r.Result.Stats
		pr.Loss = st.Loss
		pr.OK = st.Rcv > 0
		if pr.OK && st.Avg != nil {
			pr.AvgMs = math.Round(*st.Avg*10) / 10
		}
		gs[g].Probes = append(gs[g].Probes, pr)
	}
	for _, g := range gs {
		var sum float64
		for _, p := range g.Probes {
			if p.Offline {
				continue
			}
			g.Total++
			if p.OK {
				g.OK++
				sum += p.AvgMs
			}
		}
		if g.OK > 0 {
			g.AvgMs = math.Round(sum/float64(g.OK)*10) / 10
		}
		switch {
		case g.Total == 0:
			g.State = StateNone
		case g.OK == 0:
			g.State = StateDown
		case g.OK*2 >= g.Total:
			g.State = StateOK
		default:
			g.State = StateWeak
		}
	}
	return gs
}

// verdict 按各组结果下结论。icmp 只在大陆连端口全失败后才有。
func verdict(gs map[string]*Group, icmp *ICMPCheck, icmpOnly bool) string {
	measured, down := 0, 0
	for _, k := range carrierGroups {
		if gs[k].State == StateNone {
			continue
		}
		measured++
		if gs[k].State == StateDown {
			down++
		}
	}
	if measured == 0 {
		return VerdictNoProbes
	}
	if hk := gs[GroupAbroad]; hk.Total > 0 && hk.OK == 0 {
		return VerdictAbroadDown
	}
	// 只有一家有测点时,它不通也说不上"三网都不通"
	if down == measured && measured >= 2 {
		switch {
		case icmpOnly:
			return VerdictBlocked
		case icmp != nil && icmp.CNOK > 0:
			return VerdictPortBlocked
		case icmp != nil && icmp.CNTotal > 0 && icmp.HKOK > 0:
			return VerdictIPBlocked
		}
		return VerdictBlocked
	}
	if down > 0 {
		return VerdictPartial
	}
	return VerdictOK
}

// orderedGroups 按界面顺序排好:移动、电信、联通、香港。
func orderedGroups(gs map[string]*Group) []Group {
	out := make([]Group, 0, 4)
	for _, k := range append(append([]string{}, carrierGroups...), GroupAbroad) {
		out = append(out, *gs[k])
	}
	return out
}
