package render

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
)

// 线路的分流规则:同一条线路里按目标(域名、IP 段、端口)分给不同出口,从上往下第一条命中的算,
// 都没命中的走线路自己选的上游。存在 Line.RouteRules(JSON 数组);空 = 不分流,和没有这个功能时完全一样。
// 和 rules 包的"规则"(时段 / 突发限速)不是一回事。

// RouteRule 一条分流规则。
type RouteRule struct {
	Name   string   `json:"name,omitempty"` // 可选的名字(查应用域名填的应用名),只用于显示,不影响分流
	Type   string   `json:"type"`           // 见 RouteRuleTypes
	Values []string `json:"values"`         // 任一命中即算命中
	To     int      `json:"to"`             // >0 上游 id;RouteDirect 直连;RouteReject 拦截
}

const (
	RouteDirect = 0
	RouteReject = -1

	maxRouteRules  = 100
	maxRouteValues = 5000 // 一条线路所有规则的匹配内容加起来
)

// RouteRuleTypes 支持的匹配方式。
var RouteRuleTypes = map[string]bool{"domain_suffix": true, "domain": true, "domain_keyword": true, "ip_cidr": true, "port": true}

// ParseRouteRules 读出并整理分流规则:去掉空白和重复,域名转小写,IP 写成网段,端口区间写成 a:b。
// 内容不合法时返回错误(说清是第几条的哪个值)。空 / null / [] 返回 nil, nil。
func ParseRouteRules(raw json.RawMessage) ([]RouteRule, error) {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return nil, nil
	}
	var in []RouteRule
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("分流规则不是合法 JSON: %w", err)
	}
	if len(in) > maxRouteRules {
		return nil, fmt.Errorf("分流规则最多 %d 条", maxRouteRules)
	}
	var out []RouteRule
	total := 0
	for i, r := range in {
		if !RouteRuleTypes[r.Type] {
			return nil, fmt.Errorf("第 %d 条分流规则:不支持的匹配方式 %q", i+1, r.Type)
		}
		if r.To < RouteReject {
			return nil, fmt.Errorf("第 %d 条分流规则:出口无效", i+1)
		}
		seen := map[string]bool{}
		var vals []string
		for _, v := range r.Values {
			nv, err := normRouteValue(r.Type, v)
			if err != nil {
				return nil, fmt.Errorf("第 %d 条分流规则:%w", i+1, err)
			}
			if nv != "" && !seen[nv] {
				seen[nv] = true
				vals = append(vals, nv)
			}
		}
		if len(vals) == 0 {
			return nil, fmt.Errorf("第 %d 条分流规则没有填匹配内容", i+1)
		}
		if total += len(vals); total > maxRouteValues {
			return nil, fmt.Errorf("分流规则的匹配内容加起来最多 %d 项", maxRouteValues)
		}
		out = append(out, RouteRule{Name: routeRuleName(r.Name), Type: r.Type, Values: vals, To: r.To})
	}
	return out, nil
}

// routeRuleName 规则名只用于显示:去掉控制字符与首尾空白,最长 32 个字。
func routeRuleName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if rs := []rune(s); len(rs) > 32 {
		s = strings.TrimSpace(string(rs[:32]))
	}
	return s
}

// normRouteValue 整理一个匹配值;空串表示跳过。
func normRouteValue(typ, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	switch typ {
	case "ip_cidr":
		if p, err := netip.ParsePrefix(v); err == nil {
			return p.Masked().String(), nil
		}
		if a, err := netip.ParseAddr(v); err == nil && a.Zone() == "" {
			a = a.Unmap()
			return netip.PrefixFrom(a, a.BitLen()).String(), nil
		}
		return "", fmt.Errorf("「%s」不是 IP 或 IP 段(例:1.2.3.4、91.108.4.0/22)", v)
	case "port":
		a, b, isRange := strings.Cut(strings.ReplaceAll(v, "-", ":"), ":")
		lo, err1 := strconv.Atoi(strings.TrimSpace(a))
		hi, err2 := lo, error(nil)
		if isRange {
			hi, err2 = strconv.Atoi(strings.TrimSpace(b))
		}
		if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
			return "", fmt.Errorf("「%s」不是端口或端口区间(例:443、8000-9000)", v)
		}
		if lo == hi {
			return strconv.Itoa(lo), nil
		}
		return strconv.Itoa(lo) + ":" + strconv.Itoa(hi), nil
	default: // 三种域名
		v = strings.ToLower(v)
		if strings.ContainsAny(v, " \t\r\n") {
			return "", fmt.Errorf("「%s」里有空格", v)
		}
		if typ == "domain_keyword" {
			return v, nil
		}
		if strings.ContainsAny(v, "/:@*?#,") {
			return "", fmt.Errorf("「%s」不是域名:只填域名本身,比如 netflix.com", v)
		}
		if _, err := netip.ParseAddr(v); err == nil {
			return "", fmt.Errorf("「%s」是 IP,请用「IP 段」匹配", v)
		}
		return v, nil
	}
}

// LineUpstreams 线路用到的上游 id:自己选的上游 + 分流规则指向的上游(去重,不含直连 / 拦截)。
// 删上游的引用检查、巡检和"未使用"判断都按它算。
func LineUpstreams(l model.Line) []uint {
	var out []uint
	seen := map[uint]bool{}
	add := func(id uint) {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	add(l.UpstreamId)
	rules, _ := ParseRouteRules(l.RouteRules)
	for _, r := range rules {
		if r.To > 0 {
			add(uint(r.To))
		}
	}
	return out
}

// HasRouteRules 线路是否用了分流规则。
func HasRouteRules(l model.Line) bool {
	rules, _ := ParseRouteRules(l.RouteRules)
	return len(rules) > 0
}

// routeRuleCond 一条规则的匹配条件(sing-box 路由规则的字段)。端口与端口区间在 sing-box 里是"或"的关系。
func routeRuleCond(r RouteRule) map[string]interface{} {
	if r.Type != "port" {
		return map[string]interface{}{r.Type: r.Values}
	}
	var ports []int
	var ranges []string
	for _, v := range r.Values {
		if strings.Contains(v, ":") {
			ranges = append(ranges, v)
		} else if n, err := strconv.Atoi(v); err == nil {
			ports = append(ports, n)
		}
	}
	m := map[string]interface{}{}
	if len(ports) > 0 {
		m["port"] = ports
	}
	if len(ranges) > 0 {
		m["port_range"] = ranges
	}
	return m
}

// routeRuleJSON 线路里一条分流规则对应的路由规则。
func routeRuleJSON(inbound string, r RouteRule, ups map[uint]model.Upstream) (json.RawMessage, error) {
	m := routeRuleCond(r)
	m["inbound"] = []string{inbound}
	switch {
	case r.To == RouteReject:
		m["action"] = "reject"
	case r.To == RouteDirect:
		m["action"], m["outbound"] = "route", "direct"
	default:
		up, ok := ups[uint(r.To)]
		if !ok {
			return nil, fmt.Errorf("指向不存在的上游 #%d", r.To)
		}
		m["action"], m["outbound"] = "route", up.Name
	}
	return json.Marshal(m)
}

// routeResolveRules 不放行内网时,线路里最后会走直连的那部分流量要先在本机解析域名(见 BuildConfig 里 resolve 那段)。
// 只对走直连的做:走上游的不在本机解析(解析失败会拒绝连接;上游那头的网络由上游自己管)。所以每条直连规则都排除掉
// 排在它前面、不走直连的规则;线路默认直连时排除全部不走直连的规则。
func routeResolveRules(l model.Line, rules []RouteRule) []json.RawMessage {
	var out []json.RawMessage
	var prior []interface{} // 排在前面、不走直连(上游 / 拦截)的规则条件
	emit := func(cond map[string]interface{}) {
		base := map[string]interface{}{"inbound": []string{l.Name}}
		for k, v := range cond {
			base[k] = v
		}
		rule := base
		if len(prior) > 0 {
			ex := append([]interface{}(nil), prior...)
			rule = map[string]interface{}{"type": "logical", "mode": "and", "rules": []interface{}{
				base,
				map[string]interface{}{"type": "logical", "mode": "or", "rules": ex, "invert": true},
			}}
		}
		rule["action"] = "resolve"
		b, _ := json.Marshal(rule)
		out = append(out, b)
	}
	for _, r := range rules {
		if r.To == RouteDirect {
			emit(routeRuleCond(r))
		} else {
			prior = append(prior, routeRuleCond(r))
		}
	}
	if l.UpstreamId == 0 {
		emit(nil)
	}
	return out
}
