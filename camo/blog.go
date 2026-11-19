package camo

// 个人博客(用户 11-19 精修):正文一栏居中;首页最上面是最新一篇(大图、标题、导语),下面是更早的文章;
// 文章页大题图、标签、上一篇;文末和首页底下有邮件订阅(经接口提交,api.go;没开脚本时提交回订阅那一页)。
// 题图有图包照片就用照片(media.go),没有画插图。
//
// 导航、页脚、「更早的文章」「阅读时间」这些固定的字,文章和 RSS、订阅页的地址都按种子换(审查 F-H1)。

import (
	"fmt"
	"strings"
	"time"
)

var firstNames = []string{"Mara", "Theo", "Iris", "Noah", "Lena", "Owen", "Sana", "Felix", "Nora", "Elias", "Clara", "Jonas", "Ada", "Milo", "Ines", "Hugo",
	"Anna", "Ben", "Carla", "David", "Elena", "Finn", "Greta", "Henrik", "Isla", "Jakob", "Kira", "Leo", "Maya", "Nils", "Olga", "Pablo",
	"Rosa", "Simon", "Tessa", "Viktor", "Wren", "Yara", "Zoe", "Emil"}
var lastNames = []string{"Lindqvist", "Hart", "Calder", "Brandt", "Okafor", "Price", "Ruiz", "Moreau", "Vance", "Holm", "Arden", "Keller", "Novak", "Sato", "Ferreira", "Lund",
	"Ahlberg", "Bianchi", "Castell", "Dahl", "Ellery", "Fontaine", "Graf", "Halvorsen", "Ivers", "Jansen", "Kowalski", "Larkin", "Marchetti", "Nyberg", "Oduya", "Petrov",
	"Quist", "Rahal", "Solberg", "Tanaka", "Ueda", "Varga", "Weller", "Zimmer"}

func genBlog(s *Site, g *gen) {
	s.name = g.pick(firstNames) + " " + g.pick(lastNames)
	first := strings.Fields(s.name)[0]
	s.apiPre = g.sk.api
	s.addScript(g, scriptParts{forms: true, formKey: g.sk.formKey})
	av := s.asset(g, "svg", svgType, avatar(g))
	feed, posts, dot := g.sk.feed, g.sk.posts, g.sk.dot
	s.logo = g.img(g.c("logo")+" "+g.c("av"), av, 0, 0, false)
	s.head = `<link rel="alternate" type="application/rss+xml" title="` + esc(s.name) + `" href="` + feed + `">`
	rss := g.say("RSS", "RSS", "Feed", "RSS feed", "Subscribe via RSS", "XML")
	s.foot = `<span>` + esc(fill(g.say("Words and pictures by %s", "Written by %s", "Made by %s", "Thanks for reading", "%s's corner of the web", "Written and photographed by %s",
		"A personal site by %s", "Thanks for stopping by"), first)) + dot + `<a href="` + feed + `">` + esc(rss) + `</a></span>`
	homeNav, aboutNav := g.say("Writing", "Posts", "Blog", "Journal", "Notes", "Archive", "Home", "Articles"), g.say("About", "About", "About me", "Hello", "Bio", "Info")
	nav := [][2]string{{"/", homeNav}, {"/about/", aboutNav}}
	topic := g.alt("{Photography|Film photography|Pictures|Photos|Notes}, {travel|slow travel|long walks|hiking|cycling} and {small software projects|things I fix myself|the occasional bit of code|old bikes|a home server|whatever I am tinkering with}")
	s.desc = fill(g.say("Notes and pictures by %[1]s: %[2]s.", "%[1]s writes about %[2]s.", "The personal site of %[1]s. %[3]s.", "%[1]s's blog: %[2]s.", "%[3]s. A blog by %[1]s.", "Personal notes from %[1]s on %[2]s."),
		s.name, strings.ToLower(topic[:1])+topic[1:], topic)
	st := subTexts{
		title: g.say("Get new posts by email", "Subscribe by email", "New posts in your inbox", "Follow along by email", "Email updates", "Never miss a post", "Prefer email?"),
		note: g.say("One email when something new goes up. No spam, unsubscribe any time.", "A short note when I publish something new, about twice a month.", "Only new posts, nothing else. Unsubscribe with one click.",
			"I send an email for each new post and nothing in between.", "New posts, delivered when they are ready. Usually once or twice a month.", "Leave your address and I will let you know when there is something new.",
			"No schedule, no tracking, just the posts."),
		button: g.say("Subscribe", "Subscribe", "Sign up", "Sign me up", "Join", "Get updates", "Follow"),
		label:  g.say("Email", "Email address", "Your email", "Email"),
	}
	subForm := subscribePage(g, s, nav, g.say("Subscribe", "Newsletter", "Email updates", "Follow by email", "Subscribe by email", "Get updates"), st)
	s.subscribeAPI(g, subForm)
	box := subscribeBox(g, s, st)

	type post struct {
		title, slug, date, lead, kind, hero, thumb string
		tags                                       []string
		at                                         time.Time
	}
	var entries []post
	hw, hh, pw, ph := g.sk.hero[0], g.sk.hero[1], g.sk.pthumb[0], g.sk.pthumb[1] // 题图、列表小图的尺寸(按种子)
	day := g.n(2, 15)
	pats := g.r.Perm(len(blogTitlePatterns)) // 标题句式一个站里不重样(都是「A few thoughts on …」开头一眼就是模板)
	for i, ti := range g.r.Perm(len(blogTopics))[:8] {
		tp := blogTopics[ti]
		title := capFirst(fmt.Sprintf(blogTitlePatterns[pats[i%len(pats)]], tp.text))
		at := g.base.Add(-time.Duration(day) * 24 * time.Hour)
		p := post{title: title, slug: slugify(title), date: g.date(at), lead: blogLead(g, tp.text), kind: tp.kind, tags: tp.tags, at: at}
		big, small := picSize{w: hw, h: hh}, picSize{w: pw, h: ph, pw: 120, ph: 80}
		if i == 0 { // 首页最上面那篇用大图
			big.pw, big.ph, small.pw, small.ph = 240, 120, 0, 0
		}
		pics := s.picture(g, i, tp.kind, "", big, small)
		p.hero, p.thumb = pics[0], pics[1]
		entries = append(entries, p)
		day += g.n(9, 45)
	}
	// 这个站里反复出现的几处写法,先挑好
	readFmt := g.pick([]string{"%d min read", "%d min read", "%d minute read", "%d min", "about %d minutes", "~%d min read", "%d-minute read", "Reading time: %d min"})
	tagFmt := g.pick([]string{"#%s", "#%s", "%s", "%s"})
	back := g.say("← All writing", "← Back to all posts", "← Home", "All posts", "← Back", "← Index", "↑ Back to the list", "← Everything")
	older := g.say("Older:", "Previous:", "Before this:", "Read next:", "Earlier:", "Previously:", "Next up:")
	latest := g.say("Latest", "New", "Most recent", "Just published", "Newest post", "Latest post", "Fresh")
	moreHead := g.say("More writing", "Earlier posts", "Older posts", "Archive", "Previously", "Everything else", "Past entries", "More posts")
	var list, items strings.Builder
	featured := ""
	for i, p := range entries {
		var body strings.Builder
		fmt.Fprintf(&body, `<p>%s</p>`, esc(p.lead))
		for _, para := range blogParagraphs(g, g.n(2, 4)) {
			fmt.Fprintf(&body, `<p>%s</p>`, esc(para))
		}
		var tags strings.Builder
		for _, tg := range p.tags {
			fmt.Fprintf(&tags, `<span class="%s">%s</span>`, g.c("chip"), esc(fmt.Sprintf(tagFmt, tg)))
		}
		pp := posts + p.slug + "/"
		nav2 := ""
		if i+1 < len(entries) {
			nav2 = fmt.Sprintf(`%s%s <a href="%s%s/">%s</a>`, dot, esc(older), posts, entries[i+1].slug, esc(entries[i+1].title))
		}
		read := fmt.Sprintf(readFmt, g.n(3, 9))
		if i > 0 {
			fmt.Fprintf(&list, `<div class="%s"><a class="%s" href="%s">%s</a><div><a href="%s">%s</a><div class="%s %s">%s%s%s</div><p>%s</p></div></div>`,
				g.c("post"), g.c("pimg"), pp, g.img("", p.thumb, pw, ph, true), pp, esc(p.title), g.c("muted"), g.c("meta"), p.date, dot, esc(read), esc(p.lead))
		}
		s.add(pp, htmlType, layout(g, s, pp, g.title(p.title, s.name), nav,
			fmt.Sprintf(`<article class="%s"><h1>%s</h1><div class="%s">%s<span>%s%s%s%s%s</span></div><div class="%s">%s</div>%s<div class="%s">%s</div>%s<p><a href="/">%s</a>%s</p></article>`,
				g.c("col"), esc(p.title), g.c("byline"), g.img("", av, 0, 0, false), esc(s.name), dot, p.date, dot, esc(read), g.c("hero"), g.img("", p.hero, hw, hh, false), body.String(), g.c("tags"), tags.String(), box, esc(back), nav2)))
		// RSS 里的链接要写全(阅读器不认相对地址),但生成时不知道这个站会挂在哪个域名、哪个端口上:写成以 / 开头的地址,
		// 和这个站里别的链接一样;guid 不当链接用
		fmt.Fprintf(&items, "<item><title>%s</title><link>%s</link><guid isPermaLink=\"false\">%s</guid><pubDate>%s</pubDate><description>%s</description></item>",
			esc(p.title), pp, p.slug, p.at.Add(9*time.Hour).Format(time.RFC1123Z), esc(p.lead))
		if i == 0 {
			featured = fmt.Sprintf(`<a class="%s" href="%s"><div class="%s">%s</div><span class="%s">%s</span><h2>%s</h2><div class="%s %s">%s%s%s</div><p>%s</p></a>`,
				g.c("top1"), pp, g.c("pimg"), g.img("", p.hero, hw, hh, false), g.c("kick"), esc(latest), esc(p.title), g.c("muted"), g.c("meta"), p.date, dot, esc(read), esc(p.lead))
		}
	}
	s.add(feed, "application/rss+xml; charset=utf-8", []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>%s</title><link>/</link><description>%s</description>%s</channel></rss>`,
		esc(s.name), esc(topic), items.String())))
	s.add("/", htmlType, layout(g, s, "/", s.name, nav,
		fmt.Sprintf(`<div class="%s"><div class="%s">%s<div><h1>%s</h1><p class="%s">%s</p></div></div>%s<h2>%s</h2>%s%s</div>`,
			g.c("col"), g.c("author"), g.img("", av, 0, 0, false), esc(s.name), g.c("sub"), esc(topic), featured, esc(moreHead), list.String(), box)))
	hello := fmt.Sprintf(g.say("Hi, I'm %s.", "Hello! I'm %s.", "I'm %s.", "My name is %s.", "%s here.", "Hey, I'm %s.", "Welcome. I'm %s."), first)
	aboutText := g.say("This site is where I keep {notes and pictures|notes, photos and the odd how-to} from things I am working on. It is a personal site with no ads and no tracking.",
		"I started this site to have {one place|somewhere quiet} for the things I make and the places I go. There are no ads here and nothing is tracked.",
		"I write here {a few times a month|when I have something to say|whenever a project is finished}, mostly for myself and a handful of friends.",
		"Everything here is written and photographed by me, slowly, in my spare time. No sponsors, no ads.",
		"This is a personal notebook that happens to be public. Posts are {irregular|occasional}, and I like it that way.",
		"I have kept a site like this for years. It is {small|simple} on purpose: text, a few pictures, and nothing else.")
	mail := g.sk.subPage
	follow := []struct{ pre, href1, text1, mid, href2, text2, post string }{ // 一句话里两个链接:订阅源、邮件订阅(先后各种说法不一样)
		{"If you would rather read it in a feed reader, there is an ", feed, "RSS feed", ", or you can ", mail, "get new posts by email", "."},
		{"You can follow along by ", feed, "RSS", " or ", mail, "email", "."},
		{"New posts go out by ", mail, "email", " and in the ", feed, "feed", "."},
		{"There is a ", feed, "feed", " for readers, and an ", mail, "email list", " if you prefer that."},
		{"Subscribe with ", feed, "RSS", ", or leave your address ", mail, "here", " and I will write when there is something new."},
		{"To keep up, use the ", feed, "RSS feed", " or the ", mail, "newsletter", "."}}[g.r.IntN(6)]
	s.add("/about/", htmlType, layout(g, s, "/about/", g.title(aboutNav, s.name), nav,
		fmt.Sprintf(`<div class="%s">%s<h1>%s</h1><p>%s %s.</p><p>%s</p><p>%s<a href="%s">%s</a>%s<a href="%s">%s</a>%s</p></div>`,
			g.c("col"), g.img(g.c("avbig"), av, 0, 0, false), esc(g.say("About", "About", "About me", "Hello", "Who writes this", "About this site")), esc(hello), esc(topic), esc(aboutText),
			esc(follow.pre), follow.href1, esc(follow.text1), esc(follow.mid), follow.href2, esc(follow.text2), esc(follow.post))))
	s.setNotFound(layout(g, s, "", nfTitle(g, s.name), nav, notFoundBody(g)))
}
