// Package ext 处理外部节点:抓取外部订阅、解析成分享链接与 clash 代理,聚合进用户订阅。
package ext

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// fetchAgent 抓取时冒充的一种客户端身份。id 存进库(ExtNode.FetchAgent),UA 串以后改了也认得。
type fetchAgent struct{ id, ua string }

// fetchAgents 按这个顺序试。有的面板按客户端发不同内容:Clash 系给全量 YAML,
// 通用身份只给它认为该客户端支持的一两种协议(真机遇到过:同一条订阅 5 个 vs 117 个)。
var fetchAgents = []fetchAgent{{"clash", "ClashMeta/1.18.0 (m-ui)"}, {"v2rayn", "v2rayN/7.0 m-ui"}}

// fetchAttemptTimeout 每个身份单独计时。以前两次请求共用调用方的 30 秒:第一次超时 25 秒后,第二次只剩 5 秒。
var fetchAttemptTimeout = 25 * time.Second

// Fetch 抓取外部订阅,返回内容和这次用的身份。先以 Clash Meta 身份要,只有"HTTP 200 但解析不出节点"才换下一个身份。
// prev 是上次抓取成功用的身份(缓存就是它给的;空 = 还没有缓存):
//   - 这个身份出错(网络错误、4xx、5xx、超时)直接返回错误,调用方保留旧缓存。以前任何错误都换身份,
//     机场在 Cloudflare / WAF 后面间歇 429 时,缓存在两种身份的内容之间来回翻,节点集合和名字都变,隐藏随之失配;
//   - 别的身份出错不算数,接着试下一个(缓存本来就不是它给的);
//   - 还没有缓存时任何错误都接着试下一个,都失败才报第一个错误。
//
// 顺序固定先 Clash 身份:哪怕上次是通用身份给的,Clash 身份恢复了也换回节点更全的那份,不会因为一次维护页永远卡在少的那份上。
// 都解析不出节点时把第一份 200 的原文交出去,让调用方报"没有可识别的节点"(同样保留旧缓存)。
func Fetch(ctx context.Context, rawURL, prev string) (body []byte, agent string, err error) {
	if prev != "" && !knownAgent(prev) {
		prev = fetchAgents[0].id // 老数据没记身份:当时总是先用 Clash 身份
	}
	var spare []byte // 没有能进 Clash 订阅的节点的那份:都没有更好的就交它
	var spareAgent string
	spareNodes := false
	var firstErr error
	for _, a := range fetchAgents {
		b, err := fetchAs(ctx, rawURL, a.ua)
		if err != nil {
			if a.id == prev {
				return nil, "", err
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		it := Parse(string(b))
		if len(it.Clash()) > 0 {
			return b, a.id, nil
		}
		// 一个能用的节点都没有(或者只有本站不认识的链接):换下一个身份试;都这样就交第一份(有链接的优先)
		if spare == nil || (!spareNodes && len(it.Nodes) > 0) {
			spare, spareAgent, spareNodes = b, a.id, len(it.Nodes) > 0
		}
	}
	if spare != nil {
		return spare, spareAgent, nil
	}
	return nil, "", firstErr
}

func knownAgent(id string) bool {
	for _, a := range fetchAgents {
		if a.id == id {
			return true
		}
	}
	return false
}

func fetchAs(ctx context.Context, rawURL, agent string) ([]byte, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("订阅地址必须是 http(s) URL")
	}
	ctx, cancel := context.WithTimeout(ctx, fetchAttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", agent)
	c := &http.Client{Timeout: fetchAttemptTimeout}
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

// Parse 解析外部内容:base64 链接列表 / 明文链接列表 / clash YAML(取 proxies)/ sing-box JSON(取 outbounds)。
// 每个节点同时带名字、指纹、链接与 Clash 代理(见 Node),来自同一次解析。
func Parse(content string) Items {
	var it Items
	text := strings.TrimSpace(content)
	if text == "" {
		return it
	}
	if strings.HasPrefix(text, "{") {
		var doc struct {
			Outbounds []map[string]interface{} `json:"outbounds"`
		}
		if json.Unmarshal([]byte(text), &doc) == nil && len(doc.Outbounds) > 0 {
			for _, o := range doc.Outbounds {
				typ, _ := o["type"].(string)
				tag, _ := o["tag"].(string)
				if tag == "" || o["server"] == nil {
					continue // selector / urltest / direct 这些不是节点
				}
				if p, ok := OutboundToClash(typ, tag, o); ok {
					n := clashNode(p)
					n.SingBox = o
					it.Nodes = append(it.Nodes, n)
				}
			}
			return it
		}
	}
	if looksLikeClash(text) {
		var doc struct {
			Proxies []map[string]interface{} `yaml:"proxies"`
		}
		if yaml.Unmarshal([]byte(text), &doc) == nil {
			for _, p := range doc.Proxies {
				if name, _ := p["name"].(string); name != "" {
					// clash YAML 里的节点也转成分享链接,链接订阅(小火箭 / nextin 等)才看得到
					it.Nodes = append(it.Nodes, clashNode(normalizeYAML(p)))
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
		it.Nodes = append(it.Nodes, linkNode(line))
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

// WithPrefix 给节点名加前缀。链接和 Clash 按同一个名字改:vmess 改 base64 里的 ps(以前在链接末尾追加 "#前缀",
// vmess 链接因此不再是合法 base64,sing-box 订阅整条跳过它);没名字的链接以前只剩前缀,现在是前缀 + 服务器地址,和 Clash 一致。
func WithPrefix(it Items, prefix string) Items {
	if prefix == "" {
		return it
	}
	out := Items{Nodes: make([]Node, 0, len(it.Nodes))}
	for _, n := range it.Nodes {
		out.Nodes = append(out.Nodes, n.renamed(prefix+n.Name))
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
			// sing-box 的插件参数是 SIP003 字符串,Clash 要的是 map:原样塞字符串 Clash 不认,整份配置加载失败
			name, opts := pluginToClash(plugin, str("plugin_opts"))
			p["plugin"] = name
			if len(opts) > 0 {
				p["plugin-opts"] = opts
			}
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
