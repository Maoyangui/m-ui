// Package ext 处理外部节点:抓取外部订阅、解析成分享链接与 clash 代理,聚合进用户订阅。
package ext

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/upstream"

	"gopkg.in/yaml.v3"
)

// Items 是一份外部内容解析后的节点集合。
type Items struct {
	Links []string                 // 分享链接(用于链接订阅)
	Clash []map[string]interface{} // clash 代理(用于 clash 订阅)
}

// Fetch 抓取外部订阅。UA 用通用客户端标识,让服务商返回链接列表而不是 clash YAML(两种都能解析)。
func Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("订阅地址必须是 http(s) URL")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "v2rayN/7.0 m-ui")
	c := &http.Client{Timeout: 25 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, errors.New("订阅内容为空")
	}
	return b, nil
}

// Parse 解析外部内容:base64 链接列表 / 明文链接列表 / clash YAML(取 proxies)。
func Parse(content string) Items {
	var it Items
	text := strings.TrimSpace(content)
	if text == "" {
		return it
	}
	if looksLikeClash(text) {
		var doc struct {
			Proxies []map[string]interface{} `yaml:"proxies"`
		}
		if yaml.Unmarshal([]byte(text), &doc) == nil {
			for _, p := range doc.Proxies {
				if name, _ := p["name"].(string); name != "" {
					np := normalizeYAML(p)
					it.Clash = append(it.Clash, np)
					// clash YAML 里的节点也转成分享链接,链接订阅(小火箭 / nextin 等)才看得到
					if link, ok := ClashToLink(np); ok {
						it.Links = append(it.Links, link)
					}
				}
			}
		}
		return it
	}
	if !strings.Contains(text, "://") {
		if dec, err := decodeBase64Loose(text); err == nil && strings.Contains(dec, "://") {
			text = dec
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "://") {
			continue
		}
		it.Links = append(it.Links, line)
		if p, ok := LinkToClash(line); ok {
			it.Clash = append(it.Clash, p)
		}
	}
	return it
}

func looksLikeClash(text string) bool {
	head := text
	if len(head) > 4096 {
		head = head[:4096]
	}
	return strings.Contains(head, "proxies:") || strings.HasPrefix(head, "proxy-groups:") || strings.HasPrefix(head, "mixed-port:") || strings.HasPrefix(head, "port:")
}

func decodeBase64Loose(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("-", "+", "_", "/", "\n", "", "\r", "", " ", "").Replace(s)
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// normalizeYAML 把 yaml.v3 解出的 map[string]interface{} 里的嵌套 map[interface{}]interface{} 规整成 string 键。
func normalizeYAML(v map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(v))
	for k, val := range v {
		out[k] = normalizeValue(val)
	}
	return out
}

func normalizeValue(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		return normalizeYAML(x)
	case map[interface{}]interface{}:
		m := make(map[string]interface{}, len(x))
		for k, val := range x {
			m[fmt.Sprint(k)] = normalizeValue(val)
		}
		return m
	case []interface{}:
		for i := range x {
			x[i] = normalizeValue(x[i])
		}
		return x
	}
	return v
}

// WithPrefix 给节点名加前缀(链接改 fragment,clash 改 name)。
func WithPrefix(it Items, prefix string) Items {
	if prefix == "" {
		return it
	}
	out := Items{}
	for _, l := range it.Links {
		if i := strings.LastIndex(l, "#"); i >= 0 {
			out.Links = append(out.Links, l[:i+1]+url.PathEscape(prefix)+l[i+1:])
		} else {
			out.Links = append(out.Links, l+"#"+url.PathEscape(prefix))
		}
	}
	for _, p := range it.Clash {
		q := make(map[string]interface{}, len(p))
		for k, v := range p {
			q[k] = v
		}
		if name, _ := p["name"].(string); name != "" {
			q["name"] = prefix + name
		}
		out.Clash = append(out.Clash, q)
	}
	return out
}

// LinkToClash 把一条分享链接转成 clash/mihomo 代理(经 sing-box 出站参数中转)。
func LinkToClash(link string) (map[string]interface{}, bool) {
	p, err := upstream.ParseLink(link)
	if err != nil {
		return nil, false
	}
	return OutboundToClash(p.Type, p.Name, p.Options)
}

// OutboundToClash 把 sing-box 出站参数映射为 clash 代理字段。
func OutboundToClash(typ, name string, o map[string]interface{}) (map[string]interface{}, bool) {
	str := func(k string) string { s, _ := o[k].(string); return s }
	num := func(k string) int {
		switch v := o[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		}
		return 0
	}
	p := map[string]interface{}{"name": name, "server": str("server"), "port": num("server_port")}
	if p["server"] == "" || p["port"] == 0 {
		return nil, false
	}
	tls, _ := o["tls"].(map[string]interface{})
	tlsOn := tls != nil && tls["enabled"] == true
	applyTLS := func(sniKey string) {
		if !tlsOn {
			return
		}
		p["tls"] = true
		if sn, _ := tls["server_name"].(string); sn != "" {
			p[sniKey] = sn
		}
		if ins, _ := tls["insecure"].(bool); ins {
			p["skip-cert-verify"] = true
		}
		if alpn, ok := tls["alpn"].([]interface{}); ok && len(alpn) > 0 {
			p["alpn"] = alpn
		} else if a, ok := tls["alpn"].(string); ok && a != "" {
			p["alpn"] = []string{a}
		}
		if utls, ok := tls["utls"].(map[string]interface{}); ok {
			if fp, _ := utls["fingerprint"].(string); fp != "" {
				p["client-fingerprint"] = fp
			}
		}
		if re, ok := tls["reality"].(map[string]interface{}); ok && re["enabled"] == true {
			ro := map[string]interface{}{"public-key": re["public_key"]}
			if sid, _ := re["short_id"].(string); sid != "" {
				ro["short-id"] = sid
			}
			p["reality-opts"] = ro
			if _, has := p["client-fingerprint"]; !has {
				p["client-fingerprint"] = "chrome"
			}
		}
	}
	applyTransport := func() {
		tr, _ := o["transport"].(map[string]interface{})
		if tr == nil {
			return
		}
		switch tr["type"] {
		case "ws":
			p["network"] = "ws"
			wo := map[string]interface{}{}
			if path, _ := tr["path"].(string); path != "" {
				wo["path"] = path
			}
			if h, ok := tr["headers"].(map[string]interface{}); ok {
				if host, _ := h["Host"].(string); host != "" {
					wo["headers"] = map[string]interface{}{"Host": host}
				}
			}
			p["ws-opts"] = wo
		case "httpupgrade":
			p["network"] = "ws"
			wo := map[string]interface{}{"v2ray-http-upgrade": true}
			if path, _ := tr["path"].(string); path != "" {
				wo["path"] = path
			}
			if host, _ := tr["host"].(string); host != "" {
				wo["headers"] = map[string]interface{}{"Host": host}
			}
			p["ws-opts"] = wo
		case "grpc":
			p["network"] = "grpc"
			p["grpc-opts"] = map[string]interface{}{"grpc-service-name": tr["service_name"]}
		case "http":
			p["network"] = "h2"
			ho := map[string]interface{}{}
			if path, _ := tr["path"].(string); path != "" {
				ho["path"] = path
			}
			if hosts, ok := tr["host"].([]interface{}); ok {
				ho["host"] = hosts
			} else if h, ok := tr["host"].(string); ok && h != "" {
				ho["host"] = []string{h}
			}
			p["h2-opts"] = ho
		}
	}
	switch typ {
	case "vless":
		p["type"] = "vless"
		p["uuid"] = str("uuid")
		p["udp"] = true
		if f := str("flow"); f != "" {
			p["flow"] = f
		}
		if pe := str("packet_encoding"); pe != "" {
			p["packet-encoding"] = pe
		}
		applyTLS("servername")
		applyTransport()
	case "vmess":
		p["type"] = "vmess"
		p["uuid"] = str("uuid")
		p["alterId"] = num("alter_id")
		if sec := str("security"); sec != "" {
			p["cipher"] = sec
		} else {
			p["cipher"] = "auto"
		}
		p["udp"] = true
		applyTLS("servername")
		applyTransport()
	case "trojan":
		p["type"] = "trojan"
		p["password"] = str("password")
		p["udp"] = true
		applyTLS("sni")
		applyTransport()
	case "shadowsocks":
		p["type"] = "ss"
		p["cipher"] = str("method")
		p["password"] = str("password")
		p["udp"] = true
		if plugin := str("plugin"); plugin != "" {
			p["plugin"] = plugin
			p["plugin-opts"] = str("plugin_opts")
		}
	case "hysteria2":
		p["type"] = "hysteria2"
		p["password"] = str("password")
		applyTLS("sni")
		if obfs, ok := o["obfs"].(map[string]interface{}); ok {
			p["obfs"], _ = obfs["type"].(string)
			p["obfs-password"], _ = obfs["password"].(string)
		}
		if up := num("up_mbps"); up > 0 {
			p["up"] = fmt.Sprintf("%d Mbps", up)
		}
		if down := num("down_mbps"); down > 0 {
			p["down"] = fmt.Sprintf("%d Mbps", down)
		}
	case "tuic":
		p["type"] = "tuic"
		p["uuid"] = str("uuid")
		p["password"] = str("password")
		applyTLS("sni")
		if cc := str("congestion_control"); cc != "" {
			p["congestion-controller"] = cc
		}
		if m := str("udp_relay_mode"); m != "" {
			p["udp-relay-mode"] = m
		}
	case "anytls":
		p["type"] = "anytls"
		p["password"] = str("password")
		applyTLS("sni")
	case "socks":
		p["type"] = "socks5"
		if u := str("username"); u != "" {
			p["username"] = u
			p["password"] = str("password")
		}
		p["udp"] = true
	default:
		return nil, false
	}
	return p, true
}
