package ext

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ClashToLink 把一条 clash / mihomo 代理转成通用分享链接,供链接订阅(小火箭 / nextin / sing-box 等)使用。
// 外部订阅常常只给 clash YAML,不转换的话这些节点在链接订阅里就会消失。
// 支持 ss / vmess / vless / trojan / hysteria2 / tuic / anytls / socks5 / http;其它类型返回 false。
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
	alpn := ""
	if list, ok := p["alpn"].([]interface{}); ok && len(list) > 0 {
		parts := make([]string, 0, len(list))
		for _, a := range list {
			parts = append(parts, fmt.Sprint(a))
		}
		alpn = strings.Join(parts, ",")
	}
	// 传输层(vless / vmess / trojan 共用)
	network := str("network")
	if network == "" {
		network = "tcp"
	}
	transport := func() {
		set("type", network)
		switch network {
		case "ws":
			if wo := sub("ws-opts"); wo != nil {
				if path, _ := wo["path"].(string); path != "" {
					set("path", path)
				}
				if h, ok := wo["headers"].(map[string]interface{}); ok {
					if host, _ := h["Host"].(string); host != "" {
						set("host", host)
					}
				}
			}
		case "grpc":
			if g := sub("grpc-opts"); g != nil {
				if s, _ := g["grpc-service-name"].(string); s != "" {
					set("serviceName", s)
				}
			}
		case "h2", "http":
			q.Set("type", "http")
			if h := sub("h2-opts"); h != nil {
				if path, _ := h["path"].(string); path != "" {
					set("path", path)
				}
			}
		}
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
		return "ss://" + userinfo + "@" + hostPort + frag, true
	case "vmess":
		id := str("uuid")
		if id == "" {
			return "", false
		}
		v := map[string]interface{}{
			"v": "2", "ps": name, "add": server, "port": port, "id": id, "aid": str("alterId"), "scy": firstNonEmpty(str("cipher"), "auto"),
			"net": network, "type": "none", "host": "", "path": "", "tls": "", "sni": "", "fp": str("client-fingerprint"), "alpn": alpn,
		}
		if v["aid"] == "" {
			v["aid"] = "0"
		}
		if network == "ws" {
			if wo := sub("ws-opts"); wo != nil {
				v["path"], _ = wo["path"].(string)
				if h, ok := wo["headers"].(map[string]interface{}); ok {
					v["host"], _ = h["Host"].(string)
				}
			}
		} else if network == "grpc" {
			if g := sub("grpc-opts"); g != nil {
				v["path"], _ = g["grpc-service-name"].(string)
			}
		}
		if boolOf("tls") {
			v["tls"] = "tls"
			v["sni"] = sni
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
		if !boolOf("tls") && sub("reality-opts") == nil {
			p["tls"] = true // trojan 一定走 TLS
		}
		q.Set("security", "tls")
		set("sni", sni)
		set("fp", str("client-fingerprint"))
		set("alpn", alpn)
		if insecure {
			q.Set("allowInsecure", "1")
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// portString 把 clash 里可能是数字或字符串的端口统一成字符串。
func portString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.Itoa(int(x))
	}
	return ""
}
