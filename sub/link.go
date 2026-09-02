// Package sub 生成用户订阅:分享链接(link)与 clash 配置。
//
// 每个用户订阅列出其被分配的、已启用的线路,每条线路按"客户端侧代理"渲染:
// 客户端连接的是我们的入口服务器。线路的对外地址来自 Line.Addrs;为空时用入口主机 + 线路端口。
package sub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/render"
)

// Entry 是一个对外入口(如 香港/台湾):订阅里线路的连接地址与 SNI。
type Entry struct {
	Name     string  // 入口名(用于备注后缀,单入口时留空)
	Host     string  // 连接地址(IP 或域名)
	SNI      string  // TLS server_name
	Suffix   string  // 备注后缀,如 "-港 x2";单入口且倍率为 1 时留空
	Insecure bool    // 服务端为自签证书:客户端需允许不安全
	NodeId   uint    // 对应 nodes.id(0 = 不按服务器过滤线路)
	Ratio    float64 // 流量倍率(仅展示)
}

// addr 是一条线路的一个对外地址(展开 Line.Addrs 或回落到入口)。
type addr struct {
	server   string
	port     int
	sni      string
	remark   string
	insecure bool
}

// resolveAddrs 把线路的对外地址展开为一组 addr。
func resolveAddrs(line model.Line, entries []Entry) []addr {
	var custom []struct {
		Server     string `json:"server"`
		ServerPort int    `json:"server_port"`
		Remark     string `json:"remark"`
		SNI        string `json:"sni"`
	}
	if len(line.Addrs) > 0 {
		_ = json.Unmarshal(line.Addrs, &custom)
	}
	insecure := len(entries) > 0 && entries[0].Insecure
	if len(custom) > 0 {
		out := make([]addr, 0, len(custom))
		for _, c := range custom {
			port := c.ServerPort
			if port == 0 {
				port = line.Port
			}
			out = append(out, addr{server: c.Server, port: port, sni: c.SNI, remark: c.Remark, insecure: insecure})
		}
		return out
	}
	out := make([]addr, 0, len(entries))
	for _, e := range entries {
		if e.NodeId != 0 && !render.LineOnNode(line, e.NodeId) {
			continue // 该线路没部署在这台服务器上
		}
		out = append(out, addr{server: e.Host, port: line.Port, sni: e.SNI, remark: e.Suffix, insecure: e.Insecure})
	}
	return out
}

// userCred 取用户某协议的凭据字段。
func userCred(user model.User, protocol string) map[string]interface{} {
	var creds map[string]map[string]interface{}
	if len(user.Credentials) > 0 {
		_ = json.Unmarshal(user.Credentials, &creds)
	}
	if creds == nil {
		return map[string]interface{}{}
	}
	return creds[protocol]
}

// GenerateLinks 为一个用户在给定线路集合下生成分享链接(每条线路每个地址一到两行)。
func GenerateLinks(user model.User, lines []model.Line, entries []Entry) []string {
	var links []string
	for _, line := range lines {
		for _, a := range resolveAddrs(line, entries) {
			links = append(links, lineToURIs(line, user, a, line.Name+a.remark)...)
		}
	}
	return links
}

func lineToURIs(line model.Line, user model.User, a addr, remark string) []string {
	switch line.Protocol {
	case "hysteria2":
		return []string{hysteria2URI(line, user, a, remark)}
	case "anytls":
		return []string{anytlsURI(user, a, remark)}
	case "shadowsocks":
		return []string{shadowsocksURI(line, user, a, remark)}
	case "tuic":
		return []string{tuicURI(line, user, a, remark)}
	case "trojan":
		return []string{trojanURI(line, user, a, remark)}
	case "vless":
		return []string{vlessURI(line, user, a, remark)}
	case "vmess":
		return []string{vmessURI(line, user, a, remark)}
	case "socks":
		return []string{socksURI(user, a, remark)}
	case "http":
		return []string{httpURI(line, user, a, remark)}
	case "mixed":
		return []string{socksURI(user, a, remark), httpURI(line, user, a, remark)}
	}
	return nil
}

func hostPort(server string, port int) string {
	if strings.Contains(server, ":") { // IPv6
		return fmt.Sprintf("[%s]:%d", server, port)
	}
	return fmt.Sprintf("%s:%d", server, port)
}

// ---- 与 s-ui 字节级一致的三种既有格式(有真实数据黄金测试,勿改顺序)----

// hysteria2://<password>@host:port?security=tls&sni=<sni>&fastopen=<0|1>#remark
func hysteria2URI(line model.Line, user model.User, a addr, remark string) string {
	password, _ := userCred(user, "hysteria2")["password"].(string)
	q := "security=tls"
	if a.sni != "" {
		q += "&sni=" + url.QueryEscape(a.sni)
	}
	q += "&fastopen=" + boolParam(lineOption(line, "tcp_fast_open"))
	if a.insecure {
		q += "&insecure=1"
	}
	return withFragment(fmt.Sprintf("hysteria2://%s@%s?%s", password, hostPort(a.server, a.port), q), remark)
}

// anytls://<password>@host:port?security=tls&sni=<sni>#remark
func anytlsURI(user model.User, a addr, remark string) string {
	password, _ := userCred(user, "anytls")["password"].(string)
	q := "security=tls"
	if a.sni != "" {
		q += "&sni=" + url.QueryEscape(a.sni)
	}
	if a.insecure {
		q += "&insecure=1"
	}
	return withFragment(fmt.Sprintf("anytls://%s@%s?%s", password, hostPort(a.server, a.port), q), remark)
}

// ss://base64(method:password)@host:port#remark  (SIP002,备注原样,与 s-ui 一致)
func shadowsocksURI(line model.Line, user model.User, a addr, remark string) string {
	var opts map[string]interface{}
	_ = json.Unmarshal(line.Options, &opts)
	method, _ := opts["method"].(string)
	userinfo := base64.StdEncoding.EncodeToString([]byte(method + ":" + ssClientPassword(line, user)))
	return fmt.Sprintf("ss://%s@%s#%s", userinfo, hostPort(a.server, a.port), remark)
}

// ssClientPassword 客户端用的 shadowsocks 密码:2022 系列多用户为 "服务端PSK:用户PSK"(与 s-ui 一致),
// 其余算法直接用用户密码。
func ssClientPassword(line model.Line, user model.User) string {
	var opts map[string]interface{}
	_ = json.Unmarshal(line.Options, &opts)
	method, _ := opts["method"].(string)
	password, _ := userCred(user, ssCredKey(method))["password"].(string)
	if strings.HasPrefix(method, "2022") {
		if psk, _ := opts["password"].(string); psk != "" {
			return psk + ":" + password
		}
	}
	return password
}

// ---- 新增协议 ----

// tlsQuery 按线路 TLS 模式给出客户端 TLS 参数(通用 v2ray 风格键名)。
func tlsQuery(line model.Line, a addr) []string {
	c := render.ParseTLS(line)
	switch c.Mode {
	case "cert":
		q := []string{"security=tls"}
		if c.Fingerprint != "" {
			q = append(q, "fp="+url.QueryEscape(c.Fingerprint))
		}
		if a.sni != "" {
			q = append(q, "sni="+url.QueryEscape(a.sni))
		}
		if a.insecure {
			q = append(q, "allowInsecure=1")
		}
		return q
	case "reality":
		fp := c.Fingerprint
		if fp == "" {
			fp = "chrome"
		}
		sid := ""
		if len(c.Reality.ShortIDs) > 0 {
			sid = c.Reality.ShortIDs[0]
		}
		return []string{
			"security=reality",
			"pbk=" + url.QueryEscape(c.Reality.PublicKey),
			"sid=" + url.QueryEscape(sid),
			"fp=" + url.QueryEscape(fp),
			"sni=" + url.QueryEscape(c.Reality.HandshakeServer),
		}
	}
	return nil
}

// transportQuery 把线路传输配置转成 v2ray 风格参数(type/host/path/serviceName)。
func transportQuery(line model.Line) []string {
	tr := transportOf(line)
	typ, _ := tr["type"].(string)
	if typ == "" {
		typ = "tcp"
	}
	q := []string{"type=" + typ}
	switch typ {
	case "ws":
		if p, _ := tr["path"].(string); p != "" {
			q = append(q, "path="+url.QueryEscape(p))
		}
		if h := wsHost(tr); h != "" {
			q = append(q, "host="+url.QueryEscape(h))
		}
	case "grpc":
		if s, _ := tr["service_name"].(string); s != "" {
			q = append(q, "serviceName="+url.QueryEscape(s))
		}
	case "httpupgrade":
		if h, _ := tr["host"].(string); h != "" {
			q = append(q, "host="+url.QueryEscape(h))
		}
		if p, _ := tr["path"].(string); p != "" {
			q = append(q, "path="+url.QueryEscape(p))
		}
	case "http":
		if hosts := stringList(tr["host"]); len(hosts) > 0 {
			q = append(q, "host="+url.QueryEscape(strings.Join(hosts, ",")))
		}
		if p, _ := tr["path"].(string); p != "" {
			q = append(q, "path="+url.QueryEscape(p))
		}
	}
	return q
}

func transportOf(line model.Line) map[string]interface{} {
	tr := map[string]interface{}{}
	if len(line.Transport) > 0 {
		_ = json.Unmarshal(line.Transport, &tr)
	}
	return tr
}

func wsHost(tr map[string]interface{}) string {
	if headers, ok := tr["headers"].(map[string]interface{}); ok {
		switch h := headers["Host"].(type) {
		case string:
			return h
		case []interface{}:
			if len(h) > 0 {
				s, _ := h[0].(string)
				return s
			}
		}
	}
	h, _ := tr["host"].(string)
	return h
}

func stringList(v interface{}) []string {
	switch x := v.(type) {
	case string:
		if x != "" {
			return []string{x}
		}
	case []interface{}:
		out := make([]string, 0, len(x))
		for _, i := range x {
			if s, ok := i.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// vless://uuid@host:port?type=..&security=..&...[&flow=xtls-rprx-vision]#remark
func vlessURI(line model.Line, user model.User, a addr, remark string) string {
	id, _ := userCred(user, "vless")["uuid"].(string)
	q := append(transportQuery(line), tlsQuery(line, a)...)
	if render.VisionEnabled(line) {
		q = append(q, "flow=xtls-rprx-vision")
	}
	return withFragment(fmt.Sprintf("vless://%s@%s?%s", id, hostPort(a.server, a.port), strings.Join(q, "&")), remark)
}

// trojan://password@host:port?type=..&security=tls&sni=..#remark
func trojanURI(line model.Line, user model.User, a addr, remark string) string {
	password, _ := userCred(user, "trojan")["password"].(string)
	q := append(transportQuery(line), tlsQuery(line, a)...)
	return withFragment(fmt.Sprintf("trojan://%s@%s?%s", url.PathEscape(password), hostPort(a.server, a.port), strings.Join(q, "&")), remark)
}

// tuic://uuid:password@host:port?security=tls&sni=..&congestion_control=..&udp_relay_mode=native&alpn=h3#remark
func tuicURI(line model.Line, user model.User, a addr, remark string) string {
	c := userCred(user, "tuic")
	id, _ := c["uuid"].(string)
	password, _ := c["password"].(string)
	var opts map[string]interface{}
	_ = json.Unmarshal(line.Options, &opts)
	cc, _ := opts["congestion_control"].(string)
	if cc == "" {
		cc = "cubic"
	}
	q := append(tlsQuery(line, a), "congestion_control="+cc, "udp_relay_mode=native", "alpn=h3")
	if a.insecure {
		q = append(q, "allow_insecure=1")
	}
	return withFragment(fmt.Sprintf("tuic://%s:%s@%s?%s", id, url.PathEscape(password), hostPort(a.server, a.port), strings.Join(q, "&")), remark)
}

// vmess://base64({v:2, ps, add, port, id, aid, net, type, host, path, tls, sni, fp})
func vmessURI(line model.Line, user model.User, a addr, remark string) string {
	id, _ := userCred(user, "vmess")["uuid"].(string)
	tr := transportOf(line)
	typ, _ := tr["type"].(string)
	obj := map[string]interface{}{
		"v": "2", "ps": remark, "add": a.server, "port": fmt.Sprint(a.port),
		"id": id, "aid": 0, "scy": "auto", "net": "tcp", "type": "none", "host": "", "path": "", "tls": "none",
	}
	switch typ {
	case "ws", "httpupgrade":
		obj["net"] = typ
		obj["path"], _ = tr["path"].(string)
		obj["host"] = wsHost(tr)
	case "grpc":
		obj["net"] = "grpc"
		obj["path"], _ = tr["service_name"].(string)
	case "http":
		obj["net"] = "tcp"
		obj["type"] = "http"
		obj["host"] = strings.Join(stringList(tr["host"]), ",")
		obj["path"], _ = tr["path"].(string)
	}
	c := render.ParseTLS(line)
	if c.Mode == "cert" {
		obj["tls"] = "tls"
		obj["sni"] = a.sni
		if c.Fingerprint != "" {
			obj["fp"] = c.Fingerprint
		}
	}
	b, _ := json.Marshal(obj)
	return "vmess://" + base64.StdEncoding.EncodeToString(b)
}

// socks5://user:pass@host:port#remark
func socksURI(user model.User, a addr, remark string) string {
	c := userCred(user, "socks")
	u, _ := c["username"].(string)
	p, _ := c["password"].(string)
	return withFragment(fmt.Sprintf("socks5://%s:%s@%s", url.PathEscape(u), url.PathEscape(p), hostPort(a.server, a.port)), remark)
}

// http(s)://user:pass@host:port#remark
func httpURI(line model.Line, user model.User, a addr, remark string) string {
	c := userCred(user, "http")
	u, _ := c["username"].(string)
	p, _ := c["password"].(string)
	scheme := "http"
	if render.ParseTLS(line).Mode == "cert" {
		scheme = "https"
	}
	return withFragment(fmt.Sprintf("%s://%s:%s@%s", scheme, url.PathEscape(u), url.PathEscape(p), hostPort(a.server, a.port)), remark)
}

// ---- 工具 ----

// withFragment 用 Go 的 URL 片段编码追加备注(CJK 百分号编码、括号等子分隔符保留字面),
// 与 s-ui 的 addParams(通过 url.URL.String())完全一致。
func withFragment(base, remark string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base + "#" + remark
	}
	u.Fragment = remark
	return u.String()
}

// lineOption 读取线路 Options 中的一个布尔选项。
func lineOption(line model.Line, key string) bool {
	var opts map[string]interface{}
	if len(line.Options) > 0 {
		_ = json.Unmarshal(line.Options, &opts)
	}
	v, _ := opts[key].(bool)
	return v
}

func boolParam(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func ssCredKey(method string) string {
	if strings.HasPrefix(method, "2022-blake3-aes-128") {
		return "shadowsocks16"
	}
	return "shadowsocks"
}
