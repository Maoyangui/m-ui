package sub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/upstream"
)

// httpLink 把本站 http / https 线路的链接(http://user:pass@host:port#name)转成 sing-box http 出站。
func httpLink(raw string) *upstream.Parsed {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	opts := map[string]interface{}{"server": u.Hostname(), "server_port": port}
	if u.User != nil {
		opts["username"] = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			opts["password"] = pw
		}
	}
	if u.Scheme == "https" {
		opts["tls"] = map[string]interface{}{"enabled": true, "server_name": u.Hostname()}
	}
	return &upstream.Parsed{Type: "http", Name: u.Fragment, Options: opts}
}

var (
	badLinkMu     sync.Mutex
	badLinkWarned = map[string]time.Time{}
)

// warnBadOwnLink 本站线路生成的链接 sing-box 那边解析不了(多半是线路的自定义地址写坏了,比如老数据里的 [v6]):
// 这个节点写不进配置。以前静默跳过,用户只看到少了一个节点,面板毫无提示。
// 链接里有凭据:日志只记节点名(# 后面)、协议和去掉了原链接的原因;同一个节点一小时只记一次(客户端会定时刷订阅)。
func warnBadOwnLink(link string, err error) {
	scheme, name := "", ""
	if i := strings.Index(link, "://"); i > 0 {
		scheme = link[:i]
	}
	if i := strings.LastIndex(link, "#"); i >= 0 {
		name, _ = url.PathUnescape(link[i+1:])
	}
	var ue *url.Error // url.Parse 的错误会带上整条链接(含密码),只留里面的原因
	if errors.As(err, &ue) {
		err = ue.Err
	}
	key := scheme + "|" + name
	now := time.Now()
	badLinkMu.Lock()
	if now.Sub(badLinkWarned[key]) < time.Hour {
		badLinkMu.Unlock()
		return
	}
	if len(badLinkWarned) > 1000 {
		badLinkWarned = map[string]time.Time{}
	}
	badLinkWarned[key] = now
	badLinkMu.Unlock()
	logger.Warning("sing-box 订阅:本站节点「", name, "」(", scheme, ")的链接解析不了,没写进配置,检查这条线路的自定义地址: ", err)
}

// BuildSingBoxSub 生成 sing-box 客户端配置(JSON,?format=json):
// SFA(Android)、SFI(iOS)、sing-box 桌面版都只认这种"远程配置",链接列表和 clash YAML 它们导不进去。
// 节点与链接订阅同源(本站线路 + 外部节点的分享链接),外加 proxy 选择组 / auto 测速组、
// TUN + 本地混合入站、DNS 与基础分流;字段按 sing-box 1.12+ 写法,不使用已废弃的 legacy 字段。
//
// 部署在多台服务器上的线路和 Clash 一样再出一个线路测速组(urltest,判断条件同 clash.go 的 url-test 组):
// 客户端选这条线路,由组在这几台服务器里挑延迟低的,出口和计费口径不变。节点逐条线路展开,
// 展开顺序、去重规则都和 GenerateLinksFor 一样,节点的 tag 与顺序一字不变(只有和线路组同名的外部节点改名);
// 组名在本站节点之后、外部节点之前起:撞了本站节点只改组名,外部节点撞上组名由外部节点改名。
func BuildSingBoxSub(user model.User, lines []model.Line, opt Options) (Result, error) {
	var tmpl map[string]interface{}
	if strings.TrimSpace(opt.SingBoxTmpl) != "" {
		if err := json.Unmarshal([]byte(opt.SingBoxTmpl), &tmpl); err != nil {
			return Result{}, fmt.Errorf("sing-box 模板不是合法 JSON: %w", err)
		}
	}
	var nodes []map[string]interface{}
	var tags []string
	used := map[string]bool{"proxy": true, "auto": true, "direct": true}
	var ownOutbounds []interface{} // 模板自己写的出站
	if raw, ok := tmpl["outbounds"].([]interface{}); ok {
		ownOutbounds = raw
		for _, o := range raw {
			if m, ok := o.(map[string]interface{}); ok {
				if t, _ := m["tag"].(string); t != "" {
					used[t] = true
				}
			}
		}
	}
	// add 把一条分享链接转成节点出站,返回它的 tag;认不出的链接跳过,返回空串(own = 本站线路生成的)
	add := func(l string, own bool) string {
		p, err := upstream.ParseLink(l)
		if err != nil {
			if p = httpLink(l); p == nil {
				if own {
					warnBadOwnLink(l, err) // 本站线路生成的链接也解析不了:节点少了一个,要让管理员看见
				}
				return "" // 外部节点里认不出的链接照旧跳过
			}
		}
		tag := p.Name
		if tag == "" {
			tag = p.Type
		}
		for i := 2; used[tag]; i++ {
			tag = fmt.Sprintf("%s %d", p.Name, i)
		}
		used[tag] = true
		ob := make(map[string]interface{}, len(p.Options)+2)
		for k, v := range p.Options {
			ob[k] = v
		}
		ob["type"], ob["tag"] = p.Type, tag
		nodes = append(nodes, ob)
		tags = append(tags, tag)
		return tag
	}
	type lineGroup struct {
		name    string
		members []string
	}
	var pending []lineGroup
	for _, line := range lines {
		var names []string
		for _, a := range resolveAddrs(line, opt.Entries, opt.LineNodes[line.Id]) {
			for _, l := range lineToURIs(line, user, a, line.Name+a.remark) {
				if tag := add(l, true); tag != "" {
					names = append(names, tag)
				}
			}
		}
		// 与 clash.go 的 url-test 组同一个条件:这条线路有不止一个入口(mixed 一个入口本来就出 socks + http 两个,不算)
		if len(names) > 1 && line.Protocol != "mixed" && strings.TrimSpace(line.Name) != "" {
			pending = append(pending, lineGroup{line.Name, names})
		}
	}
	// 线路组在本站节点之后、外部节点之前起名:撞了本站节点、模板出站或 proxy / auto / direct,只给组名加序号,本站节点名一字不动。
	// 外部节点最后定名,撞上组名由外部节点加序号:外部订阅里冒出一个和线路同名的节点,也挤不动本站线路组的名字,
	// 选过这个组的客户端(SFA / SFI 按 tag 记住选择)不会被悄悄换到第三方节点上
	var groups []map[string]interface{}
	var groupTags []string
	for _, g := range pending {
		tag := g.name
		for i := 2; used[tag]; i++ {
			tag = fmt.Sprintf("%s %d", g.name, i)
		}
		used[tag] = true
		groupTags = append(groupTags, tag)
		groups = append(groups, map[string]interface{}{"type": "urltest", "tag": tag, "outbounds": g.members,
			"url": "http://www.gstatic.com/generate_204", "interval": "3m", "tolerance": 50})
	}
	for _, e := range opt.External { // 外部节点不成组
		for _, l := range e.Links {
			add(l, false)
		}
	}
	if len(nodes) == 0 {
		// 一个节点都没有(线路还没分配):给一份能加载的配置,客户端不会因为 500 把整个订阅标成坏的
		cfg := map[string]interface{}{
			"log":       map[string]interface{}{"level": "info"},
			"outbounds": []map[string]interface{}{{"type": "direct", "tag": "direct"}},
			"route":     map[string]interface{}{"final": "direct"},
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		return Result{Body: string(b), Headers: headers(user, opt, "application/json; charset=utf-8")}, nil
	}

	selectList := append(append([]string{"auto"}, groupTags...), tags...)
	outbounds := []map[string]interface{}{
		{"type": "selector", "tag": "proxy", "outbounds": selectList, "default": "auto", "interrupt_exist_connections": true},
		{"type": "urltest", "tag": "auto", "outbounds": tags, "url": "http://www.gstatic.com/generate_204", "interval": "3m", "tolerance": 50},
	}
	outbounds = append(outbounds, groups...)
	outbounds = append(outbounds, nodes...)
	outbounds = append(outbounds, map[string]interface{}{"type": "direct", "tag": "direct"})

	if tmpl != nil {
		return singBoxFromTemplate(user, opt, tmpl, ownOutbounds, outbounds, nodes, tags)
	}
	cfg := singBoxBase()
	cfg["outbounds"] = outbounds
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Result{}, err
	}
	return Result{Body: string(b), Headers: headers(user, opt, "application/json; charset=utf-8")}, nil
}

// singBoxFromTemplate 按模板出配置:模板写了的段(dns、route、inbounds……)原样用,没写的用内置的。
// 节点接在模板的出站后面;模板的 selector / urltest 里写 "{all}" 会展开成全部节点(只是节点,不含线路组);
// proxy / auto / direct 与线路组模板没写就补上,写了用模板的。
func singBoxFromTemplate(user model.User, opt Options, tmpl map[string]interface{}, own []interface{}, generated, nodes []map[string]interface{}, tags []string) (Result, error) {
	cfg := singBoxBase()
	for k, v := range tmpl {
		cfg[k] = v
	}
	has := map[string]bool{}
	out := make([]interface{}, 0, len(own)+len(generated))
	for _, o := range own {
		if m, ok := o.(map[string]interface{}); ok {
			if t, _ := m["tag"].(string); t != "" {
				has[t] = true
			}
			if list, ok := m["outbounds"].([]interface{}); ok {
				m["outbounds"] = expandAll(list, tags)
			}
		}
		out = append(out, o)
	}
	for _, g := range generated {
		if t, _ := g["tag"].(string); !has[t] {
			out = append(out, g)
		}
	}
	cfg["outbounds"] = out
	// 模板的 route 没写默认解析器:sing-box 1.14 起缺了它整份配置起不来(实测 FATAL),而模板作者多半只想改分流。
	// 从 dns 段里挑一个不走代理的服务器补上;挑不出来就不动,让客户端报错说清楚
	if route, ok := cfg["route"].(map[string]interface{}); ok {
		if _, has := route["default_domain_resolver"]; !has {
			if tag := directDNSTag(cfg["dns"]); tag != "" {
				route["default_domain_resolver"] = tag
			}
		}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Result{}, err
	}
	return Result{Body: string(b), Headers: headers(user, opt, "application/json; charset=utf-8")}, nil
}

// directDNSTag dns 段里不走代理的服务器:优先系统解析(type local),其次第一个没写 detour 的。
func directDNSTag(dns interface{}) string {
	m, _ := dns.(map[string]interface{})
	var servers []map[string]interface{}
	switch v := m["servers"].(type) {
	case []map[string]interface{}:
		servers = v
	case []interface{}:
		for _, s := range v {
			if x, ok := s.(map[string]interface{}); ok {
				servers = append(servers, x)
			}
		}
	}
	first := ""
	for _, s := range servers {
		tag, _ := s["tag"].(string)
		if tag == "" || s["detour"] != nil {
			continue
		}
		if s["type"] == "local" {
			return tag
		}
		if first == "" {
			first = tag
		}
	}
	return first
}

// expandAll 把列表里的 "{all}" 换成全部节点。
func expandAll(list []interface{}, tags []string) []interface{} {
	out := make([]interface{}, 0, len(list)+len(tags))
	for _, v := range list {
		if s, _ := v.(string); s == "{all}" {
			for _, t := range tags {
				out = append(out, t)
			}
			continue
		}
		out = append(out, v)
	}
	return out
}

// DefaultSingBoxTemplate 内置的 sing-box 骨架(不含节点),面板上「填入内置模板」用它。
func DefaultSingBoxTemplate() string {
	b, _ := json.MarshalIndent(singBoxBase(), "", "  ")
	return string(b)
}

// singBoxBase 内置的 sing-box 配置骨架:TUN + 本地混合入站、DNS 与基础分流。
func singBoxBase() map[string]interface{} {
	return map[string]interface{}{
		"log": map[string]interface{}{"level": "info", "timestamp": true},
		"dns": map[string]interface{}{
			"servers": []map[string]interface{}{
				{"type": "https", "tag": "remote", "server": "1.1.1.1", "detour": "proxy"},
				{"type": "local", "tag": "local"},
			},
			"final":    "remote",
			"strategy": "prefer_ipv4",
		},
		"inbounds": []map[string]interface{}{
			{"type": "tun", "tag": "tun-in", "address": []string{"172.19.0.1/30", "fdfe:dcba:9876::1/126"}, "auto_route": true, "strict_route": false},
			{"type": "mixed", "tag": "mixed-in", "listen": "127.0.0.1", "listen_port": 2080},
		},
		"route": map[string]interface{}{
			"rules": []map[string]interface{}{
				{"action": "sniff"},
				{"protocol": "dns", "action": "hijack-dns"},
				{"ip_is_private": true, "outbound": "direct"},
				{"clash_mode": "Direct", "outbound": "direct"},
				{"clash_mode": "Global", "outbound": "proxy"},
			},
			"final":                   "proxy",
			"auto_detect_interface":   true,
			"default_domain_resolver": "local",
		},
		"experimental": map[string]interface{}{
			"clash_api":  map[string]interface{}{"external_controller": "127.0.0.1:9090"},
			"cache_file": map[string]interface{}{"enabled": true},
		},
	}
}
