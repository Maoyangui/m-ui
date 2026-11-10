package ext

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// NodeInfo 面板里展开一条外部订阅时看到的一个节点:基本信息、该协议的全部字段、分享链接。
type NodeInfo struct {
	Index    int     `json:"index"` // 在解析结果里的下标(只在同一份列表内有效;跨刷新认节点用 Key)
	Key      string  `json:"key"`   // 身份指纹:隐藏、改名、测速与大陆连通的结果都按它对应节点
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Server   string  `json:"server"`
	Port     int     `json:"port"`
	Fields   []Field `json:"fields"`
	Link     string  `json:"link,omitempty"`
	Upstream bool    `json:"upstream"`          // 能否测速、作为本站上游:能无损转成 sing-box 出站
	Reason   string  `json:"reason,omitempty"`  // 不能的原因(协议、传输方式、插件不支持等)
	Hidden   bool    `json:"hidden,omitempty"`  // 不输出到用户订阅
	Keyword  bool    `json:"keyword,omitempty"` // 是关键词排除的(手动显示不了,要改关键词)
	Rename   string  `json:"rename,omitempty"`  // 改的名字;空 = 用原名
}

// Annotate 把筛选结果标到节点明细上,面板显示用。
func Annotate(infos []NodeInfo, f Filter) {
	r := Roster{Names: make([]string, len(infos)), Keys: make([]string, len(infos))}
	for i, n := range infos {
		r.Names[i], r.Keys[i] = n.Name, n.Key
	}
	for i, d := range f.Decide(r) {
		infos[i].Hidden, infos[i].Keyword, infos[i].Rename = d.Hidden, d.Keyword, d.Rename
	}
}

// Field 节点的一个参数,值统一成字符串给界面直接显示(嵌套结构用紧凑 JSON)。
type Field struct {
	Key   string `json:"k"`
	Value string `json:"v"`
}

// fieldOrder 常见字段的显示顺序;不在这里的按字母序排在后面。
var fieldOrder = []string{
	"name", "type", "server", "port", "ports",
	"uuid", "password", "username", "cipher", "flow",
	"sni", "servername", "alpn", "skip-cert-verify", "client-fingerprint", "reality-opts", "tls",
	"network", "ws-opts", "grpc-opts", "h2-opts", "http-opts",
	"plugin", "plugin-opts", "obfs", "obfs-password",
	"up", "down", "congestion-controller", "udp-relay-mode", "udp", "tfo",
}

// Details 把解析结果整理成面板要的节点明细。下标与 Items.Nodes 一致;只有分享链接、转不成 Clash 的节点也列出来
// (以前不列,面板上看不到也就没法隐藏,链接订阅里照常输出)。
func Details(it Items) []NodeInfo {
	out := make([]NodeInfo, 0, len(it.Nodes))
	for i, nd := range it.Nodes {
		n := NodeInfo{Index: i, Key: nd.Key, Name: nd.Name, Link: nd.Link}
		if p := nd.Clash; p != nil {
			n.Type, _ = p["type"].(string)
			n.Server, _ = p["server"].(string)
			n.Port = toInt(p["port"])
			n.Fields = fieldsOf(p)
		} else {
			n.Type, n.Server, n.Port = linkBasics(nd.Link)
			n.Fields = []Field{{Key: "name", Value: nd.Name}, {Key: "type", Value: n.Type}, {Key: "server", Value: n.Server}, {Key: "port", Value: strconv.Itoa(n.Port)}}
		}
		if _, _, err := Outbound(nd); err != nil {
			n.Reason = err.Error()
		} else {
			n.Upstream = true
		}
		out = append(out, n)
	}
	return out
}

// linkBasics 解析不了的链接也尽量给出协议、地址、端口。
func linkBasics(link string) (typ, server string, port int) {
	base, _, _ := strings.Cut(link, "#")
	if i := strings.Index(base, "://"); i > 0 {
		typ = strings.ToLower(base[:i])
	}
	if u, err := url.Parse(base); err == nil {
		server = u.Hostname()
		port, _ = strconv.Atoi(u.Port())
	}
	return
}

func toInt(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func fieldsOf(p map[string]interface{}) []Field {
	seen := map[string]bool{}
	var out []Field
	add := func(k string) {
		v, ok := p[k]
		if !ok || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, Field{Key: k, Value: fieldString(v)})
	}
	for _, k := range fieldOrder {
		add(k)
	}
	rest := make([]string, 0, len(p))
	for k := range p {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		add(k)
	}
	return out
}

func fieldString(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprint(v)
}
