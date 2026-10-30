package ext

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
)

// Filter 外部节点输出到用户订阅前的筛选与改名。都按节点在外部订阅里的原名记:订阅刷新后节点的顺序、数量都可能变,原名才认得出。
type Filter struct {
	Hidden  map[string]bool   // 手动隐藏的节点
	Renames map[string]string // 原名 → 新名
	Exclude []string          // 名字含这些关键词的不输出(不分大小写):官网、到期、剩余流量这类信息节点
}

// ParseFilter 从 ExtNode 的三个字段组出筛选条件,解析不了的当没有。
func ParseFilter(hidden, renames json.RawMessage, exclude string) Filter {
	f := Filter{Hidden: map[string]bool{}, Renames: map[string]string{}}
	var list []string
	if len(hidden) > 0 && json.Unmarshal(hidden, &list) == nil {
		for _, n := range list {
			f.Hidden[n] = true
		}
	}
	if len(renames) > 0 {
		_ = json.Unmarshal(renames, &f.Renames)
	}
	for _, k := range strings.FieldsFunc(exclude, func(r rune) bool {
		return r == ',' || r == '，' || r == '|' || r == '、' || r == '\n' || r == ';' || r == '；'
	}) {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			f.Exclude = append(f.Exclude, k)
		}
	}
	return f
}

// Hides 这个节点要不要从订阅里拿掉;第二个返回值说明是不是关键词排除的(面板上显示用)。
func (f Filter) Hides(name string) (hidden, byKeyword bool) {
	if f.Hidden[name] {
		return true, false
	}
	low := strings.ToLower(name)
	for _, k := range f.Exclude {
		if strings.Contains(low, k) {
			return true, true
		}
	}
	return false, false
}

// Empty 没有任何筛选与改名。
func (f Filter) Empty() bool { return len(f.Hidden) == 0 && len(f.Renames) == 0 && len(f.Exclude) == 0 }

// Apply 按筛选条件处理节点:隐藏的拿掉、改名的换名字。Links 与 Clash 两份各自处理(分享链接转不了 Clash 的、Clash 转不了链接的都有)。
func (f Filter) Apply(it Items) Items {
	if f.Empty() {
		return it
	}
	out := Items{}
	for _, l := range it.Links {
		name := LinkName(l)
		if hide, _ := f.Hides(name); hide {
			continue
		}
		if to := f.Renames[name]; to != "" {
			l = RenameLink(l, to)
		}
		out.Links = append(out.Links, l)
	}
	for _, p := range it.Clash {
		name, _ := p["name"].(string)
		if hide, _ := f.Hides(name); hide {
			continue
		}
		q := p
		if to := f.Renames[name]; to != "" {
			q = make(map[string]interface{}, len(p))
			for k, v := range p {
				q[k] = v
			}
			q["name"] = to
		}
		out.Clash = append(out.Clash, q)
	}
	return out
}

// LinkName 分享链接里的节点名:vmess 在 base64 的 JSON 里,其余在 # 后面。
func LinkName(link string) string {
	if body, ok := strings.CutPrefix(link, "vmess://"); ok {
		var v struct {
			PS string `json:"ps"`
		}
		if b, err := decodeBase64Loose(body); err == nil {
			_ = json.Unmarshal([]byte(b), &v)
		}
		return v.PS
	}
	i := strings.LastIndex(link, "#")
	if i < 0 {
		return ""
	}
	name, err := url.PathUnescape(link[i+1:])
	if err != nil {
		return link[i+1:]
	}
	return name
}

// RenameLink 给分享链接换名字。
func RenameLink(link, name string) string {
	if body, ok := strings.CutPrefix(link, "vmess://"); ok {
		b, err := decodeBase64Loose(body)
		if err != nil {
			return link
		}
		var v map[string]interface{}
		if json.Unmarshal([]byte(b), &v) != nil {
			return link
		}
		v["ps"] = name
		enc, err := json.Marshal(v)
		if err != nil {
			return link
		}
		return "vmess://" + base64.StdEncoding.EncodeToString(enc)
	}
	if i := strings.LastIndex(link, "#"); i >= 0 {
		link = link[:i]
	}
	return link + "#" + url.PathEscape(name)
}
