package sub

import (
	"encoding/json"
	"fmt"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/upstream"
)

// BuildSingBoxSub 生成 sing-box 客户端配置(JSON,?format=json):
// SFA(Android)、SFI(iOS)、sing-box 桌面版都只认这种"远程配置",链接列表和 clash YAML 它们导不进去。
// 节点与链接订阅同源(本站线路 + 外部节点的分享链接),外加 proxy 选择组 / auto 测速组、
// TUN + 本地混合入站、DNS 与基础分流;字段按 sing-box 1.12+ 写法,不使用已废弃的 legacy 字段。
func BuildSingBoxSub(user model.User, lines []model.Line, opt Options) (Result, error) {
	links := GenerateLinks(user, lines, opt.Entries)
	for _, e := range opt.External {
		links = append(links, e.Links...)
	}
	var nodes []map[string]interface{}
	var tags []string
	used := map[string]bool{"proxy": true, "auto": true, "direct": true}
	for _, l := range links {
		p, err := upstream.ParseLink(l)
		if err != nil {
			continue // http 等 sing-box 客户端不常用的类型跳过
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
		return Result{}, fmt.Errorf("没有可用节点")
	}

	outbounds := []map[string]interface{}{
		{"type": "selector", "tag": "proxy", "outbounds": append([]string{"auto"}, tags...), "default": "auto", "interrupt_exist_connections": true},
		{"type": "urltest", "tag": "auto", "outbounds": tags, "url": "http://www.gstatic.com/generate_204", "interval": "3m", "tolerance": 50},
	}
	outbounds = append(outbounds, nodes...)
	outbounds = append(outbounds, map[string]interface{}{"type": "direct", "tag": "direct"})

	cfg := map[string]interface{}{
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
		"outbounds": outbounds,
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
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return Result{}, err
	}
	return Result{Body: string(b), Headers: headers(user, opt, "application/json; charset=utf-8")}, nil
}
