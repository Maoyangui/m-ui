package sub

import (
	"bytes"
	"html"
	"html/template"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 期望输出里的链接:http(s) 在新标签页打开,tg / mailto / tel 交给系统。href 传转义好的。
func webLink(href, text string) string {
	return `<a href="` + href + `" target="_blank" rel="noopener noreferrer nofollow">` + text + `</a>`
}
func sysLink(href, text string) string { return `<a href="` + href + `">` + text + `</a>` }

// 全角标点写成转义,和半角的看着一样,免得测试里看走眼
const (
	fwL     = "（" // (
	fwR     = "）" // )
	fwColon = "：" // :
	fwComma = "，" // ,
	cnStop  = "。" // 。
)

func TestLinkifyVectors(t *testing.T) {
	cases := []struct{ name, in, want string }{
		// 需求原话:文字(链接),半角与全角括号、冒号
		{"需求原话", "Tg: @MaoyangSupport(https://t.me/MaoyangSupport) / Wa: +34 671143339(https://wa.me/34671143339)",
			"Tg: " + webLink("https://t.me/MaoyangSupport", "@MaoyangSupport") + " / Wa: " + webLink("https://wa.me/34671143339", "+34 671143339")},
		{"全角括号", "Tg: @MaoyangSupport" + fwL + "https://t.me/MaoyangSupport" + fwR,
			"Tg: " + webLink("https://t.me/MaoyangSupport", "@MaoyangSupport")},
		{"全角括号与冒号", "Tg" + fwColon + "@MaoyangSupport" + fwL + "https://t.me/MaoyangSupport" + fwR + " / Wa" + fwColon + "+34 671143339" + fwL + "https://wa.me/34671143339" + fwR,
			"Tg" + fwColon + webLink("https://t.me/MaoyangSupport", "@MaoyangSupport") + " / Wa" + fwColon + webLink("https://wa.me/34671143339", "+34 671143339")},
		{"中文链接文字", "客服" + fwColon + "小王" + fwL + "https://t.me/xw" + fwR, "客服" + fwColon + webLink("https://t.me/xw", "小王")},
		{"半角中文链接文字", "客服:小王(https://t.me/xw)", "客服:" + webLink("https://t.me/xw", "小王")},
		{"往回找到分隔符为止", "Clash/Mihomo 教程(https://example.com)", "Clash/" + webLink("https://example.com", "Mihomo 教程")},
		{"没有分隔符就到行首", "github-maoyangui(https://github.com/Maoyangui)", webLink("https://github.com/Maoyangui", "github-maoyangui")},
		{"冒号后的文字", "有问题请联系:客服(https://t.me/x)", "有问题请联系:" + webLink("https://t.me/x", "客服")},
		{"两个紧挨着", "A(https://a.com)B(https://b.com)", webLink("https://a.com", "A") + webLink("https://b.com", "B")},
		{"号码后面跟数字时用括号写法", "Wa: +34 671143339(https://wa.me/34671143339) 90 点后回复",
			"Wa: " + webLink("https://wa.me/34671143339", "+34 671143339") + " 90 点后回复"},

		// 关键词自动识别:只包住值,关键词和冒号仍是文字
		{"关键词", "Tg: @MaoyangSupport / Wa: +34 671143339",
			"Tg: " + webLink("https://t.me/MaoyangSupport", "@MaoyangSupport") + " / Wa: " + webLink("https://wa.me/34671143339", "+34 671143339")},
		{"飞机群", "飞机群:@abcde", "飞机群:" + webLink("https://t.me/abcde", "@abcde")},
		{"电报全角冒号", "电报" + fwColon + "@abcdef", "电报" + fwColon + webLink("https://t.me/abcdef", "@abcdef")},
		{"TG群", "TG群" + fwColon + "@my_group", "TG群" + fwColon + webLink("https://t.me/my_group", "@my_group")},
		{"WS 号码", "WS: +1 650-555-1234", "WS: " + webLink("https://wa.me/16505551234", "+1 650-555-1234")},
		{"WhatsApp 全角冒号", "WhatsApp" + fwColon + "+34 671143339", "WhatsApp" + fwColon + webLink("https://wa.me/34671143339", "+34 671143339")},
		{"Telegram 号码", "tg: +34 671143339", "tg: " + webLink("https://t.me/+34671143339", "+34 671143339")},
		{"号码后面隔一个字", "Wa: +34 671143339 9点后回复", "Wa: " + webLink("https://wa.me/34671143339", "+34 671143339") + " 9点后回复"},
		{"用户名后的句号不算", "tg: @abcde.", "tg: " + webLink("https://t.me/abcde", "@abcde") + "."},
		{"粘在词里的 ws 不算", "News: @channel", "News: @channel"},
		{"粘在词里的 tg 不算", "aTg: @abcdef", "aTg: @abcdef"},
		{"用户名太短", "TG: @abc", "TG: @abc"},
		{"WhatsApp 没有用户名链接", "wa: @someone", "wa: @someone"},
		{"号码太短", "wa: +12 3456", "wa: +12 3456"},
		{"用户名超长不猜", "tg: @" + strings.Repeat("a", 33), "tg: @" + strings.Repeat("a", 33)},

		// [文字](链接)
		{"Markdown 写法", "[Clash/Mihomo 教程](https://example.com/a?b=1&c=2)", webLink("https://example.com/a?b=1&amp;c=2", "Clash/Mihomo 教程")},
		{"Markdown 链接带括号", "[维基](https://en.wikipedia.org/wiki/Foo_(bar))", webLink("https://en.wikipedia.org/wiki/Foo_(bar)", "维基")},

		// mailto / tel / tg 交给系统
		{"mailto", "邮箱 a@b.com(mailto:a@b.com)", sysLink("mailto:a@b.com", "邮箱 a@b.com")},
		{"tel", "电话(tel:+34671143339)", sysLink("tel:+34671143339", "电话")},
		{"tg 协议", "打开 App(tg://resolve?domain=abc)", sysLink("tg://resolve?domain=abc", "打开 App")},
		{"协议大写", "看(HTTPS://EXAMPLE.COM)", webLink("HTTPS://EXAMPLE.COM", "看")},

		// 裸地址:去掉句末标点;中文紧跟在后面也能截开
		{"裸地址句号", "官网 https://example.com/x" + cnStop, "官网 " + webLink("https://example.com/x", "https://example.com/x") + cnStop},
		{"裸地址全角逗号", "官网" + fwColon + "https://example.com" + fwComma + "备用" + fwColon + "https://b.example.com" + cnStop,
			"官网" + fwColon + webLink("https://example.com", "https://example.com") + fwComma + "备用" + fwColon + webLink("https://b.example.com", "https://b.example.com") + cnStop},
		{"裸地址后面直接跟中文", "请访问https://example.com了解详情", "请访问" + webLink("https://example.com", "https://example.com") + "了解详情"},
		{"空格加括号里的裸地址", "价格 (https://example.com/p)", "价格 (" + webLink("https://example.com/p", "https://example.com/p") + ")"},
		{"顶格括号里的裸地址", "(https://example.com)", "(" + webLink("https://example.com", "https://example.com") + ")"},
		{"裸地址遇到括号截断", "https://example.com/wiki/Foo_(bar)", webLink("https://example.com/wiki/Foo_", "https://example.com/wiki/Foo_") + "(bar)"},

		// XSS:不合格的一律原样显示成文字,全部转义
		{"脚本标签", "<script>alert(1)</script>", "&lt;script&gt;alert(1)&lt;/script&gt;"},
		{"javascript 协议", "点我(javascript:alert(1))", "点我(javascript:alert(1))"},
		{"javascript 大小写混写", "点我(JaVaScRiPt:alert(1))", "点我(JaVaScRiPt:alert(1))"},
		{"vbscript", "点我(vbscript:msgbox(1))", "点我(vbscript:msgbox(1))"},
		{"Markdown javascript", "[x](javascript:alert(1))", "[x](javascript:alert(1))"},
		{"Markdown data", "[x](data:text/html;base64,PHNjcmlwdD4=)", "[x](data:text/html;base64,PHNjcmlwdD4=)"},
		{"引号逃逸", `点我(https://a.com/"onmouseover="alert(1))`,
			"点我(" + webLink("https://a.com/", "https://a.com/") + "&#34;onmouseover=&#34;alert(1))"},
		{"Markdown 引号逃逸", `[x](https://a.com" onclick="alert(1))`,
			"[x](" + webLink("https://a.com", "https://a.com") + "&#34; onclick=&#34;alert(1))"},
		{"单引号", "点我(https://a.com/'x)", "点我(" + webLink("https://a.com/", "https://a.com/") + "&#39;x)"},
		{"实体编码", "点我(&#106;avascript:alert(1))", "点我(&amp;#106;avascript:alert(1))"},
		{"没有主机", "点我(https:///evil)", "点我(https:///evil)"},
		{"不在白名单的协议", "看(ftp://example.com)", "看(ftp://example.com)"},
		{"标签当文字", "<b>粗</b>(https://a.com)", "&lt;b&gt;粗&lt;/b&gt;(" + webLink("https://a.com", "https://a.com") + ")"},
		{"双向控制字符进不了地址", "x(https://a.com/\u202eevil)", "x(" + webLink("https://a.com/", "https://a.com/") + "\u202eevil)"},
		{"控制字符进不了地址", "x(https://a.com/\x00b)", "x(" + webLink("https://a.com/", "https://a.com/") + "\ufffdb)"},
		{"空白进不了地址", "x(https://a.com/\tb)", "x(" + webLink("https://a.com/", "https://a.com/") + "\tb)"},

		// 换行原样保留(公告是 pre-wrap)
		{"多行", "第一行\n第二行 Tg: @abcdef", "第一行\n第二行 Tg: " + webLink("https://t.me/abcdef", "@abcdef")},
		{"CRLF", "a\r\nb", "a\nb"},
		{"单独的 CR", "a\rb", "a\nb"},
		{"空", "", ""},
	}
	for _, c := range cases {
		if got := string(linkify(c.in)); got != c.want {
			t.Errorf("%s\n  in:   %q\n  got:  %s\n  want: %s", c.name, c.in, got, c.want)
		}
	}
}

// 模板里 {{linkify .}} 输出原样插入,不会被 html/template 再转义一遍;别处的文字照常转义。
func TestLinkifyInTemplateNotDoubleEscaped(t *testing.T) {
	tm := template.Must(template.New("x").Funcs(template.FuncMap{"linkify": linkify}).Parse(
		`<div class="notice">{{linkify .}}</div><p class="foot">{{linkify .}} · {{.}}</p>`))
	var buf bytes.Buffer
	if err := tm.Execute(&buf, "Tg: @abcdef(https://t.me/abcdef) <i>"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, `<a href="https://t.me/abcdef"`) != 2 || !strings.Contains(out, "&lt;i&gt;") || strings.Contains(out, "&amp;lt;") || strings.Contains(out, "<i>") {
		t.Fatalf("模板输出不对:\n%s", out)
	}
	if !strings.Contains(out, "· Tg: @abcdef(https://t.me/abcdef) &lt;i&gt;") {
		t.Fatalf("不经 linkify 的地方应照常转义、不出链接:\n%s", out)
	}
}

// 链接不带 class / style:字号、颜色由所在段落决定(落地页只给 .notice a 之类换颜色)。
func TestLinkifyAnchorsCarryNoStyle(t *testing.T) {
	out := string(linkify("Tg: @abcdef / [x](https://a.com) / 电话(tel:+34671143339) / https://b.com"))
	if n := strings.Count(out, "<a "); n != 4 {
		t.Fatalf("应有 4 个链接,实际 %d: %s", n, out)
	}
	for _, bad := range []string{"style=", "class=", "font", "size"} {
		if strings.Contains(out, bad) {
			t.Fatalf("链接不该带 %q: %s", bad, out)
		}
	}
}

var anchorRe = regexp.MustCompile(`<a href="([^"]*)"( target="_blank" rel="noopener noreferrer nofollow")?>([^<]*)</a>`)

// 20 万条随机拼接的恶意片段:链接之外不能残留 < > " ',href 只能是白名单协议、只含可打印 ASCII、本身合格;
// 链接文字里同样没有未转义的尖括号引号。
func TestLinkifyPropertyNoInjection(t *testing.T) {
	t.Parallel()
	atoms := []string{"<", ">", "\"", "'", "&", "(", ")", fwL, fwR, "[", "]", " ", ":", fwColon, fwComma, cnStop, "/", "\n", "\r", "\t", "@", "+",
		"javascript:", "JaVaScRiPt:", "data:", "vbscript:", "https://", "http://", "HTTPS://", "tg://", "mailto:", "tel:", "ftp://", "a.com", "x", "1234567",
		"tg", "wa", "ws", "电报", "飞机", "群", "<script>", "onerror=", "&#106;", "\u202e", "\x00", "%3C", "`", "\\", "abcdef", "t.me/", "+34 671143339", "](", "文字"}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 200000; i++ {
		var b strings.Builder
		for n := 1 + r.Intn(12); n > 0; n-- {
			b.WriteString(atoms[r.Intn(len(atoms))])
		}
		in := b.String()
		out := string(linkify(in))
		rest := anchorRe.ReplaceAllStringFunc(out, func(m string) string {
			sm := anchorRe.FindStringSubmatch(m)
			href := html.UnescapeString(sm[1])
			l := strings.ToLower(href)
			if !(strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "tg:") || strings.HasPrefix(l, "mailto:") || strings.HasPrefix(l, "tel:")) {
				t.Fatalf("不在白名单的地址 %q,输入 %q", href, in)
			}
			if safeHref(href) != href {
				t.Fatalf("不合格的地址进了 href: %q,输入 %q", href, in)
			}
			if strings.ContainsAny(sm[3], `"'`) {
				t.Fatalf("链接文字里有未转义的引号: %q,输入 %q", sm[3], in)
			}
			return ""
		})
		if strings.ContainsAny(rest, `<>"'`) {
			t.Fatalf("链接之外残留了标记字符: %q -> %q", in, out)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("输出不是合法 UTF-8: %q", in)
		}
	}
}

// 恶意长输入耗时随长度线性增长:几百 KB 也是零点几秒(正则 RE2 线性,往回找文字不越过上一个链接)。
// 时长默认只做宽松检查(各包并行跑、开 -race 时会慢好几倍),M_UI_LOAD_TESTS=1 时按 1 秒严格检查。
func TestLinkifyHostileInputIsLinear(t *testing.T) {
	limit := 10 * time.Second
	if os.Getenv("M_UI_LOAD_TESTS") == "1" {
		limit = time.Second
	}
	for name, in := range map[string]string{
		"括号与半截地址": strings.Repeat("a(", 50000) + strings.Repeat("https://x.com/(", 20000) + strings.Repeat("tg: +1 2", 20000),
		"连续的显式链接": strings.Repeat("文字(https://a.com)", 20000),
		"没闭合的方括号": strings.Repeat("[a](", 60000) + strings.Repeat("]", 60000),
		"超长链接文字":  strings.Repeat("字", 150000) + "(https://a.com)",
		"关键词刷屏":   strings.Repeat("tg: @abcdef wa: +34 671143339 ", 15000),
	} {
		st := time.Now()
		out := linkify(in)
		d := time.Since(st)
		t.Logf("%s:%d 字节用了 %v", name, len(in), d)
		if d > limit {
			t.Fatalf("%s:%d 字节用了 %v,超过 %v", name, len(in), d, limit)
		}
		if len(out) == 0 {
			t.Fatalf("%s:输出为空", name)
		}
	}
}
