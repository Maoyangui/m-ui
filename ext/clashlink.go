package ext

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ClashToLink 把一条 clash / mihomo 代理转成通用分享链接,供链接订阅(小火箭 / nextin / sing-box 等)使用。
// 外部订阅常常只给 clash YAML,不转换的话这些节点在链接订阅里就会消失。
// 支持 ss / vmess / vless / trojan / hysteria2 / tuic / anytls / socks5 / http;其它类型返回 false。
// 能写进链接的参数都要写全(ss 插件、跳过证书校验、httpupgrade、h2 的 Host):漏一个,链接订阅里的这个节点就连不上。
func ClashToLink(p map[string]interface{}) (string, bool) {
	str := func(keys ...string) string {
		for _, k := range keys {
			switch v := p[k].(type) {
			case string:
				if v != "" {
					return v
				}
			case int, int64, float64:
				return fmt.Sprint(v)
			}
		}
		return ""
	}
	boolOf := func(k string) bool { b, _ := p[k].(bool); return b }
	sub := func(k string) map[string]interface{} { m, _ := p[k].(map[string]interface{}); return m }
	name := str("name")
	server, port := str("server"), str("port")
	if server == "" || port == "" {
		return "", false
	}
	hostPort := net.JoinHostPort(server, port)
	frag := "#" + url.PathEscape(name)
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	sni := str("sni", "servername")
	insecure := boolOf("skip-cert-verify")
	alpn := strings.Join(stringList(p["alpn"]), ",")
	// 传输层(vless / vmess / trojan 共用)
	network := str("network")
	if network == "" {
		network = "tcp"
	}
	tr := transportOfClash(p, network)
	transport := func() {
		switch tr.kind {
		case "http1": // Clash 的 network: http 是 TCP 上套 HTTP 伪装头
			q.Set("type", "tcp")
			q.Set("headerType", "http")
		case "h2":
			q.Set("type", "http")
		default:
			set("type", tr.kind)
		}
		set("path", tr.path)
		set("host", tr.host)
		set("serviceName", tr.service)
	}
	tlsParams := func() {
		if re := sub("reality-opts"); re != nil {
			q.Set("security", "reality")
			set("pbk", fmt.Sprint(re["public-key"]))
			if sid := fmt.Sprint(re["short-id"]); sid != "" && sid != "<nil>" {
				set("sid", sid)
			}
			set("sni", sni)
			set("fp", firstNonEmpty(str("client-fingerprint"), "chrome"))
			return
		}
		if boolOf("tls") {
			q.Set("security", "tls")
			set("sni", sni)
			set("fp", str("client-fingerprint"))
			set("alpn", alpn)
			if insecure {
				q.Set("allowInsecure", "1")
			}
		}
	}

	switch str("type") {
	case "ss":
		cipher, pw := str("cipher"), str("password")
		if cipher == "" || pw == "" {
			return "", false
		}
		userinfo := base64.RawURLEncoding.EncodeToString([]byte(cipher + ":" + pw))
		link := "ss://" + userinfo + "@" + hostPort
		if str("plugin") != "" {
			plugin, opts, _ := clashPluginSIP003(p) // 有损(v2ray-plugin 的跳过证书校验)也照样给链接:客户端多半还能用
			if plugin == "" {
				return "", false // 链接写不出的插件(shadow-tls 等):宁可不给,也别给一条缺插件、连不上的
			}
			if opts != "" {
				plugin += ";" + opts
			}
			link += "/?" + url.Values{"plugin": {plugin}}.Encode() // SIP002
		}
		return link + frag, true
	case "vmess":
		id := str("uuid")
		if id == "" {
			return "", false
		}
		v := map[string]interface{}{
			"v": "2", "ps": name, "add": server, "port": port, "id": id, "aid": str("alterId"), "scy": firstNonEmpty(str("cipher"), "auto"),
			"net": network, "type": "none", "host": tr.host, "path": tr.path, "tls": "", "sni": "", "fp": str("client-fingerprint"), "alpn": alpn,
		}
		if v["aid"] == "" {
			v["aid"] = "0"
		}
		switch tr.kind {
		case "grpc":
			v["path"] = tr.service
		case "http1":
			v["net"], v["type"] = "tcp", "http"
		case "httpupgrade", "h2":
			v["net"] = tr.kind
		}
		if boolOf("tls") {
			v["tls"] = "tls"
			v["sni"] = sni
			if insecure {
				v["allowInsecure"] = true // upstream.ParseLink 与 v2rayN 系客户端都认这个键
			}
		}
		b, _ := json.Marshal(v)
		return "vmess://" + base64.StdEncoding.EncodeToString(b), true
	case "vless":
		id := str("uuid")
		if id == "" {
			return "", false
		}
		q.Set("encryption", "none")
		set("flow", str("flow"))
		tlsParams()
		transport()
		return "vless://" + id + "@" + hostPort + "?" + q.Encode() + frag, true
	case "trojan":
		pw := str("password")
		if pw == "" {
			return "", false
		}
		if re := sub("reality-opts"); re != nil {
			tlsParams()
		} else {
			q.Set("security", "tls")
			set("sni", sni)
			set("fp", str("client-fingerprint"))
			set("alpn", alpn)
			if insecure {
				q.Set("allowInsecure", "1")
			}
		}
		transport()
		return "trojan://" + url.PathEscape(pw) + "@" + hostPort + "?" + q.Encode() + frag, true
	case "hysteria2", "hy2":
		pw := str("password", "auth")
		if pw == "" {
			return "", false
		}
		set("sni", sni)
		if insecure {
			q.Set("insecure", "1")
		}
		if obfs := str("obfs"); obfs != "" {
			set("obfs", obfs)
			set("obfs-password", str("obfs-password"))
		}
		set("alpn", alpn)
		set("mport", str("ports"))
		return "hysteria2://" + url.PathEscape(pw) + "@" + hostPort + "?" + q.Encode() + frag, true
	case "tuic":
		id, pw := str("uuid"), str("password")
		if id == "" {
			return "", false
		}
		set("sni", sni)
		q.Set("congestion_control", firstNonEmpty(str("congestion-controller"), "cubic"))
		q.Set("udp_relay_mode", firstNonEmpty(str("udp-relay-mode"), "native"))
		q.Set("alpn", firstNonEmpty(alpn, "h3"))
		if insecure {
			q.Set("allow_insecure", "1")
		}
		return "tuic://" + id + ":" + url.PathEscape(pw) + "@" + hostPort + "?" + q.Encode() + frag, true
	case "anytls":
		pw := str("password")
		if pw == "" {
			return "", false
		}
		set("sni", sni)
		if insecure {
			q.Set("insecure", "1")
		}
		return "anytls://" + url.PathEscape(pw) + "@" + hostPort + "?" + q.Encode() + frag, true
	case "socks5":
		user, pw := str("username"), str("password")
		auth := ""
		if user != "" {
			auth = base64.StdEncoding.EncodeToString([]byte(user+":"+pw)) + "@"
		}
		return "socks://" + auth + hostPort + frag, true
	case "http":
		user, pw := str("username"), str("password")
		auth := ""
		if user != "" {
			auth = url.PathEscape(user) + ":" + url.PathEscape(pw) + "@"
		}
		scheme := "http"
		if boolOf("tls") {
			scheme = "https"
		}
		return scheme + "://" + auth + hostPort + frag, true
	}
	return "", false
}

// clashTransport Clash 代理的传输层,整理成分享链接与 sing-box 出站都用得上的形状。
type clashTransport struct {
	kind    string // tcp | ws | httpupgrade | h2 | http1 | grpc | 其它原样
	path    string
	host    string // ws / httpupgrade / http1 的 Host 头;h2 的 host 列表用逗号连起来
	hosts   []string
	headers map[string]interface{} // ws / httpupgrade / http1 的其余请求头
	service string                 // gRPC 服务名
	method  string                 // http1 的请求方法
	early   int                    // ws 的 max-early-data
	earlyH  string                 // ws 的 early-data-header-name
}

func transportOfClash(p map[string]interface{}, network string) clashTransport {
	sub := func(k string) map[string]interface{} { m, _ := p[k].(map[string]interface{}); return m }
	splitHeaders := func(h map[string]interface{}) (string, map[string]interface{}) {
		host := ""
		rest := map[string]interface{}{}
		for k, v := range h {
			if strings.EqualFold(k, "Host") {
				if l := stringList(v); len(l) > 0 {
					host = l[0]
				}
				continue
			}
			rest[k] = v
		}
		return host, rest
	}
	tr := clashTransport{kind: network}
	switch network {
	case "ws":
		wo := sub("ws-opts")
		if wo == nil {
			break
		}
		if b, _ := wo["v2ray-http-upgrade"].(bool); b {
			tr.kind = "httpupgrade"
		}
		tr.path, _ = wo["path"].(string)
		if h, ok := wo["headers"].(map[string]interface{}); ok {
			tr.host, tr.headers = splitHeaders(h)
		}
		tr.early = toInt(wo["max-early-data"])
		tr.earlyH, _ = wo["early-data-header-name"].(string)
	case "h2":
		if ho := sub("h2-opts"); ho != nil {
			tr.path, _ = ho["path"].(string)
			tr.hosts = stringList(ho["host"])
			tr.host = strings.Join(tr.hosts, ",")
		}
	case "http":
		tr.kind = "http1"
		if ho := sub("http-opts"); ho != nil {
			if l := stringList(ho["path"]); len(l) > 0 {
				tr.path = l[0]
			}
			tr.method, _ = ho["method"].(string)
			if h, ok := ho["headers"].(map[string]interface{}); ok {
				tr.host, tr.headers = splitHeaders(h)
			}
		}
	case "grpc":
		if g := sub("grpc-opts"); g != nil {
			tr.service, _ = g["grpc-service-name"].(string)
		}
	}
	return tr
}

// stringList 字符串或字符串列表(YAML 里两种写法都有)。
func stringList(v interface{}) []string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		return strings.Split(x, ",")
	case []string:
		return x
	case []interface{}:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s := fmt.Sprint(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// clashPluginSIP003 把 Clash 的 ss 插件转成 SIP003 形式(插件名 + "k=v;k=v" 参数),分享链接与 sing-box 出站都用这一套。
// 认不出的插件 plugin 返回空。loss 非空表示能转、但有参数转不过去(结果可能连不上)。
func clashPluginSIP003(p map[string]interface{}) (plugin, opts string, loss error) {
	name, _ := p["plugin"].(string)
	po, _ := p["plugin-opts"].(map[string]interface{})
	get := func(k string) string {
		if po == nil {
			return ""
		}
		switch v := po[k].(type) {
		case string:
			return v
		case bool, int, int64, float64:
			return fmt.Sprint(v)
		}
		return ""
	}
	flag := func(k string) bool { return po != nil && po[k] == true }
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+sip003Escape(v))
		}
	}
	switch name {
	case "obfs", "obfs-local", "simple-obfs":
		add("obfs", firstNonEmpty(get("mode"), "http"))
		add("obfs-host", get("host"))
		return "obfs-local", strings.Join(parts, ";"), nil
	case "v2ray-plugin":
		mode := firstNonEmpty(get("mode"), "websocket")
		add("mode", mode)
		if flag("tls") {
			parts = append(parts, "tls")
		}
		add("host", get("host"))
		add("path", get("path"))
		if flag("mux") {
			parts = append(parts, "mux=1")
		} else {
			parts = append(parts, "mux=0")
		}
		if flag("tls") && flag("skip-cert-verify") {
			loss = fmt.Errorf("参数 %s=%v 暂不支持", "plugin-opts.skip-cert-verify", true)
		}
		return "v2ray-plugin", strings.Join(parts, ";"), loss
	}
	return "", "", fmt.Errorf("ss 插件 %s 暂不支持", name)
}

// pluginToClash 反过来:sing-box / SIP003 的插件(名字 + 参数串)转成 Clash 的 plugin 与 plugin-opts。
func pluginToClash(plugin, opts string) (string, map[string]interface{}) {
	kv := parseSIP003(opts)
	switch plugin {
	case "obfs-local", "simple-obfs":
		m := map[string]interface{}{"mode": firstNonEmpty(kv["obfs"], "http")}
		if h := kv["obfs-host"]; h != "" {
			m["host"] = h
		}
		return "obfs", m
	case "v2ray-plugin":
		m := map[string]interface{}{"mode": firstNonEmpty(kv["mode"], "websocket")}
		if _, ok := kv["tls"]; ok {
			m["tls"] = true
		}
		if h := kv["host"]; h != "" {
			m["host"] = h
		}
		if p := kv["path"]; p != "" {
			m["path"] = p
		}
		if mux, ok := kv["mux"]; !ok || (mux != "" && mux != "0") {
			m["mux"] = true // sing-box / v2ray-plugin 不写 mux 时默认开
		}
		return "v2ray-plugin", m
	}
	if len(kv) == 0 {
		return plugin, nil
	}
	m := make(map[string]interface{}, len(kv))
	for k, v := range kv {
		m[k] = v
	}
	return plugin, m
}

// parseSIP003 解析 "k=v;flag;k2=v2"(反斜杠转义 ; = \)。没有值的开关(如 tls)值为空串。
func parseSIP003(s string) map[string]string {
	out := map[string]string{}
	var key, val strings.Builder
	inVal := false
	flush := func() {
		if k := key.String(); k != "" {
			out[k] = val.String()
		}
		key.Reset()
		val.Reset()
		inVal = false
	}
	for i := 0; i < len(s); i++ {
		cur := &key
		if inVal {
			cur = &val
		}
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case c == ';':
			flush()
		case c == '=' && !inVal:
			inVal = true
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

func sip003Escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, "=", `\=`).Replace(v)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
