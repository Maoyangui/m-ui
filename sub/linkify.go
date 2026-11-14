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
// 文字一律 HTML 转义,只有下面几种写法变成 <a>;地址(关键词拼出来的也一样)一律过 safeHref:只放行
// http / https / tg / mailto / tel,并且必须整段是可打印 ASCII、不含引号尖括号反斜杠反引号。
// 不合格的写法原样显示成文字(不丢字,只是点不了)。链接不带 class 和 style,字号、行高跟随所在的段落。
//
//	[文字](链接)              方括号认 [ ] ［ ］【 】,圆括号半角全角都行;文字里有 / : 之类、或链接里有括号时用它
//	文字(链接)                括号半角全角都行,括号前可以空格;括号和地址都不显示。文字从括号往前取,到最近的分隔符
//	                          (冒号、斜杠、逗号……)、上一个链接、或前面自动识别出的地址 / @用户名 / +号码为止。
//	                          括号前紧挨着的是关键词的值(Tg: @用户名(链接))时,值本身就是链接文字;
//	                          紧挨着的是裸地址、或前面没有文字(顶格、紧跟分隔符)时,括号里的地址照原样显示成链接
//	Tg / 电报 / 飞机: @用户名   → https://t.me/用户名(号码 +xx… → https://t.me/+号码);冒号半角全角都行
//	WhatsApp / WA / WS: +号码  → https://wa.me/号码。关键词只认 ASCII 字母;前面紧贴汉字照认,
//	                          紧贴拉丁字母或数字不认(News: 里的 ws)
//	裸的 http(s) 地址          → 地址本身就是链接文字(句末标点、没配对的 ] } 不算地址)
//
// 每一行各自处理,换行原样保留。每条正则只跑一次 FindAll 再按位置合并,往回找链接文字不越过上一个括号,
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

// 正则里的全角符号写成 \x{FF08} 这样的转义:中文输入法打出来的多半是全角的,和半角的看着几乎一样,写转义不会弄混。
var (
	// [文字](链接):方括号认半角 [ ]、全角 ［ ］和【 】,圆括号认半角与全角;链接里可以有一层成对的半角括号
	// (维基百科那种 Foo_(bar) 地址)。地址里不能有全角括号,免得「【教程】（链接）后面）」一直吃到后面那个括号
	mdLinkRe = regexp.MustCompile(`[\[\x{FF3B}\x{3010}]([^\[\]\x{FF3B}\x{FF3D}\x{3010}\x{3011}\n]{1,100})[\]\x{FF3D}\x{3011}][(\x{FF08}]((?:[^()\x{FF08}\x{FF09}\s]|\([^()\x{FF08}\x{FF09}\s]*\))+)[)\x{FF09}]`)
	// 文字(链接):半角、全角括号都认,地址必须以允许的协议开头(协议名不分大小写)
	parenLinkRe = regexp.MustCompile(`[(\x{FF08}]((?:[Hh][Tt][Tt][Pp][Ss]?|[Tt][Gg])://[^\s()\x{FF08}\x{FF09}<>"'` + "`" + `]+|(?:[Mm][Aa][Ii][Ll][Tt][Oo]|[Tt][Ee][Ll]):[^\s()\x{FF08}\x{FF09}<>"'` + "`" + `]+)[)\x{FF09}]`)
	// 关键词:Tg: @用户名、WhatsApp: +号码……冒号半角全角都认(链接只包住值本身,关键词和冒号仍是文字)。
	// 大小写逐个字母写成 [Tt] 这样,不用 (?i):不区分大小写时 RE2 按 Unicode 折叠,会把 ſ(U+017F)、K(开尔文符号)
	// 也当成 s、k,用户名里混进非 ASCII 字符
	keywordLinkRe = regexp.MustCompile(`([Tt][Ee][Ll][Ee][Gg][Rr][Aa][Mm]|[Tt][Gg]|[Ww][Hh][Aa][Tt][Ss][Aa][Pp][Pp]|[Ww][Aa]|[Ww][Ss]|电报|飞机)(群|频道|[Gg][Rr][Oo][Uu][Pp]|[Cc][Hh][Aa][Nn][Nn][Ee][Ll])?[ \t]*[:\x{FF1A}][ \t]*(@[A-Za-z][A-Za-z0-9_]{4,31}|\+[0-9]{1,3}(?:[ \-]?[0-9]{2,5}){1,4})`)
	// 裸的 http(s) 地址:只取可打印 ASCII,到空白、引号、尖括号、括号、反斜杠或第一个非 ASCII 字符为止
	// (中文里地址后面常常直接跟着「,」或别的字,不截开的话整段都不像地址,就一个链接也认不出来)
	bareLinkRe = regexp.MustCompile(`[Hh][Tt][Tt][Pp][Ss]?://[!#-&*-;=?-\[\]-_a-~]+`)
)

// linkDelims 「文字(链接)」往前找文字时在这些字符处停下:冒号、斜杠、竖线、间隔号、逗号、分号、顿号、句号、
// 叹号、问号、各种括号与引号(半角、全角都算)、制表符。空格不算:「联系 小王(链接)」整个「联系 小王」都是链接文字。
const linkDelims = ":：/|·,，;；、。!！?？[]［］【】「」<>\"“”'‘’()（）\t"

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
	bare       bool // 裸地址(关键词的值不算)
}

// linkLine 先找显式写法([文字](链接)、文字(链接)),剩下的纯文本再找关键词与裸地址。
func linkLine(b *strings.Builder, line string) {
	var spans []linkSpan
	last := 0 // 上一个链接的末尾:后面的匹配不能和它重叠,往回找文字也不越过它
	md := mdLinkRe.FindAllStringSubmatchIndex(line, -1)
	pr := parenLinkRe.FindAllStringSubmatchIndex(line, -1)
	// 整行的关键词与裸地址:第二遍才输出,这里只给「文字(链接)」往回找文字定界(用到才算,只算一次)
	var imp []linkSpan
	impReady, ii, prev := false, 0, -1
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
		// 链接文字到哪为止:括号前的空白不算(「价格 (链接)」也只显示「价格」),它们和括号、地址一起藏起来
		te := m[0]
		for te > last {
			r, sz := utf8.DecodeLastRuneInString(line[last:te])
			if r == '\t' || !unicode.IsSpace(r) {
				break
			}
			te -= sz
		}
		// 前面没有文字(顶格、紧跟在上一个链接或分隔符后面):是括号里的裸地址,交给第二遍
		if te == last {
			continue
		}
		if r, _ := utf8.DecodeLastRuneInString(line[last:te]); isLinkDelim(r) {
			continue
		}
		if !impReady {
			imp, impReady = implicitSpans(line), true
		}
		for ii < len(imp) && imp[ii].end <= te { // 关键词的值与裸地址里,在文字末尾之前结束的最后一个
			prev, ii = ii, ii+1
		}
		floor := last
		if prev >= 0 && imp[prev].start >= last {
			p := imp[prev]
			if p.end == te {
				if p.bare {
					// 括号前紧挨着裸地址:那个地址自己是链接,括号里的也照原样显示成链接 —— 看得见的地址不会链到别处
					continue
				}
				// 括号前紧挨着关键词的值(需求原话 Tg: @用户名(链接)):值本身就是链接文字,地址用括号里写的
				spans = append(spans, linkSpan{start: p.start, end: m[1], text: line[p.start:te], href: href})
				last = m[1]
				continue
			}
			// 往回找文字不越过前面的地址、@用户名、+号码(「Tg: @abc 官网(链接)」):它们留给第二遍各自成链接
			floor = p.end
		}
		st := te
		for st > floor {
			r, sz := utf8.DecodeLastRuneInString(line[floor:st])
			if isLinkDelim(r) {
				break
			}
			st -= sz
		}
		// 文字开头的空白留在链接外面;往回一直走到了前面那个地址 / 号码的末尾时,中间的句点也留在外面
		// (「官网 https://a.com. 客服(链接)」里的「. 」不算链接文字)
		lead := unicode.IsSpace
		if st == floor && floor > last {
			lead = func(r rune) bool { return r == '.' || unicode.IsSpace(r) }
		}
		for st < te {
			r, sz := utf8.DecodeRuneInString(line[st:te])
			if !lead(r) {
				break
			}
			st += sz
		}
		if st == te {
			continue
		}
		spans = append(spans, linkSpan{start: st, end: m[1], text: line[st:te], href: href})
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

// implicitLinks 输出一段纯文本:关键词的值与裸地址变成链接,其余转义。
func implicitLinks(b *strings.Builder, seg string) {
	pos := 0
	for _, c := range implicitSpans(seg) {
		b.WriteString(template.HTMLEscapeString(seg[pos:c.start]))
		b.WriteString(anchor(c.text, c.href))
		pos = c.end
	}
	b.WriteString(template.HTMLEscapeString(seg[pos:]))
}

// implicitSpans 一段纯文本里的关键词与裸地址:两类各 FindAll 一次,按位置从左到右合并,重叠的取靠前的。
func implicitSpans(seg string) []linkSpan {
	var cs []linkSpan
	for _, k := range keywordLinkRe.FindAllStringSubmatchIndex(seg, -1) {
		kw := strings.ToLower(seg[k[2]:k[3]])
		if kw[0] >= 'a' && kw[0] <= 'z' && k[2] > 0 {
			// 拉丁字母的关键词粘在拉丁字母、数字后面(News: 里的 ws)不算;紧贴汉字(客服TG:)照常认
			if r, _ := utf8.DecodeLastRuneInString(seg[:k[2]]); r == '_' || (r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r))) {
				continue
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
		if href = safeHref(href); href != "" {
			cs = append(cs, linkSpan{start: k[6], end: k[7], text: val, href: href})
		}
	}
	for _, u := range bareLinkRe.FindAllStringIndex(seg, -1) {
		raw := trimLinkTail(seg[u[0]:u[1]])
		if h := safeHref(raw); h != "" {
			cs = append(cs, linkSpan{start: u[0], end: u[0] + len(raw), text: raw, href: h, bare: true})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].start < cs[j].start })
	out := cs[:0]
	pos := 0
	for _, c := range cs {
		if c.start < pos {
			continue
		}
		out = append(out, c)
		pos = c.end
	}
	return out
}

// trimLinkTail 裸地址末尾不算地址的部分:句末标点,以及地址被方括号、花括号包住时末尾没配对的 ] }
// (「备用地址[https://b.example.com]」;IPv6 的 [::1] 这种配了对的不动)。括号只数一次,耗时随长度线性。
func trimLinkTail(s string) string {
	isTrail := func(r rune) bool { return strings.ContainsRune(linkTrailing, r) }
	s = strings.TrimRightFunc(s, isTrail)
	sq := strings.Count(s, "]") - strings.Count(s, "[") // 多出来的 ]
	cu := strings.Count(s, "}") - strings.Count(s, "{")
	for {
		switch {
		case sq > 0 && strings.HasSuffix(s, "]"):
			sq--
		case cu > 0 && strings.HasSuffix(s, "}"):
			cu--
		default:
			return s
		}
		s = strings.TrimRightFunc(s[:len(s)-1], isTrail) // 句末标点里没有括号,数过的个数不变
	}
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
