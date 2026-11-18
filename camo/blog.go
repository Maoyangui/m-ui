package camo

// 个人博客(用户 11-19 精修):正文一栏居中;首页最上面是最新一篇(大图、标题、导语),下面是更早的文章;
// 文章页大题图、标签、上一篇;文末和首页底下有邮件订阅(经接口提交,api.go;没开脚本时提交到 /subscribe/ 那一页)。
// 题图有图包照片就用照片(media.go),没有画插图。

import (
	"fmt"
	"strings"
	"time"
)

var firstNames = []string{"Mara", "Theo", "Iris", "Noah", "Lena", "Owen", "Sana", "Felix", "Nora", "Elias", "Clara", "Jonas", "Ada", "Milo", "Ines", "Hugo"}
var lastNames = []string{"Lindqvist", "Hart", "Calder", "Brandt", "Okafor", "Price", "Ruiz", "Moreau", "Vance", "Holm", "Arden", "Keller", "Novak", "Sato", "Ferreira", "Lund"}

func genBlog(s *Site, g *gen) {
	s.name = g.pick(firstNames) + " " + g.pick(lastNames)
	first := strings.Fields(s.name)[0]
	s.apiPre = g.sk.api
	s.addScript(g, scriptParts{forms: true, formKey: g.sk.formKey})
	av := s.asset(g, "svg", svgType, avatar(g))
	s.logo = fmt.Sprintf(`<img class="%s %s" src="%s" alt="" width="26" height="26">`, g.c("logo"), g.c("av"), av)
	s.head = `<link rel="alternate" type="application/rss+xml" title="` + esc(s.name) + `" href="/feed.xml">`
	s.foot = `<span>Words and pictures by ` + esc(first) + ` · <a href="/feed.xml">RSS</a></span>`
	nav := [][2]string{{"/", "Writing"}, {"/about/", "About"}}
	topic := g.alt("{Photography|Film photography|Pictures}, {travel|slow travel|long walks} and {small software projects|things I fix myself|the occasional bit of code|old bikes}")
	s.desc = "Notes and pictures by " + s.name + ": " + strings.ToLower(topic[:1]) + topic[1:] + "."
	subTitle := g.pick([]string{"Get new posts by email", "Subscribe by email", "New posts in your inbox"})
	subNote := g.pick([]string{"One email when something new goes up. No spam, unsubscribe any time.", "A short note when I publish something new, about twice a month.", "Only new posts, nothing else. Unsubscribe with one click."})
	subForm := subscribePage(g, s, nav, "subscribe", "Subscribe", subNote)
	s.subscribeAPI(g, s.apiPre+"subscribe", subForm)
	box := subscribeBox(g, s, "subscribe", subTitle, subNote)

	type post struct {
		title, slug, date, lead, kind, hero, thumb string
		tags                                       []string
		at                                         time.Time
	}
	var posts []post
	day := g.n(2, 15)
	for i, ti := range g.r.Perm(len(blogTopics))[:8] {
		tp := blogTopics[ti]
		title := capFirst(fmt.Sprintf(g.pick(blogTitlePatterns), tp.text))
		at := g.base.Add(-time.Duration(day) * 24 * time.Hour)
		p := post{title: title, slug: slugify(title), date: at.Format("Jan 2, 2006"), lead: blogLead(g, tp.text), kind: tp.kind, tags: tp.tags, at: at}
		big, small := picSize{w: 960, h: 480}, picSize{w: 360, h: 240, pw: 120, ph: 80}
		if i == 0 { // 首页最上面那篇用大图
			big.pw, big.ph, small.pw, small.ph = 240, 120, 0, 0
		}
		pics := s.picture(g, i, tp.kind, "", big, small)
		p.hero, p.thumb = pics[0], pics[1]
		posts = append(posts, p)
		day += g.n(9, 45)
	}
	var list, feed strings.Builder
	featured := ""
	for i, p := range posts {
		var body strings.Builder
		fmt.Fprintf(&body, `<p>%s</p>`, esc(p.lead))
		for _, para := range blogParagraphs(g, g.n(2, 4)) {
			fmt.Fprintf(&body, `<p>%s</p>`, esc(para))
		}
		var tags strings.Builder
		for _, tg := range p.tags {
			fmt.Fprintf(&tags, `<span class="%s">#%s</span>`, g.c("chip"), tg)
		}
		nav2 := ""
		if i+1 < len(posts) {
			nav2 = fmt.Sprintf(` · Older: <a href="/posts/%s/">%s</a>`, posts[i+1].slug, esc(posts[i+1].title))
		}
		mins := g.n(3, 9)
		if i > 0 {
			fmt.Fprintf(&list, `<div class="%s"><a class="%s" href="/posts/%s/"><img src="%s" alt="" width="360" height="240" loading="lazy"></a><div><a href="/posts/%s/" style="font-weight:600;font-size:17px">%s</a><div class="%s" style="font-size:13px">%s · %d min read</div><p style="margin:6px 0 0;font-size:14px">%s</p></div></div>`,
				g.c("post"), g.c("pimg"), p.slug, p.thumb, p.slug, esc(p.title), g.c("muted"), p.date, mins, esc(p.lead))
		}
		pp := "/posts/" + p.slug + "/"
		s.add(pp, htmlType, layout(g, s, pp, p.title+" - "+s.name, nav,
			fmt.Sprintf(`<article class="%s"><h1>%s</h1><div class="%s"><img src="%s" alt=""><span>%s · %s · %d min read</span></div><div class="%s"><img src="%s" alt="" width="960" height="480"></div>%s<div class="%s">%s</div>%s<p><a href="/">← All writing</a>%s</p></article>`,
				g.c("col"), esc(p.title), g.c("byline"), av, esc(s.name), p.date, mins, g.c("hero"), p.hero, body.String(), g.c("tags"), tags.String(), box, nav2)))
		fmt.Fprintf(&feed, "<item><title>%s</title><link>/posts/%s/</link><guid isPermaLink=\"false\">%s</guid><pubDate>%s</pubDate><description>%s</description></item>",
			esc(p.title), p.slug, p.slug, p.at.Add(9*time.Hour).Format(time.RFC1123Z), esc(p.lead))
		if i == 0 {
			featured = fmt.Sprintf(`<a class="%s" href="/posts/%s/"><div class="%s"><img src="%s" alt="" width="960" height="480"></div><span class="%s">Latest</span><h2>%s</h2><div class="%s" style="font-size:13px">%s · %d min read</div><p>%s</p></a>`,
				g.c("top1"), p.slug, g.c("pimg"), p.hero, g.c("kick"), esc(p.title), g.c("muted"), p.date, mins, esc(p.lead))
		}
	}
	s.add("/feed.xml", "application/rss+xml; charset=utf-8", []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>%s</title><link>/</link><description>%s</description>%s</channel></rss>`,
		esc(s.name), esc(topic), feed.String())))
	s.add("/", htmlType, layout(g, s, "/", s.name, nav,
		fmt.Sprintf(`<div class="%s"><div class="%s"><img src="%s" alt="" width="64" height="64"><div><h1>%s</h1><p class="%s" style="margin:4px 0 0">%s</p></div></div>%s<h2>More writing</h2>%s%s</div>`,
			g.c("col"), g.c("author"), av, esc(s.name), g.c("sub"), esc(topic), featured, list.String(), box)))
	aboutText := g.alt("This site is where I keep {notes and pictures|notes, photos and the odd how-to} from things I am working on. It is a personal site with no ads and no tracking; if you would rather read it in a feed reader, there is an ")
	s.add("/about/", htmlType, layout(g, s, "/about/", "About - "+s.name, nav,
		fmt.Sprintf(`<div class="%s"><img class="%s" src="%s" alt="" width="96" height="96" style="width:96px;height:96px;border-radius:50%%;margin-bottom:14px"><h1>About</h1><p>Hi, I'm %s. %s.</p><p>%s<a href="/feed.xml">RSS feed</a>, or you can <a href="/subscribe/">get new posts by email</a>.</p></div>`,
			g.c("col"), g.c("av"), av, esc(first), esc(topic), esc(aboutText))))
	s.setNotFound(layout(g, s, "", "Not found - "+s.name, nav, notFoundBody(g)))
}

// subscribePage 「订阅」这一页(/subscribe/;没开脚本时表单提交回到这一页,带上提示)。返回往里填提示的函数。
func subscribePage(g *gen, s *Site, nav [][2]string, api, title, note string) func(string, bool) []byte {
	page := layout(g, s, "/subscribe/", title+" - "+s.name, nav, fmt.Sprintf(
		`<div class="%s"><h1>%s</h1><p class="%s">%s</p><form method="post" action="%s" data-%s="%s" novalidate>%s<div class="%s"><input name="email" type="email" autocomplete="email" placeholder="you@example.com" aria-label="Email" required><button type="submit">Subscribe</button></div></form></div>`,
		g.c("narrow"), esc(title), g.c("sub"), esc(note), s.apiPre+api, g.sk.formKey, api, alertSlot, g.c("srow")))
	s.add("/subscribe/", htmlType, []byte(strings.Replace(string(page), alertSlot, emptyAlert(g), 1)))
	return s.formPage(g, page)
}
