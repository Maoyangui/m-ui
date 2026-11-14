package ext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/upstream"
)

// Node 外部内容里的一个节点。同一次解析同时给出它的名字、身份指纹、分享链接和 Clash 代理:
// 链接订阅、Clash 订阅、面板列表、隐藏与改名都按这同一份判断,不会出现 Clash 里隐藏了、链接订阅里还在的情况。
type Node struct {
	Name  string                 // 节点名:Clash 名;只有链接的取第一个 # 之后(vmess 取 ps),都没有就用服务器地址
	Key   string                 // 身份指纹(见 clashKey):不随改名、换顺序变化
	Link  string                 // 分享链接(链接订阅 / sing-box 订阅用);转不出来为空
	Clash map[string]interface{} // Clash 代理(Clash 订阅用);转不出来为 nil
	// SingBox 来源是 sing-box JSON 时的原始出站:测速与添加为上游直接用它,不经 Clash 绕一圈丢字段。
	SingBox map[string]interface{}
}

// Items 是一份外部内容解析后的节点列表(顺序同原文)。
type Items struct {
	Nodes []Node
}

// Links 输出到链接订阅的分享链接。
func (it Items) Links() []string {
	out := make([]string, 0, len(it.Nodes))
	for _, n := range it.Nodes {
		if n.Link != "" {
			out = append(out, n.Link)
		}
	}
	return out
}

// Clash 输出到 Clash 订阅的代理。
func (it Items) Clash() []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(it.Nodes))
	for _, n := range it.Nodes {
		if n.Clash != nil {
			out = append(out, n.Clash)
		}
	}
	return out
}

// renamed 换个名字的副本:Clash 复制一份再改(抓取缓存解析一次会用很多回,原数据不能动),链接按协议改名。指纹不变。
func (n Node) renamed(name string) Node {
	out := n
	out.Name = name
	if n.Link != "" {
		out.Link = RenameLink(n.Link, name)
	}
	if n.Clash != nil {
		q := make(map[string]interface{}, len(n.Clash))
		for k, v := range n.Clash {
			q[k] = v
		}
		q["name"] = name
		out.Clash = q
	}
	if n.SingBox != nil {
		q := make(map[string]interface{}, len(n.SingBox))
		for k, v := range n.SingBox {
			q[k] = v
		}
		q["tag"] = name
		out.SingBox = q
	}
	return out
}

// clashNode clash YAML / sing-box JSON 里的一个节点:Clash 是原件,链接由它转出,名字与 Clash 一致。
func clashNode(p map[string]interface{}) Node {
	name, _ := p["name"].(string)
	n := Node{Name: name, Key: clashKey(p), Clash: p}
	if link, ok := ClashToLink(p); ok {
		n.Link = link
	}
	return n
}

// linkNode 链接列表里的一行。名字和 upstream.ParseLink 认的一致:第一个 # 之后(百分号转义不合法的原样保留),
// vmess 取 base64 里的 ps(数字也认),都没有就用服务器地址。名字不参与解析:名字里有非法 % 转义的节点
// 以前整个解析失败,面板看不到、也就没法隐藏。
func linkNode(line string) Node {
	base, frag, _ := strings.Cut(line, "#")
	name := unescapeName(frag)
	if strings.HasPrefix(strings.ToLower(base), "vmess://") {
		if ps := vmessPS(base); ps != "" {
			name = ps
		}
	}
	isVmess := strings.HasPrefix(strings.ToLower(base), "vmess://")
	name = strings.ToValidUTF8(name, "\uFFFD") // "%FF" 这类解出来不是 UTF-8:面板、订阅里都要能原样传回来比对
	n := Node{Link: line}
	if p, err := upstream.ParseLink(base); err == nil {
		if name == "" {
			name = p.Name // 去掉 # 之后解析,这里就是服务器地址
		}
		if c, ok := OutboundToClash(p.Type, name, p.Options); ok {
			n.Clash = c
		}
	} else if name == "" && !isVmess { // 解不开的 vmess 没有地址可用(URL 的"主机"是那串 base64)
		name = hostOf(base)
	}
	n.Name = name
	if n.Clash != nil {
		n.Key = clashKey(n.Clash)
	} else {
		n.Key = hashKey("raw", strings.TrimSpace(base))
	}
	if frag != "" && !isVmess {
		if _, err := url.PathUnescape(frag); err != nil {
			// 名字里有不合法的 % 转义:输出时规整成合法的转义,sing-box 订阅等按 URL 解析的客户端才认得这条
			n.Link = base + "#" + url.PathEscape(name)
		}
	}
	return n
}

// unescapeName 链接 # 之后的名字:按百分号转义解开;有不合法的转义(如 "100% 可用")就只解开合法的部分。
func unescapeName(s string) string {
	if s == "" {
		return ""
	}
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			v, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			b = append(b, byte(v))
			i += 2
			continue
		}
		b = append(b, s[i])
	}
	return string(b)
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// vmessPS vmess 链接 base64 里的名字;字符串和数字都认(和 upstream.ParseLink 一致)。
func vmessPS(base string) string {
	s := strings.TrimSpace(base)
	if len(s) < len("vmess://") || !strings.EqualFold(s[:len("vmess://")], "vmess://") {
		return ""
	}
	dec, err := decodeBase64Loose(s[len("vmess://"):])
	if err != nil {
		return ""
	}
	var v map[string]interface{}
	if json.Unmarshal([]byte(dec), &v) != nil {
		return ""
	}
	switch x := v["ps"].(type) {
	case string:
		return x
	case float64:
		return strconv.Itoa(int(x))
	}
	return ""
}

// hostOf 解析不了的链接也尽量取出服务器地址,给没名字的节点当名字。
func hostOf(base string) string {
	if u, err := url.Parse(strings.TrimSpace(base)); err == nil {
		return u.Hostname()
	}
	return ""
}

// clashKey 节点的身份指纹:sha256(类型|服务器|端口|uuid 或密码|SNI|Host|路径|gRPC 服务名) 的前 16 位。
// 名字、顺序、备注类字段都不进指纹:信息节点的"剩余流量 xx GB"每次刷新都变、机场重新编号会换名字,指纹都不变。
// 同一入口按 SNI / Host / 路径分流的不同节点,指纹也不同。缺省值统一处理(没写 SNI 等于用服务器地址、路径 "/" 等于空),
// 同一个节点不管是从 Clash YAML 还是分享链接解析来的,指纹一致。
func clashKey(p map[string]interface{}) string {
	str := func(v interface{}) string {
		switch x := v.(type) {
		case string:
			return strings.TrimSpace(x)
		case int, int64, float64:
			return fmt.Sprint(x)
		}
		return ""
	}
	typ := normType(str(p["type"]))
	server := strings.ToLower(strings.Trim(str(p["server"]), "[]"))
	port := strconv.Itoa(toInt(p["port"]))
	cred := strings.ToLower(str(p["uuid"]))
	if cred == "" {
		cred = firstNonEmpty(str(p["password"]), str(p["auth-str"]), str(p["auth_str"]), str(p["auth"]), str(p["psk"]), str(p["token"]))
	}
	host, path, svc := transportKey(p)
	tlsOn := p["tls"] == true || p["reality-opts"] != nil || alwaysTLS[typ]
	sni := strings.ToLower(firstNonEmpty(str(p["sni"]), str(p["servername"]), str(p["peer"])))
	if !tlsOn || sni == server || (host != "" && sni == host) {
		sni = ""
	}
	if path == "/" {
		path = ""
	}
	return hashKey(typ, server, port, cred, sni, host, path, svc)
}

// alwaysTLS 这些协议本身就是 TLS / QUIC,不写 tls: true 也带 SNI。
var alwaysTLS = map[string]bool{"trojan": true, "hysteria2": true, "hysteria": true, "tuic": true, "anytls": true}

func normType(t string) string {
	switch t = strings.ToLower(t); t {
	case "hy2":
		return "hysteria2"
	case "shadowsocks":
		return "ss"
	case "socks", "socks5h":
		return "socks5"
	}
	return t
}

// transportKey 传输层里区分节点的三样:Host、路径、gRPC 服务名(ss 的插件参数里的 host / path 也算)。
func transportKey(p map[string]interface{}) (host, path, svc string) {
	sub := func(k string) map[string]interface{} { m, _ := p[k].(map[string]interface{}); return m }
	first := func(v interface{}) string {
		switch x := v.(type) {
		case string:
			return x
		case []interface{}:
			if len(x) > 0 {
				s, _ := x[0].(string)
				return s
			}
		case []string:
			if len(x) > 0 {
				return x[0]
			}
		}
		return ""
	}
	headerHost := func(m map[string]interface{}) string {
		if h, ok := m["headers"].(map[string]interface{}); ok {
			for k, v := range h {
				if strings.EqualFold(k, "Host") {
					return first(v)
				}
			}
		}
		return ""
	}
	switch strings.ToLower(fmt.Sprint(p["network"])) {
	case "ws":
		if wo := sub("ws-opts"); wo != nil {
			path, host = first(wo["path"]), headerHost(wo)
		}
	case "h2":
		if ho := sub("h2-opts"); ho != nil {
			path, host = first(ho["path"]), first(ho["host"])
		}
	case "http":
		if ho := sub("http-opts"); ho != nil {
			path, host = first(ho["path"]), headerHost(ho)
		}
	case "grpc":
		if g := sub("grpc-opts"); g != nil {
			svc = first(g["grpc-service-name"])
		}
	}
	if po := sub("plugin-opts"); po != nil && host == "" && path == "" {
		host, path = first(po["host"]), first(po["path"])
	}
	return strings.ToLower(strings.TrimSpace(host)), strings.TrimSpace(path), strings.TrimSpace(svc)
}

func hashKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:8])
}

// ContentSig 节点列表的内容指纹:名字、指纹、链接或任一参数变了它就变。
// 刷新后它没变就不重写缓存;面板展开时拿到的列表也按它判断是否作废(测速、添加为上游按它防错位)。
func ContentSig(it Items) string {
	h := sha256.New()
	for _, n := range it.Nodes {
		c, _ := json.Marshal(n.Clash)
		s, _ := json.Marshal(n.SingBox)
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\n", n.Name, n.Key, n.Link, c, s)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
