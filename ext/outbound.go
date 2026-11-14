package ext

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Outbound 节点的 sing-box 出站(类型 + 参数,不含 tag),测速与"添加为上游"用。
// 来源是 sing-box JSON 的直接用原件;其余由 Clash 代理直接映射(ClashToOutbound)。以前经"Clash → 分享链接 → 解析"
// 绕一圈,ss 插件、vmess 的跳过证书校验、httpupgrade、h2 的 Host 都会丢:测速误报不通,加成上游后线路连不上。
// 映射不了或会丢参数的返回错误(说明原因),面板上标成"不能测 / 不能作上游",不拿一个残缺的出站去测。
func Outbound(n Node) (string, map[string]interface{}, error) {
	if n.SingBox != nil {
		return singBoxOutbound(n.SingBox)
	}
	if n.Clash == nil {
		typ, _, _ := linkBasics(n.Link)
		return "", nil, fmt.Errorf("协议 %s 暂不支持测速和作上游", firstNonEmpty(typ, "?"))
	}
	return ClashToOutbound(n.Clash)
}

// singBoxTypes 本站能当上游的 sing-box 出站类型(和 core.OutboundRegistry 注册的一致)。
var singBoxTypes = map[string]bool{"shadowsocks": true, "vmess": true, "vless": true, "trojan": true, "hysteria2": true,
	"tuic": true, "anytls": true, "socks": true, "http": true}

func singBoxOutbound(o map[string]interface{}) (string, map[string]interface{}, error) {
	typ, _ := o["type"].(string)
	if !singBoxTypes[typ] {
		return "", nil, fmt.Errorf("协议 %s 暂不支持测速和作上游", typ)
	}
	if d, _ := o["detour"].(string); d != "" {
		return "", nil, lossErr("detour", d) // 依赖订阅里别的出站,单独拿出来连不上
	}
	opts := make(map[string]interface{}, len(o))
	for k, v := range o {
		switch k {
		case "type", "tag", "domain_resolver", "domain_strategy": // 后两个引用订阅里的 DNS 设置,本站的配置里没有
			continue
		}
		opts[k] = v
	}
	return typ, opts, nil
}

func lossErr(key string, val interface{}) error {
	return fmt.Errorf("参数 %s=%v 暂不支持", key, val)
}

func missing(key string) error { return fmt.Errorf("缺少参数 %s", key) }

// clashReader 读 Clash 代理字段的小工具(YAML 里同一个字段可能是字符串、数字或布尔)。
type clashReader struct{ p map[string]interface{} }

func (c clashReader) str(k string) string {
	switch v := c.p[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case int, int64, float64:
		return fmt.Sprint(v)
	}
	return ""
}

func (c clashReader) bool(k string) bool {
	switch v := c.p[k].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	}
	return false
}

func (c clashReader) sub(k string) map[string]interface{} {
	m, _ := c.p[k].(map[string]interface{})
	return m
}

// ClashToOutbound 把 Clash / mihomo 代理直接转成 sing-box 出站。支持 ss(含 obfs / v2ray-plugin 插件)、vmess、vless(含 reality)、
// trojan、hysteria2(含端口跳跃、salamander)、tuic v5、anytls、socks5、http(和本站数据面注册的出站类型一致,见 core.OutboundRegistry);
// 传输层支持 ws(含 v2ray-http-upgrade → httpupgrade、early data)、h2、http、grpc;另带 smux 多路复用、tfo、ECH。
func ClashToOutbound(p map[string]interface{}) (string, map[string]interface{}, error) {
	c := clashReader{p}
	rawType := c.str("type")
	typ := normType(rawType)
	server, port := strings.Trim(c.str("server"), "[]"), toInt(p["port"])
	if server == "" || port <= 0 || port > 65535 {
		return "", nil, errors.New("缺少服务器地址或端口")
	}
	if dp := c.str("dialer-proxy"); dp != "" {
		return "", nil, lossErr("dialer-proxy", dp) // 先经订阅里另一个代理再连:单独拿出来测不出真实情况
	}
	o := map[string]interface{}{"server": server, "server_port": port}
	if c.bool("tfo") {
		o["tcp_fast_open"] = true
	}
	if c.bool("mptcp") {
		o["tcp_multi_path"] = true
	}
	network := strings.ToLower(firstNonEmpty(c.str("network"), "tcp"))
	withStream := func(tlsForce bool, sniFallback bool, alpn []string) error {
		tr, info, err := c.transportOut(network)
		if err != nil {
			return err
		}
		hint := ""
		if sniFallback { // vmess / vless 没写 SNI 时 Clash 用 Host 头当 SNI,sing-box 默认用服务器地址:照 Clash 的来
			hint = info.host
			if info.kind == "h2" && len(info.hosts) > 0 {
				hint = info.hosts[0]
			}
		}
		tls, err := c.tlsOut(tlsForce, hint, alpn)
		if err != nil {
			return err
		}
		if tls != nil {
			o["tls"] = tls
		}
		if tr != nil {
			o["transport"] = tr
		}
		if mux := c.muxOut(); mux != nil {
			o["multiplex"] = mux
		}
		return nil
	}
	switch typ {
	case "ss":
		method := c.str("cipher")
		if method == "dummy" {
			method = "none"
		}
		if method == "" {
			return "", nil, missing("cipher")
		}
		o["method"], o["password"] = method, c.str("password")
		if c.str("plugin") != "" {
			plugin, opts, err := clashPluginSIP003(p)
			if err != nil {
				return "", nil, err
			}
			o["plugin"] = plugin
			if opts != "" {
				o["plugin_opts"] = opts
			}
		}
		if c.bool("udp-over-tcp") {
			u := map[string]interface{}{"enabled": true}
			if v := toInt(p["udp-over-tcp-version"]); v > 0 {
				u["version"] = v
			}
			o["udp_over_tcp"] = u
		}
		if mux := c.muxOut(); mux != nil {
			o["multiplex"] = mux
		}
		return "shadowsocks", o, nil
	case "vmess":
		id := c.str("uuid")
		if id == "" {
			return "", nil, missing("uuid")
		}
		o["uuid"] = id
		o["security"] = firstNonEmpty(c.str("cipher"), "auto")
		if aid := toInt(p["alterId"]); aid > 0 {
			o["alter_id"] = aid
		}
		if c.bool("global-padding") {
			o["global_padding"] = true
		}
		if c.bool("authenticated-length") {
			o["authenticated_length"] = true
		}
		if pe := packetEncoding(c); pe != "" {
			o["packet_encoding"] = pe
		}
		if err := withStream(false, true, nil); err != nil {
			return "", nil, err
		}
		return "vmess", o, nil
	case "vless":
		id := c.str("uuid")
		if id == "" {
			return "", nil, missing("uuid")
		}
		o["uuid"] = id
		if flow := c.str("flow"); flow != "" {
			if flow != "xtls-rprx-vision" {
				return "", nil, lossErr("flow", flow)
			}
			o["flow"] = flow
		}
		if enc := c.str("encryption"); enc != "" && enc != "none" {
			return "", nil, lossErr("encryption", enc)
		}
		if pe := packetEncoding(c); pe != "" {
			o["packet_encoding"] = pe
		}
		if err := withStream(false, true, nil); err != nil {
			return "", nil, err
		}
		return "vless", o, nil
	case "trojan":
		pw := c.str("password")
		if pw == "" {
			return "", nil, missing("password")
		}
		o["password"] = pw
		if so := c.sub("ss-opts"); so != nil && so["enabled"] == true {
			return "", nil, lossErr("ss-opts.enabled", true)
		}
		if err := withStream(true, false, nil); err != nil {
			return "", nil, err
		}
		return "trojan", o, nil
	case "hysteria2":
		o["password"] = firstNonEmpty(c.str("password"), c.str("auth"))
		if err := c.hopping(o); err != nil {
			return "", nil, err
		}
		if up := mbps(p["up"]); up > 0 {
			o["up_mbps"] = up
		}
		if down := mbps(p["down"]); down > 0 {
			o["down_mbps"] = down
		}
		if obfs := c.str("obfs"); obfs != "" {
			if obfs != "salamander" {
				return "", nil, lossErr("obfs", obfs)
			}
			o["obfs"] = map[string]interface{}{"type": "salamander", "password": c.str("obfs-password")}
		}
		tls, err := c.tlsOut(true, "", []string{"h3"})
		if err != nil {
			return "", nil, err
		}
		o["tls"] = tls
		return "hysteria2", o, nil
	case "tuic":
		if c.str("token") != "" && c.str("uuid") == "" {
			return "", nil, lossErr("token", "v4") // TUIC v4,sing-box 只支持 v5
		}
		id := c.str("uuid")
		if id == "" {
			return "", nil, missing("uuid")
		}
		o["uuid"], o["password"] = id, c.str("password")
		if cc := c.str("congestion-controller"); cc != "" {
			o["congestion_control"] = cc
		}
		if m := c.str("udp-relay-mode"); m != "" {
			o["udp_relay_mode"] = m
		}
		if c.bool("reduce-rtt") {
			o["zero_rtt_handshake"] = true
		}
		if c.bool("udp-over-stream") {
			o["udp_over_stream"] = true
		}
		if hb := toInt(p["heartbeat-interval"]); hb > 0 {
			o["heartbeat"] = strconv.Itoa(hb) + "ms"
		}
		tls, err := c.tlsOut(true, "", []string{"h3"})
		if err != nil {
			return "", nil, err
		}
		o["tls"] = tls
		return "tuic", o, nil
	case "anytls":
		pw := c.str("password")
		if pw == "" {
			return "", nil, missing("password")
		}
		o["password"] = pw
		if v := toInt(p["idle-session-check-interval"]); v > 0 {
			o["idle_session_check_interval"] = strconv.Itoa(v) + "s"
		}
		if v := toInt(p["idle-session-timeout"]); v > 0 {
			o["idle_session_timeout"] = strconv.Itoa(v) + "s"
		}
		if v := toInt(p["min-idle-session"]); v > 0 {
			o["min_idle_session"] = v
		}
		tls, err := c.tlsOut(true, "", nil)
		if err != nil {
			return "", nil, err
		}
		o["tls"] = tls
		return "anytls", o, nil
	case "socks5":
		if c.bool("tls") {
			return "", nil, lossErr("tls", true)
		}
		o["version"] = "5"
		if u := c.str("username"); u != "" {
			o["username"], o["password"] = u, c.str("password")
		}
		return "socks", o, nil
	case "http":
		if u := c.str("username"); u != "" {
			o["username"], o["password"] = u, c.str("password")
		}
		if h := c.sub("headers"); len(h) > 0 {
			o["headers"] = h
		}
		tls, err := c.tlsOut(false, "", nil)
		if err != nil {
			return "", nil, err
		}
		if tls != nil {
			o["tls"] = tls
		}
		return "http", o, nil
	}
	return "", nil, fmt.Errorf("协议 %s 暂不支持测速和作上游", firstNonEmpty(rawType, "?"))
}

func packetEncoding(c clashReader) string {
	switch {
	case c.str("packet-encoding") != "":
		return c.str("packet-encoding")
	case c.bool("xudp"):
		return "xudp"
	case c.bool("packet-addr"):
		return "packetaddr"
	}
	return ""
}

// tlsOut Clash 的 TLS 参数 → sing-box 出站的 tls。force:协议本身就是 TLS / QUIC;sniHint:没写 SNI 时用的名字。
func (c clashReader) tlsOut(force bool, sniHint string, defaultALPN []string) (map[string]interface{}, error) {
	re := c.sub("reality-opts")
	if !force && !c.bool("tls") && re == nil {
		return nil, nil
	}
	t := map[string]interface{}{"enabled": true}
	if sni := firstNonEmpty(c.str("sni"), c.str("servername"), c.str("peer"), sniHint); sni != "" {
		t["server_name"] = sni
	}
	if c.bool("skip-cert-verify") {
		t["insecure"] = true
	}
	if alpn := stringList(c.p["alpn"]); len(alpn) > 0 {
		t["alpn"] = alpn
	} else if len(defaultALPN) > 0 {
		t["alpn"] = defaultALPN
	}
	fp := c.str("client-fingerprint")
	if re != nil {
		pk, _ := re["public-key"].(string)
		if pk == "" {
			return nil, missing("reality-opts.public-key")
		}
		r := map[string]interface{}{"enabled": true, "public_key": pk}
		if sid := fmt.Sprint(re["short-id"]); sid != "" && sid != "<nil>" {
			r["short_id"] = sid
		}
		t["reality"] = r
		if fp == "" {
			fp = "chrome" // reality 必须带 uTLS 指纹
		}
	}
	if fp != "" && fp != "none" {
		t["utls"] = map[string]interface{}{"enabled": true, "fingerprint": fp}
	}
	if c.bool("disable-sni") {
		t["disable_sni"] = true
	}
	if ech := c.sub("ech-opts"); ech != nil && ech["enable"] == true {
		e := map[string]interface{}{"enabled": true}
		if cfg, _ := ech["config"].(string); cfg != "" {
			e["config"] = []string{"-----BEGIN ECH CONFIGS-----", cfg, "-----END ECH CONFIGS-----"}
		}
		t["ech"] = e
	}
	return t, nil
}

// transportOut Clash 的 network + *-opts → sing-box 出站的 transport(TCP 返回 nil)。
func (c clashReader) transportOut(network string) (map[string]interface{}, clashTransport, error) {
	tr := transportOfClash(c.p, network)
	withHeaders := func(t map[string]interface{}) {
		if len(tr.headers) > 0 {
			t["headers"] = tr.headers
		}
	}
	switch tr.kind {
	case "", "tcp":
		return nil, tr, nil
	case "ws":
		t := map[string]interface{}{"type": "ws"}
		if tr.path != "" {
			t["path"] = tr.path
		}
		h := map[string]interface{}{}
		for k, v := range tr.headers {
			h[k] = v
		}
		if tr.host != "" {
			h["Host"] = tr.host
		}
		if len(h) > 0 {
			t["headers"] = h
		}
		if tr.early > 0 {
			t["max_early_data"] = tr.early
			t["early_data_header_name"] = firstNonEmpty(tr.earlyH, "Sec-WebSocket-Protocol")
		}
		return t, tr, nil
	case "httpupgrade":
		t := map[string]interface{}{"type": "httpupgrade"}
		if tr.path != "" {
			t["path"] = tr.path
		}
		if tr.host != "" {
			t["host"] = tr.host
		}
		withHeaders(t)
		return t, tr, nil
	case "h2":
		t := map[string]interface{}{"type": "http"}
		if len(tr.hosts) > 0 {
			t["host"] = tr.hosts
		}
		if tr.path != "" {
			t["path"] = tr.path
		}
		return t, tr, nil
	case "http1":
		t := map[string]interface{}{"type": "http"}
		if tr.host != "" {
			t["host"] = []string{tr.host}
		}
		if tr.path != "" {
			t["path"] = tr.path
		}
		if tr.method != "" {
			t["method"] = tr.method
		}
		withHeaders(t)
		return t, tr, nil
	case "grpc":
		t := map[string]interface{}{"type": "grpc"}
		if tr.service != "" {
			t["service_name"] = tr.service
		}
		return t, tr, nil
	}
	return nil, tr, fmt.Errorf("传输方式 %s 暂不支持", tr.kind)
}

// muxOut Clash 的 smux → sing-box 的 multiplex。
func (c clashReader) muxOut() map[string]interface{} {
	s := c.sub("smux")
	if s == nil || s["enabled"] != true {
		return nil
	}
	m := map[string]interface{}{"enabled": true}
	if v, _ := s["protocol"].(string); v != "" {
		m["protocol"] = v
	}
	for from, to := range map[string]string{"max-connections": "max_connections", "min-streams": "min_streams", "max-streams": "max_streams"} {
		if n := toInt(s[from]); n > 0 {
			m[to] = n
		}
	}
	if s["padding"] == true {
		m["padding"] = true
	}
	if b, ok := s["brutal-opts"].(map[string]interface{}); ok && b["enabled"] == true {
		m["brutal"] = map[string]interface{}{"enabled": true, "up_mbps": mbps(b["up"]), "down_mbps": mbps(b["down"])}
	}
	return m
}

// hopping hysteria / hysteria2 的端口跳跃:Clash 的 ports "20000-30000,443" → sing-box 的 ["20000:30000","443:443"]。
func (c clashReader) hopping(o map[string]interface{}) error {
	ports := c.str("ports")
	if ports == "" {
		ports = c.str("mport")
	}
	if ports != "" {
		list, err := PortRanges(ports)
		if err != nil {
			return err
		}
		o["server_ports"] = list
	}
	if hi := c.p["hop-interval"]; hi != nil {
		switch v := hi.(type) {
		case string:
			if v != "" {
				if _, err := strconv.Atoi(v); err == nil {
					v += "s"
				}
				o["hop_interval"] = v
			}
		default:
			if n := toInt(v); n > 0 {
				o["hop_interval"] = strconv.Itoa(n) + "s"
			}
		}
	}
	return nil
}

// PortRanges 端口跳跃的写法 "20000-30000,443"(或 20000:30000)→ sing-box 的 server_ports(每段都要 "起:止",单个端口写成 "443:443")。
func PortRanges(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.ReplaceAll(part, "-", ":"))
		if part == "" {
			continue
		}
		lo, hi, ok := strings.Cut(part, ":")
		if !ok {
			hi = lo
		}
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		b, err2 := strconv.Atoi(strings.TrimSpace(hi))
		if err1 != nil || err2 != nil || a < 1 || b > 65535 || a > b {
			return nil, lossErr("ports", s)
		}
		out = append(out, strconv.Itoa(a)+":"+strconv.Itoa(b))
	}
	return out, nil
}

// mbps "100 Mbps" / "1 Gbps" / 100 → 100(Mbps)。认不出返回 0。
func mbps(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		i := 0
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
			i++
		}
		n, err := strconv.ParseFloat(s[:i], 64)
		if err != nil {
			return 0
		}
		switch unit := strings.TrimSpace(s[i:]); {
		case strings.HasPrefix(unit, "g"):
			n *= 1000
		case strings.HasPrefix(unit, "k"):
			n /= 1000
		}
		return int(n)
	}
	return 0
}
