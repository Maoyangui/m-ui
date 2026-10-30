package sub

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
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

// BuildSingBoxSub 生成 sing-box 客户端配置(JSON,?format=json):
// SFA(Android)、SFI(iOS)、sing-box 桌面版都只认这种"远程配置",链接列表和 clash YAML 它们导不进去。
// 节点与链接订阅同源(本站线路 + 外部节点的分享链接),外加 proxy 选择组 / auto 测速组、
// TUN + 本地混合入站、DNS 与基础分流;字段按 sing-box 1.12+ 写法,不使用已废弃的 legacy 字段。
func BuildSingBoxSub(user model.User, lines []model.Line, opt Options) (Result, error) {
	links := GenerateLinksFor(user, lines, opt.Entries, opt.LineNodes)
	for _, e := range opt.External {
		links = append(links, e.Links...)
	}
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
	for _, l := range links {
		p, err := upstream.ParseLink(l)
		if err != nil {
			if p = httpLink(l); p == nil {
				continue // 认不出的链接跳过
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

	outbounds := []map[string]interface{}{
		{"type": "selector", "tag": "proxy", "outbounds": append([]string{"auto"}, tags...), "default": "auto", "interrupt_exist_connections": true},
		{"type": "urltest", "tag": "auto", "outbounds": tags, "url": "http://www.gstatic.com/generate_204", "interval": "3m", "tolerance": 50},
	}
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
// 节点接在模板的出站后面;模板的 selector / urltest 里写 "{all}" 会展开成全部节点;
// proxy / auto / direct 模板没写就补上,写了用模板的。
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
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Result{}, err
	}
	return Result{Body: string(b), Headers: headers(user, opt, "application/json; charset=utf-8")}, nil
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
