package ext

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/Maoyangui/m-ui/upstream"
)

// NodeInfo 面板里展开一条外部订阅时看到的一个节点:基本信息、该协议的全部字段、分享链接。
type NodeInfo struct {
	Index    int     `json:"index"` // 在解析结果里的下标,测速与添加为上游按它指
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Server   string  `json:"server"`
	Port     int     `json:"port"`
	Fields   []Field `json:"fields"`
	Link     string  `json:"link,omitempty"`
	Upstream bool    `json:"upstream"` // 能否作为本站上游:协议受支持、且能转成分享链接再解析回来
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

// Details 把解析结果整理成面板要的节点明细。下标与 Items.Clash 一致。
func Details(it Items) []NodeInfo {
	out := make([]NodeInfo, 0, len(it.Clash))
	for i, p := range it.Clash {
		n := NodeInfo{Index: i}
		n.Name, _ = p["name"].(string)
		n.Type, _ = p["type"].(string)
		n.Server, _ = p["server"].(string)
		n.Port = toInt(p["port"])
		n.Fields = fieldsOf(p)
		if link, ok := ClashToLink(p); ok {
			n.Link = link
			if _, err := upstream.ParseLink(link); err == nil {
				n.Upstream = true
			}
		}
		out = append(out, n)
	}
	return out
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
