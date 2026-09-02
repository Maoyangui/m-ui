// Package upstream 解析代理分享链接为 m-ui 上游(类型 + sing-box 出站参数)。
//
// 支持机场/服务商常见的四种链接:tuic:// hysteria2:// (hy2://) ss:// socks5://,
// 输出形态与既有数据库中的上游完全一致,可直接落库。
package upstream

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Parsed 是一条链接解析出的上游。
type Parsed struct {
	Type    string                 `json:"type"`
	Name    string                 `json:"name"`
	Options map[string]interface{} `json:"options"`
}

// ParseLink 解析一条分享链接。
func ParseLink(raw string) (*Parsed, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("链接为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("链接格式错误: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "tuic":
		return parseTuic(u)
	case "hysteria2", "hy2":
		return parseHysteria2(u)
	case "anytls":
		return parseAnytls(u)
	case "ss":
		return parseShadowsocks(u)
	case "socks5", "socks", "socks5h":
		return parseSocks(u)
	case "vless":
		return parseVless(u)
	case "trojan":
		return parseTrojan(u)
	case "vmess":
		return parseVmess(raw)
	}
	return nil, fmt.Errorf("不支持的链接类型: %s://(支持 vless/vmess/trojan/tuic/hysteria2/anytls/ss/socks5)", u.Scheme)
}

// OptionsJSON 把 Options 编码为落库的 JSON。
func (p *Parsed) OptionsJSON() json.RawMessage {
	b, _ := json.Marshal(p.Options)
	return b
}

func hostPort(u *url.URL, defaultPort int) (string, int, error) {
	host := u.Hostname()
	if host == "" {
		return "", 0, errors.New("缺少服务器地址")
	}
	port := defaultPort
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("端口无效: %s", p)
		}
		port = n
	}
	return host, port, nil
}

func tagOf(u *url.URL, fallback string) string {
	if u.Fragment != "" {
		return u.Fragment
	}
	return fallback
}

func flag(q url.Values, keys ...string) bool {
	for _, k := range keys {
		switch strings.ToLower(q.Get(k)) {
		case "1", "true", "yes":
			return true
		}
	}
	return false
}

// tlsBlock 组装出站 TLS:sni 缺省用主机名,alpn 缺省 h3(QUIC 协议)。
func tlsBlock(q url.Values, host string, defaultALPN string) map[string]interface{} {
	tls := map[string]interface{}{"enabled": true}
	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("peer")
	}
	if sni == "" {
		sni = host
	}
	tls["server_name"] = sni
	if alpn := q.Get("alpn"); alpn != "" {
		tls["alpn"] = strings.Split(alpn, ",")
	} else if defaultALPN != "" {
		tls["alpn"] = []string{defaultALPN}
	}
	if flag(q, "insecure", "allowInsecure", "allow_insecure", "skip-cert-verify") {
		tls["insecure"] = true
	}
	return tls
}

// tuic://<uuid>:<password>@host:port?congestion_control=cubic&udp_relay_mode=native&sni=&alpn=h3#name
func parseTuic(u *url.URL) (*Parsed, error) {
	host, port, err := hostPort(u, 443)
	if err != nil {
		return nil, err
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, errors.New("tuic 链接缺少 uuid")
	}
	password, _ := u.User.Password()
	q := u.Query()
	cc := firstNonEmpty(q.Get("congestion_control"), q.Get("congestion-control"), "cubic")
	opts := map[string]interface{}{
		"server":             host,
		"server_port":        port,
		"uuid":               u.User.Username(),
		"password":           password,
		"congestion_control": cc,
		"tls":                tlsBlock(q, host, "h3"),
	}
	if mode := firstNonEmpty(q.Get("udp_relay_mode"), q.Get("udp-relay-mode")); mode != "" {
		opts["udp_relay_mode"] = mode
	}
	if flag(q, "zero_rtt_handshake", "reduce_rtt", "reduce-rtt") {
		opts["zero_rtt_handshake"] = true
	}
	if flag(q, "udp_over_stream", "udp-over-stream") {
		opts["udp_over_stream"] = true
	}
	return &Parsed{Type: "tuic", Name: tagOf(u, host), Options: opts}, nil
}

// hysteria2://<password>@host:port?sni=&insecure=1&obfs=salamander&obfs-password=&upmbps=&downmbps=#name
func parseHysteria2(u *url.URL) (*Parsed, error) {
	host, port, err := hostPort(u, 443)
	if err != nil {
		return nil, err
	}
	password := ""
	if u.User != nil {
		password = u.User.Username()
		if pw, ok := u.User.Password(); ok && pw != "" {
			// 部分客户端把 auth 写成 user:pass,合并回一个口令
			password = password + ":" + pw
		}
	}
	q := u.Query()
	opts := map[string]interface{}{
		"server":      host,
		"server_port": port,
		"password":    password,
		"tls":         tlsBlock(q, host, "h3"),
	}
	if v, err := strconv.Atoi(q.Get("upmbps")); err == nil && v > 0 {
		opts["up_mbps"] = v
	}
	if v, err := strconv.Atoi(q.Get("downmbps")); err == nil && v > 0 {
		opts["down_mbps"] = v
	}
	if q.Get("obfs") == "salamander" {
		opts["obfs"] = map[string]interface{}{
			"type":     "salamander",
			"password": firstNonEmpty(q.Get("obfs-password"), q.Get("obfs_password")),
		}
	}
	if mport := q.Get("mport"); mport != "" {
		opts["server_ports"] = strings.Split(strings.ReplaceAll(mport, "-", ":"), ",")
	}
	return &Parsed{Type: "hysteria2", Name: tagOf(u, host), Options: opts}, nil
}

// anytls://password@host:port?sni=..&insecure=1#name
func parseAnytls(u *url.URL) (*Parsed, error) {
	host, port, err := hostPort(u, 443)
	if err != nil {
		return nil, err
	}
	password := ""
	if u.User != nil {
		password = u.User.Username()
	}
	if password == "" {
		return nil, errors.New("anytls 链接缺少密码")
	}
	q := u.Query()
	opts := map[string]interface{}{
		"server":      host,
		"server_port": port,
		"password":    password,
		"tls":         tlsBlock(q, host, ""),
	}
	return &Parsed{Type: "anytls", Name: tagOf(u, host), Options: opts}, nil
}

// ss://base64(method:password)@host:port#name  或  ss://method:password@host:port#name
func parseShadowsocks(u *url.URL) (*Parsed, error) {
	var method, password string
	if u.User != nil {
		method = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			password = pw
		} else {
			// userinfo 整体是 base64(method:password)
			decoded, err := decodeBase64Loose(method)
			if err != nil {
				return nil, errors.New("ss 链接的 userinfo 既不是 method:password 也不是合法 base64")
			}
			parts := strings.SplitN(decoded, ":", 2)
			if len(parts) != 2 {
				return nil, errors.New("ss 链接缺少加密方式或密码")
			}
			method, password = parts[0], parts[1]
		}
	} else {
		// 老式:ss://base64(method:password@host:port)#name
		decoded, err := decodeBase64Loose(u.Host + u.Path)
		if err != nil {
			return nil, errors.New("ss 链接格式无法识别")
		}
		inner, err := url.Parse("ss://" + decoded)
		if err != nil || inner.User == nil {
			return nil, errors.New("ss 链接格式无法识别")
		}
		inner.Fragment = u.Fragment
		return parseShadowsocks(inner)
	}
	host, port, err := hostPort(u, 8388)
	if err != nil {
		return nil, err
	}
	if method == "" || password == "" {
		return nil, errors.New("ss 链接缺少加密方式或密码")
	}
	opts := map[string]interface{}{
		"server":      host,
		"server_port": port,
		"method":      method,
		"password":    password,
	}
	q := u.Query()
	if plugin := q.Get("plugin"); plugin != "" {
		parts := strings.SplitN(plugin, ";", 2)
		opts["plugin"] = parts[0]
		if len(parts) == 2 {
			opts["plugin_opts"] = parts[1]
		}
	}
	return &Parsed{Type: "shadowsocks", Name: tagOf(u, host), Options: opts}, nil
}

// socks5://[user:pass@]host:port#name
func parseSocks(u *url.URL) (*Parsed, error) {
	host, port, err := hostPort(u, 1080)
	if err != nil {
		return nil, err
	}
	opts := map[string]interface{}{
		"server":      host,
		"server_port": port,
		"version":     "5",
	}
	if u.User != nil && u.User.Username() != "" {
		opts["username"] = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			opts["password"] = pw
		}
	}
	return &Parsed{Type: "socks", Name: tagOf(u, host), Options: opts}, nil
}

// clientTLS 按 v2ray 风格参数组装出站 TLS(security=tls|reality;sni/alpn/fp/insecure/pbk/sid)。
// forceTLS 用于 trojan 这类缺省即 TLS 的协议。
func clientTLS(q url.Values, host string, forceTLS bool) map[string]interface{} {
	security := strings.ToLower(q.Get("security"))
	if security == "" && forceTLS {
		security = "tls"
	}
	if security != "tls" && security != "reality" {
		return nil
	}
	tls := map[string]interface{}{"enabled": true, "server_name": firstNonEmpty(q.Get("sni"), q.Get("peer"), host)}
	if alpn := q.Get("alpn"); alpn != "" {
		tls["alpn"] = strings.Split(alpn, ",")
	}
	if flag(q, "insecure", "allowInsecure", "allow_insecure") {
		tls["insecure"] = true
	}
	fp := q.Get("fp")
	if security == "reality" {
		if fp == "" {
			fp = "chrome"
		}
		tls["reality"] = map[string]interface{}{"enabled": true, "public_key": q.Get("pbk"), "short_id": q.Get("sid")}
	}
	if fp != "" {
		tls["utls"] = map[string]interface{}{"enabled": true, "fingerprint": fp}
	}
	return tls
}

// clientTransport 把 type/path/host/serviceName 参数转成 sing-box 出站 transport;TCP 返回 nil。
func clientTransport(q url.Values) map[string]interface{} {
	typ := strings.ToLower(q.Get("type"))
	host, path := q.Get("host"), q.Get("path")
	switch typ {
	case "ws":
		tr := map[string]interface{}{"type": "ws", "path": firstNonEmpty(path, "/")}
		if host != "" {
			tr["headers"] = map[string]interface{}{"Host": host}
		}
		return tr
	case "grpc":
		return map[string]interface{}{"type": "grpc", "service_name": q.Get("serviceName")}
	case "httpupgrade":
		tr := map[string]interface{}{"type": "httpupgrade", "path": firstNonEmpty(path, "/")}
		if host != "" {
			tr["host"] = host
		}
		return tr
	case "http", "h2":
		tr := map[string]interface{}{"type": "http", "path": firstNonEmpty(path, "/")}
		if host != "" {
			tr["host"] = strings.Split(host, ",")
		}
		return tr
	case "tcp", "":
		if strings.EqualFold(q.Get("headerType"), "http") {
			tr := map[string]interface{}{"type": "http", "path": firstNonEmpty(path, "/")}
			if host != "" {
				tr["host"] = strings.Split(host, ",")
			}
			return tr
		}
	}
	return nil
}

// vless://uuid@host:port?type=..&security=..&sni=..&pbk=..&sid=..&fp=..&flow=..#name
func parseVless(u *url.URL) (*Parsed, error) {
	host, port, err := hostPort(u, 443)
	if err != nil {
		return nil, err
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, errors.New("vless 链接缺少 uuid")
	}
	q := u.Query()
	opts := map[string]interface{}{"server": host, "server_port": port, "uuid": u.User.Username()}
	if flow := q.Get("flow"); flow != "" {
		opts["flow"] = flow
	}
	if tls := clientTLS(q, host, false); tls != nil {
		opts["tls"] = tls
	}
	if tr := clientTransport(q); tr != nil {
		opts["transport"] = tr
	}
	return &Parsed{Type: "vless", Name: tagOf(u, host), Options: opts}, nil
}

// trojan://password@host:port?type=..&security=tls&sni=..#name
func parseTrojan(u *url.URL) (*Parsed, error) {
	host, port, err := hostPort(u, 443)
	if err != nil {
		return nil, err
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, errors.New("trojan 链接缺少密码")
	}
	q := u.Query()
	opts := map[string]interface{}{"server": host, "server_port": port, "password": u.User.Username()}
	if tls := clientTLS(q, host, true); tls != nil {
		opts["tls"] = tls
	}
	if tr := clientTransport(q); tr != nil {
		opts["transport"] = tr
	}
	return &Parsed{Type: "trojan", Name: tagOf(u, host), Options: opts}, nil
}

// vmess://base64({v,ps,add,port,id,aid,net,type,host,path,tls,sni,alpn,fp})  (v2rayN 格式)
func parseVmess(raw string) (*Parsed, error) {
	payload := strings.TrimPrefix(strings.TrimSpace(raw), "vmess://")
	decoded, err := decodeBase64Loose(payload)
	if err != nil {
		return nil, errors.New("vmess 链接不是合法 base64")
	}
	var v map[string]interface{}
	if err := json.Unmarshal([]byte(decoded), &v); err != nil {
		return nil, errors.New("vmess 链接内容不是 JSON")
	}
	str := func(k string) string {
		switch x := v[k].(type) {
		case string:
			return x
		case float64:
			return strconv.Itoa(int(x))
		}
		return ""
	}
	host, id := str("add"), str("id")
	if host == "" || id == "" {
		return nil, errors.New("vmess 链接缺少 add/id")
	}
	port, err := strconv.Atoi(str("port"))
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("vmess 端口无效: %s", str("port"))
	}
	aid, _ := strconv.Atoi(firstNonEmpty(str("aid"), "0"))
	opts := map[string]interface{}{"server": host, "server_port": port, "uuid": id, "security": "auto", "alter_id": aid}

	q := url.Values{}
	q.Set("type", str("net"))
	q.Set("host", str("host"))
	q.Set("path", str("path"))
	if strings.EqualFold(str("type"), "http") {
		q.Set("headerType", "http")
	}
	if str("net") == "grpc" {
		q.Set("serviceName", str("path"))
	}
	if tr := clientTransport(q); tr != nil {
		opts["transport"] = tr
	}
	if str("tls") == "tls" {
		tq := url.Values{}
		tq.Set("security", "tls")
		tq.Set("sni", firstNonEmpty(str("sni"), str("host")))
		tq.Set("alpn", str("alpn"))
		tq.Set("fp", str("fp"))
		if _, ok := v["allowInsecure"]; ok {
			tq.Set("insecure", "1")
		}
		opts["tls"] = clientTLS(tq, host, false)
	}
	return &Parsed{Type: "vmess", Name: firstNonEmpty(str("ps"), host), Options: opts}, nil
}

// decodeBase64Loose 容忍 URL-safe / 无 padding 的 base64。
func decodeBase64Loose(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), nil
		}
	}
	return "", errors.New("not base64")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ServerAddr 从上游参数里取出 host:port(用于 TCP 探测)。
func ServerAddr(options json.RawMessage) (string, error) {
	var o struct {
		Server string `json:"server"`
		Port   int    `json:"server_port"`
	}
	if err := json.Unmarshal(options, &o); err != nil {
		return "", err
	}
	if o.Server == "" || o.Port == 0 {
		return "", errors.New("上游缺少 server/server_port")
	}
	return net.JoinHostPort(o.Server, strconv.Itoa(o.Port)), nil
}
