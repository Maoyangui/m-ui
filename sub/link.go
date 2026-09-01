// Package sub 生成用户订阅:分享链接(link)与 clash 配置。
//
// 每个用户订阅列出其被分配的、已启用的线路,每条线路按"客户端侧代理"渲染:
// 客户端连接的是我们的入口服务器(hy2/anytls 带 TLS+用户凭据,ss 用用户密码)。
// 线路的对外地址来自 Line.Addrs;为空时用入口主机 + 线路端口。
package sub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/fangjunsheng555/m-ui/database/model"
)

// Entry 是一个对外入口(如 香港/台湾):订阅里线路的连接地址与 SNI。
type Entry struct {
	Name   string // 入口名(用于备注后缀,单入口时留空)
	Host   string // 连接地址(IP 或域名)
	SNI    string // TLS server_name
	Suffix string // 备注后缀,如 "-港";单入口留空
}

// addr 是一条线路的一个对外地址(展开 Line.Addrs 或回落到入口)。
type addr struct {
	server string
	port   int
	sni    string
	remark string // 该地址的额外备注(叠加在线路名后)
}

// resolveAddrs 把线路的对外地址展开为一组 addr。
// Line.Addrs 为空 → 每个入口一条(常态:单入口即一条);非空 → 逐条使用其 server/port。
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
	if len(custom) > 0 {
		out := make([]addr, 0, len(custom))
		for _, c := range custom {
			port := c.ServerPort
			if port == 0 {
				port = line.Port
			}
			out = append(out, addr{server: c.Server, port: port, sni: c.SNI, remark: c.Remark})
		}
		return out
	}
	out := make([]addr, 0, len(entries))
	for _, e := range entries {
		out = append(out, addr{server: e.Host, port: line.Port, sni: e.SNI, remark: e.Suffix})
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

// GenerateLinks 为一个用户在给定线路集合下生成分享链接(每条线路每个地址一行)。
func GenerateLinks(user model.User, lines []model.Line, entries []Entry) []string {
	var links []string
	for _, line := range lines {
		for _, a := range resolveAddrs(line, entries) {
			remark := line.Name + a.remark
			if uri := lineToURI(line, user, a, remark); uri != "" {
				links = append(links, uri)
			}
		}
	}
	return links
}

func lineToURI(line model.Line, user model.User, a addr, remark string) string {
	switch line.Protocol {
	case "hysteria2":
		return hysteria2URI(line, user, a, remark)
	case "anytls":
		return anytlsURI(user, a, remark)
	case "shadowsocks":
		return shadowsocksURI(line, user, a, remark)
	}
	return ""
}

func hostPort(server string, port int) string {
	if strings.Contains(server, ":") { // IPv6
		return fmt.Sprintf("[%s]:%d", server, port)
	}
	return fmt.Sprintf("%s:%d", server, port)
}

// hysteria2://<password>@host:port?security=tls&sni=<sni>&fastopen=<0|1>#remark
// fastopen 取自线路的 tcp_fast_open 选项,与 s-ui 一致。
func hysteria2URI(line model.Line, user model.User, a addr, remark string) string {
	password, _ := userCred(user, "hysteria2")["password"].(string)
	q := "security=tls"
	if a.sni != "" {
		q += "&sni=" + url.QueryEscape(a.sni)
	}
	q += "&fastopen=" + boolParam(lineOption(line, "tcp_fast_open"))
	return withFragment(fmt.Sprintf("hysteria2://%s@%s?%s", password, hostPort(a.server, a.port), q), remark)
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

// anytls://<password>@host:port?security=tls&sni=<sni>#remark
func anytlsURI(user model.User, a addr, remark string) string {
	password, _ := userCred(user, "anytls")["password"].(string)
	q := "security=tls"
	if a.sni != "" {
		q += "&sni=" + url.QueryEscape(a.sni)
	}
	return withFragment(fmt.Sprintf("anytls://%s@%s?%s", password, hostPort(a.server, a.port), q), remark)
}

// ss://base64(method:password)@host:port#remark  (SIP002 无 padding,备注原样,与 s-ui 一致)
func shadowsocksURI(line model.Line, user model.User, a addr, remark string) string {
	var opts map[string]interface{}
	_ = json.Unmarshal(line.Options, &opts)
	method, _ := opts["method"].(string)
	// 非 2022 算法:用用户的 shadowsocks 密码;线路自身单密码不进链接(与 s-ui 一致)。
	password, _ := userCred(user, ssCredKey(method))["password"].(string)
	userinfo := base64.StdEncoding.EncodeToString([]byte(method + ":" + password))
	return fmt.Sprintf("ss://%s@%s#%s", userinfo, hostPort(a.server, a.port), remark)
}

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

func ssCredKey(method string) string {
	if strings.HasPrefix(method, "2022-blake3-aes-128") {
		return "shadowsocks16"
	}
	return "shadowsocks"
}
