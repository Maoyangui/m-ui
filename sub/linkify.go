package sub

import (
	"html/template"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 落地页公告与联系方式里的链接。
//
// 文案是管理员和代理填的纯文本,这里是落地页上**唯一**把它变成 HTML(template.HTML)的地方:
// 文字一律 HTML 转义,只有下面几种写法变成 <a>;地址只放行 http / https / tg / mailto / tel,
// 并且必须整段是可打印 ASCII、不含引号尖括号反斜杠反引号。不合格的写法原样显示成文字(不丢字,只是点不了)。
// 链接不带 class 和 style,字号、行高跟随所在的段落。
//
//	[文字](链接)              文字里有 / : 之类、或链接里有括号时用它
//	文字(链接)                括号半角全角都行,紧贴文字:文字从括号往前取到最近的分隔符(冒号、斜杠、逗号……)为止;
//	                          括号和地址都不显示
//	Tg / 电报 / 飞机: @用户名   → https://t.me/用户名(号码 +xx… → https://t.me/+号码);冒号半角全角都行
//	WhatsApp / WA / WS: +号码  → https://wa.me/号码
//	裸的 http(s) 地址          → 地址本身就是链接文字
//
// 每一行各自处理,换行原样保留。每条正则只跑一次 FindAll 再按位置合并,往回找链接文字也不越过上一个链接,
// 耗时随长度线性增长(恶意拼出来的几百 KB 也只要零点几秒)。

// safeHref 地址合格就原样返回,否则返回空串。
func safeHref(raw string) string {
	if raw == "" || len(raw) > 2048 {
		return ""
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c <= 0x20 || c >= 0x7f || strings.IndexByte("\"'<>\\`", c) >= 0 {
			return ""
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return ""
		}
	case "tg": // tg://resolve?domain=xxx
		if u.Host == "" && u.Opaque == "" {
			return ""
		}
	case "mailto", "tel":
		if u.Opaque == "" {
			return ""
		}
	default:
		return ""
	}
	return raw
}

var (
	// [文字](链接):链接里可以有一层成对的括号(维基百科那种 Foo_(bar) 地址)
	mdLinkRe = regexp.MustCompile(`\[([^\[\]\n]{1,100})\]\(((?:[^()\s]|\([^()\s]*\))+)\)`)
	// 文字(链接):半角、全角括号都认,地址必须以允许的协议开头(协议名不分大小写)。
	// 正则里的全角括号、冒号写成 \x{FF08} 这样的转义:中文输入法打出来的多半是全角的,和半角的看着几乎一样,写转义不会弄混
	parenLinkRe = regexp.MustCompile(`[(\x{FF08}]((?:[Hh][Tt][Tt][Pp][Ss]?|[Tt][Gg])://[^\s()\x{FF08}\x{FF09}<>"'` + "`" + `]+|(?:[Mm][Aa][Ii][Ll][Tt][Oo]|[Tt][Ee][Ll]):[^\s()\x{FF08}\x{FF09}<>"'` + "`" + `]+)[)\x{FF09}]`)
	// 关键词:Tg: @用户名、WhatsApp: +号码……冒号半角全角都认(链接只包住值本身,关键词和冒号仍是文字)
	keywordLinkRe = regexp.MustCompile(`(?i)(telegram|tg|whatsapp|wa|ws|电报|飞机)(群|频道|group|channel)?[ \t]*[:\x{FF1A}][ \t]*(@[A-Za-z][A-Za-z0-9_]{4,31}|\+\d{1,3}(?:[ \-]?\d{2,5}){1,4})`)
	// 裸的 http(s) 地址:只取可打印 ASCII,到空白、引号、尖括号、括号、反斜杠或第一个非 ASCII 字符为止
	// (中文里地址后面常常直接跟着「,」或别的字,不截开的话整段都不像地址,就一个链接也认不出来)
	bareLinkRe = regexp.MustCompile(`[Hh][Tt][Tt][Pp][Ss]?://[!#-&*-;=?-\[\]-_a-~]+`)
)

// linkDelims 「文字(链接)」往前找文字时在这些字符处停下:冒号、斜杠、竖线、间隔号、逗号、分号、顿号、句号、
// 叹号、问号、各种括号与引号(半角、全角都算)、制表符。空格不算:「联系 小王(链接)」整个「联系 小王」都是链接文字。
const linkDelims = ":：/|·,，;；、。!！?？[]【】「」<>\"“”'‘’()（）\t"

// linkTrailing 裸地址末尾的这些标点算句子的,不算地址的。
const linkTrailing = ".,;:!?。，；：！？、"

func isLinkDelim(r rune) bool { return strings.ContainsRune(linkDelims, r) }

// anchor 拼一个链接;http(s) 在新标签页打开,tg / mailto / tel 交给系统(不多开一个空白页)。
func anchor(text, href string) string {
	h := template.HTMLEscapeString(href)
	l := strings.ToLower(href)
	if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
		return `<a href="` + h + `" target="_blank" rel="noopener noreferrer nofollow">` + template.HTMLEscapeString(text) + `</a>`
	}
	return `<a href="` + h + `">` + template.HTMLEscapeString(text) + `</a>`
}

// linkify 把公告 / 联系方式转义成 HTML,并把上面几种写法变成链接。模板里用 {{linkify .Notice}}。
func linkify(s string) template.HTML {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		linkLine(&b, line)
	}
	return template.HTML(b.String())
}

type linkSpan struct {
	start, end int // 这一行里被替换掉的字节区间 [start,end)
	text, href string
}

// linkLine 先找显式写法([文字](链接)、文字(链接)),剩下的纯文本再找关键词与裸地址。
func linkLine(b *strings.Builder, line string) {
	var spans []linkSpan
	last := 0 // 上一个链接的末尾:后面的匹配不能和它重叠,往回找文字也不越过它
	md := mdLinkRe.FindAllStringSubmatchIndex(line, -1)
	pr := parenLinkRe.FindAllStringSubmatchIndex(line, -1)
	mi, pi := 0, 0
	for mi < len(md) || pi < len(pr) {
		if pi >= len(pr) || (mi < len(md) && md[mi][0] <= pr[pi][0]) {
			m := md[mi]
			mi++
			if m[0] < last {
				continue
			}
			href := safeHref(line[m[4]:m[5]])
			if href == "" {
				continue // 地址不合格:整段当文字,里面的裸地址后面照样能认出来
			}
			spans = append(spans, linkSpan{start: m[0], end: m[1], text: line[m[2]:m[3]], href: href})
			last = m[1]
			continue
		}
		m := pr[pi]
		pi++
		if m[0] < last {
			continue
		}
		href := safeHref(line[m[2]:m[3]])
		if href == "" {
			continue
		}
		// 括号前面紧贴着文字才是「文字(链接)」;顶格或前面是空白、分隔符的是括号里的裸地址,交给第二遍
		r, _ := utf8.DecodeLastRuneInString(line[last:m[0]])
		if m[0] == last || unicode.IsSpace(r) || isLinkDelim(r) {
			continue
		}
		st := m[0]
		for st > last {
			r, sz := utf8.DecodeLastRuneInString(line[last:st])
			if isLinkDelim(r) {
				break
			}
			st -= sz
		}
		for st < m[0] { // 去掉文字开头的空白(留在链接外面)
			r, sz := utf8.DecodeRuneInString(line[st:m[0]])
			if !unicode.IsSpace(r) {
				break
			}
			st += sz
		}
		spans = append(spans, linkSpan{start: st, end: m[1], text: line[st:m[0]], href: href})
		last = m[1]
	}
	pos := 0
	for _, sp := range spans {
		implicitLinks(b, line[pos:sp.start])
		b.WriteString(anchor(sp.text, sp.href))
		pos = sp.end
	}
	implicitLinks(b, line[pos:])
}

// implicitLinks 处理一段纯文本里的关键词与裸地址:两类各 FindAll 一次,按位置从左到右合并,重叠的取靠前的。
func implicitLinks(b *strings.Builder, seg string) {
	var cs []linkSpan
	for _, k := range keywordLinkRe.FindAllStringSubmatchIndex(seg, -1) {
		kw := strings.ToLower(seg[k[2]:k[3]])
		if kw[0] >= 'a' && kw[0] <= 'z' && k[2] > 0 {
			if r, _ := utf8.DecodeLastRuneInString(seg[:k[2]]); unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				continue // 拉丁字母的关键词粘在别的词后面(News: 里的 ws)不算
			}
		}
		val := seg[k[6]:k[7]]
		tg := kw == "telegram" || kw == "tg" || kw == "电报" || kw == "飞机"
		href := ""
		if strings.HasPrefix(val, "@") {
			if tg && !nextIsWordChar(seg[k[7]:]) { // WhatsApp 没有 @用户名 链接;用户名后面还连着字就是没取全,不猜
				href = "https://t.me/" + val[1:]
			}
		} else if d := digitsOnly(val); len(d) >= 7 && len(d) <= 15 && !nextIsDigit(seg[k[7]:]) {
			if tg {
				href = "https://t.me/+" + d
			} else {
				href = "https://wa.me/" + d
			}
		}
		if href != "" {
			cs = append(cs, linkSpan{start: k[6], end: k[7], text: val, href: href})
		}
	}
	for _, u := range bareLinkRe.FindAllStringIndex(seg, -1) {
		raw := strings.TrimRightFunc(seg[u[0]:u[1]], func(r rune) bool { return strings.ContainsRune(linkTrailing, r) })
		if h := safeHref(raw); h != "" {
			cs = append(cs, linkSpan{start: u[0], end: u[0] + len(raw), text: raw, href: h})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].start < cs[j].start })
	pos := 0
	for _, c := range cs {
		if c.start < pos {
			continue
		}
		b.WriteString(template.HTMLEscapeString(seg[pos:c.start]))
		b.WriteString(anchor(c.text, c.href))
		pos = c.end
	}
	b.WriteString(template.HTMLEscapeString(seg[pos:]))
}

func nextIsWordChar(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r == '_' || r == '@' || (r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)))
}

func nextIsDigit(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsDigit(r)
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
