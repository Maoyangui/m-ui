package camo

import (
	"fmt"
	"html"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"
)

// gen 按种子出各种随机选择;同一个种子永远生成同一个站(重启、换模板再换回来都一样)。
type gen struct {
	r    *rand.Rand
	hue  int
	base time.Time // 「最近更新」这类日期的基准:种子决定,不随当天日期变
	sk   skin
	seed uint64
	bank []int // 这个站的文章用句库里的哪些句子(text.go 的 paraBank)
}

// newGen 日期的基准是 now 所在那一周的周一(UTC)再往前 0~2 天:页面上「最近同步」「发布于」这些日期一直是最近的,
// 内容每周最多变一次(固定成某一天的话,过几个月就像一个没人管的站)。
func newGen(seed uint64, now time.Time) *gen {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	base := WeekOf(now).Add(-time.Duration(r.IntN(3)) * 24 * time.Hour)
	g := &gen{r: r, hue: r.IntN(360), base: base, seed: seed}
	g.sk = newSkin(g)
	return g
}

// fork 另起一路随机数(画插图用):画多画少不影响主随机数,有没有照片,站上别的东西都一样
func (g *gen) fork(n int) *gen {
	s := g.seed ^ uint64(n+1)*0x9e3779b97f4a7c15
	return &gen{r: rand.New(rand.NewPCG(s, s^0xd1b54a32d192ed03)), hue: g.hue, base: g.base, sk: g.sk, seed: g.seed}
}

// WeekOf now 所在那一周的周一 0 点(UTC):伪装站按它换内容
func WeekOf(now time.Time) time.Time {
	d := now.UTC().Truncate(24 * time.Hour)
	return d.Add(-time.Duration((int(d.Weekday())+6)%7) * 24 * time.Hour)
}

func (g *gen) pick(list []string) string { return list[g.r.IntN(len(list))] }
func (g *gen) n(lo, hi int) int          { return lo + g.r.IntN(hi-lo+1) }

// pickN 不重复地挑 n 个(保持原来的先后)
func (g *gen) pickN(list []string, n int) []string {
	if n >= len(list) {
		return append([]string(nil), list...)
	}
	idx := g.r.Perm(len(list))[:n]
	keep := map[int]bool{}
	for _, i := range idx {
		keep[i] = true
	}
	var out []string
	for i, x := range list {
		if keep[i] {
			out = append(out, x)
		}
	}
	return out
}

func (g *gen) modTime() time.Time { return g.base.Add(-time.Duration(g.n(1, 20)) * time.Hour) }
func (g *gen) daysAgo(d int) string {
	return g.base.Add(-time.Duration(d) * 24 * time.Hour).Format("2006-01-02")
}
func (g *gen) dateAgo(d int) string {
	return g.date(g.base.Add(-time.Duration(d) * 24 * time.Hour))
}

// date 给人看的日期:写法按种子定(Jan 2, 2006 / 2 Jan 2006 / 2006-01-02 ……),一个站里前后一致
func (g *gen) date(t time.Time) string { return t.Format(g.sk.dateFmt) }

func (g *gen) accent() string     { return hslHex(float64(g.hue), 62, 44) }
func (g *gen) accentSoft() string { return hslHex(float64(g.hue), 70, 95) }

// id 像打包工具、短链接那样的随机串(小写字母和数字)
func (g *gen) id(n int) string {
	const abc = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = abc[g.r.IntN(len(abc))]
	}
	return string(b)
}

func (g *gen) hex(n int) string {
	const abc = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = abc[g.r.IntN(16)]
	}
	return string(b)
}

func (g *gen) b64(n int) string {
	const abc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := make([]byte, n)
	for i := range b {
		b[i] = abc[g.r.IntN(64)]
	}
	return string(b)
}

var esc = html.EscapeString

// words 站名、产品名里用的词。原来只有 22 个:镜像站名一共 88 种、视频站名 88 种,几十台里就有重名的,
// 站名本身也成了能枚举的特征(审查 F-H1)
var words = []string{"Lumen", "Northgate", "Cedar", "Harbor", "Quarry", "Juniper", "Meridian", "Granite", "Kestrel", "Tidal",
	"Copper", "Orchard", "Summit", "Vesper", "Alder", "Basalt", "Fernway", "Halcyon", "Larch", "Pinecrest", "Riverside", "Saltmarsh",
	"Amber", "Birchwood", "Calico", "Dovetail", "Elm", "Foxglove", "Greyfield", "Heron", "Ironwood", "Jasper", "Kelp", "Linden",
	"Marlow", "Nettle", "Oakridge", "Pebble", "Quill", "Rowan", "Sable", "Thistle", "Umber", "Verdant", "Willow", "Yarrow",
	"Zephyr", "Aster", "Bramble", "Cobalt", "Drift", "Ember", "Flint", "Gable", "Hazel", "Indigo", "Keystone", "Lantern",
	"Moss", "Northwind", "Opal", "Parcel"}

// ---- 皮肤 ----

// skin 每个种子一套:类名、静态文件放的目录、排版的几个尺寸和字体。所有 m-ui 的伪装站出自同一份代码,
// 类名、目录、版式写死的话页面骨架本身就成了指纹;按种子换掉,不同服务器上的站看不出出自一处。
type skin struct {
	cls     map[string]string
	dir     string // 样式表、图片放的目录
	css     string // 样式表的路径
	maxW    int    // 内容最宽
	fs      int    // 正文字号
	head    int    // 顶栏高
	rad     int    // 圆角
	font    string
	bg      string
	navLeft bool   // 导航紧跟站名(否则靠右)
	kf      string // CSS 关键帧名的前缀

	api      string // 接口前缀(视频站、软件下载、博客;api.go)
	formKey  string // 表单上写接口地址的 data- 属性名
	playKey  string // 播放按钮上写视频 id 的 data- 属性名
	osKey    string // 下载卡片上写系统的 data- 属性名
	imgName  string // 照片文件名的写法(词干、宽度)
	errStyle int    // 接口报错的写法(几种常见框架的样子)
	camel    bool   // 接口字段名用 camelCase(否则 snake_case)

	// 下面几样原来各台都一样(审查 F-H1),现在按种子换
	lang      string // <html lang>
	sep       string // 页面标题里页名和站名之间的分隔
	siteFirst bool   // 页面标题里站名在前
	icon      string // 站点图标的路径
	robots    string // robots.txt 的内容
	copyFmt   string // 页脚版权那一句的写法(%[1]d 年份、%[2]s 站名)
	mailPH    string // 邮箱输入框里的示例
	dateFmt   string // 给人看的日期的写法
	dot       string // 「时长 · 播放量」这类并列项之间的分隔
	cache     string // 页面的 Cache-Control

	// 页面骨架的写法。同一段标记各台一字不差的话,拿那段标记去搜和拿一句话去搜是一样的(搜索引擎收的是整页源码)
	nl       string // 顶层几块之间的分隔(有的站源码压成一行,有的不压)
	ind      string // <head> 里各行的缩进
	doctype  string
	charset  string
	viewport string
	iconTag  string // 站点图标那一行的写法(%s 是路径)
	headOrd  int    // <head> 里标题、描述、样式表这几样的先后
	og       int    // Open Graph 那几行:0 不带,1~3 几种常见的组合

	// 各种站自己的地址(原来 /watch/、/posts/、/feed.xml、/subscribe/、/releases/ 各台都一样)
	watch   string // 视频页的前缀
	posts   string // 文章的前缀
	feed    string // RSS 的地址
	subPage string // 订阅页的地址
	subAPI  string // 订阅接口(相对接口前缀)
	rel     string // 各版本页的前缀
	nextKey string // 登录后回到哪一页的参数名

	imgStyle int    // <img> 的属性先后、带不带宽高
	inStyle  int    // <input> 的属性先后
	thumb    [2]int // 视频封面小图的尺寸
	big      [2]int // 播放页大图
	wide     [2]int // 首页精选
	hero     [2]int // 博客题图
	pthumb   [2]int // 博客列表里的小图
	chart    [2]int // 镜像站流量图
}

var clsWords = map[string][]string{
	"wrap": {"wrap", "container", "inner", "page", "layout"}, "logo": {"logo", "mark", "brand-mark", "site-logo", "emblem"},
	"name": {"brand", "site-name", "home", "site-title", "name"}, "sub": {"lead", "subtitle", "intro", "tagline", "summary"},
	"card": {"card", "panel", "box", "surface", "block"}, "chip": {"tag", "pill", "label", "chip", "token"},
	"muted": {"muted", "meta", "dim", "quiet", "note"}, "mono": {"mono", "num", "code", "digits", "tnum"},
	"r": {"right", "ta-r", "end", "num-col", "align-r"}, "hide": {"hide-sm", "wide-only", "md-up", "lg-only", "desk"},
	"grid": {"grid", "items", "cards", "tiles", "gallery"}, "thumb": {"thumb", "cover", "poster", "media", "frame"},
	"dur": {"duration", "time", "length", "runtime", "len"}, "vt": {"v-title", "item-title", "caption", "heading", "headline"},
	"post": {"post", "entry", "item", "story", "row"}, "pimg": {"post-img", "entry-img", "pic", "figure", "img"},
	"btn": {"btn", "button", "action", "cta", "dl"}, "hero": {"hero", "player", "masthead", "feature", "banner"},
	"shot": {"shot", "screen", "preview", "showcase", "visual"}, "av": {"avatar", "me", "portrait", "author", "pfp"},
	"head2": {"page-head", "top", "intro-row", "header-row", "head"}, "dl": {"downloads", "platforms", "os-list", "get", "choices"},
	"on": {"active", "current", "is-active", "selected", "on"}, "e404": {"notfound", "err-page", "missing", "e404", "nf"},
	"stats": {"stats", "numbers", "facts", "summary-row", "kpis"}, "stat": {"stat", "fact", "kpi", "figure-box", "count"},
	"ok": {"ok", "synced", "status-ok", "good", "fresh"}, "busy": {"busy", "syncing", "status-run", "running", "pending"},
	"dir": {"dir", "folder", "d", "is-dir", "f-dir"}, "file": {"file", "doc", "f", "is-file", "f-file"},
	"crumbs": {"crumbs", "breadcrumb", "path", "trail", "where"}, "chart": {"chart", "graph", "usage", "traffic", "plot"},
	"play": {"play", "video-mark", "tv", "reel", "watch"}, "cats": {"cats", "filters", "topics", "tabs", "sections"},
	"pbar": {"controls", "bar", "player-bar", "ctrl", "transport"}, "chan": {"channel", "uploader", "owner", "creator", "by"},
	"feats": {"features", "highlights", "perks", "why", "benefits"}, "feat": {"feature-item", "perk", "point", "benefit", "plus"},
	"ficon": {"f-icon", "ico", "symbol", "glyph", "badge-ico"}, "author": {"author-card", "bio", "about-me", "profile", "intro-card"},
	"byline": {"byline", "post-meta", "written", "credit", "who"}, "tags": {"tags", "topics-list", "labels", "keywords", "tag-list"},
	"gate": {"gate", "members", "overlay", "locked", "paywall"}, "spin": {"spinner", "loader", "loading", "ring", "wait"},
	"msg": {"gate-msg", "notice", "message", "lock-text", "prompt"},
	// 用户 11-19 精修加的:精选、播放页右栏、登录注册表单、订阅、软件窗口、流量图坐标
	"cta": {"signin", "account-link", "nav-cta", "login-btn", "member"}, "lead2": {"featured", "spotlight", "promo", "hero-card", "headliner"},
	"kick": {"kicker", "eyebrow", "overline", "label-sm", "pre"}, "watch": {"watch", "watch-layout", "two-col", "split", "with-side"},
	"up": {"up-next", "related", "sidebar", "next-list", "queue"}, "auth": {"auth", "sign", "login-box", "account", "entry-box"},
	"field": {"field", "form-row", "input-group", "control", "fld"}, "alert": {"alert", "flash", "form-msg", "callout", "status-msg"},
	"okay": {"success", "is-ok", "positive", "done", "good-msg"}, "bad": {"invalid", "has-error", "is-bad", "error", "wrong"},
	"busy2": {"loading-btn", "is-busy", "sending", "in-progress", "wait-btn"}, "authed": {"signed-in", "logged-in", "has-session", "authed", "member-on"},
	"btn2": {"btn-ghost", "btn-outline", "secondary", "ghost", "alt-btn"}, "acts": {"actions", "buttons", "btn-row", "ctas", "choices-row"},
	"alt": {"switch", "alt-link", "form-foot", "below", "aside-link"}, "subs": {"newsletter", "subscribe", "mailing", "signup-box", "follow"},
	"srow": {"inline-form", "sub-row", "email-row", "row-form", "field-row"}, "col": {"column", "measure", "readable", "content", "prose"},
	"top1": {"latest", "lead-post", "top-story", "first-post", "headline-post"}, "narrow": {"narrow", "slim", "small-col", "tight", "thin"},
	"win": {"app-window", "window", "mock", "app", "frame-app"}, "wbar": {"titlebar", "chrome", "win-bar", "top-bar", "caption-bar"},
	"wbody": {"app-body", "win-body", "workspace", "app-main", "shell"}, "side": {"app-side", "sidebar-nav", "nav-pane", "source-list", "rail"},
	"wmain": {"app-content", "pane", "main-pane", "detail", "view"}, "tool": {"toolbar", "tools", "actions-bar", "command-bar", "tb"},
	"srch": {"search", "search-box", "find", "filter-box", "lookup"}, "lhead": {"list-head", "section-label", "group-label", "sub-head", "caption"},
	"lrow": {"list-row", "item-row", "entry-row", "line", "cell-row"}, "ico": {"icon", "file-ico", "glyph-sm", "kind", "type-ico"},
	"prog": {"progress", "bar-sm", "meter", "upload-bar", "pbar-sm"}, "pgrid": {"photo-grid", "thumbs", "mosaic", "image-grid", "contact-sheet"},
	"stage": {"stage", "viewer", "screen-main", "canvas-main", "monitor"}, "ctrl": {"player-controls", "hud", "transport-bar", "overlay-bar", "osd"},
	"track": {"track", "lane", "channel-row", "strip", "take"}, "ms": {"ms", "toggle-sm", "mute", "chip-sm", "btn-xs"},
	"wave": {"waveform", "wave", "audio-wave", "levels", "peaks"}, "notes": {"notes", "split-notes", "note-view", "two-pane", "editor"},
	"nlist": {"note-list", "notes-list", "list-pane", "index", "entries"}, "ntext": {"note-body", "doc", "page-body", "note-text", "editor-body"},
	"dot": {"dot", "presence", "status-dot", "led", "indicator"}, "canvas": {"canvas", "art-board", "board", "paper", "drawing"},
	"axis": {"axis", "x-axis", "ticks", "scale", "chart-labels"}, "det": {"details", "meta-grid", "specs", "facts-row", "video-info"},
	// 审查 F-H1:原来写成内联样式的几处(内联样式的值各台一字不差,也能拿去搜)改成类
	"meta": {"meta-line", "small", "sm", "info-line", "minor"}, "fine": {"fine", "footnote", "smallprint", "hint", "aside-note"},
	"cap": {"chart-note", "legend", "under", "explain", "sub-caption"}, "avbig": {"avatar-lg", "portrait-lg", "face", "photo-me", "me-lg"},
}

var fonts = []string{
	`-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif`,
	`system-ui,-apple-system,"Segoe UI",Roboto,Ubuntu,Cantarell,"Noto Sans",sans-serif`,
	`"Helvetica Neue",Helvetica,Arial,sans-serif`,
	`"Segoe UI",Tahoma,Geneva,Verdana,sans-serif`,
}

func newSkin(g *gen) skin {
	keys := make([]string, 0, len(clsWords))
	for k := range clsWords {
		keys = append(keys, k)
	}
	sort.Strings(keys) // map 的顺序每次不同:先排好,同一种子才生成同一套
	// 前缀第一个字符必须是字母:CSS 类名不能以数字开头(以前用 g.id(2),约三成以数字开头,那一套样式规则整个失效 —— 自审 G1)
	style, pre := g.r.IntN(3), string(rune('a'+g.r.IntN(26)))+g.id(1)
	cls, used := map[string]string{}, map[string]bool{}
	for _, k := range keys {
		c := g.pick(clsWords[k])
		switch style {
		case 1:
			c = pre + "-" + c
		case 2:
			c = "_" + g.id(5) // 像 CSS Modules 那样的哈希类名
		}
		for used[c] {
			c += strconv.Itoa(g.r.IntN(10))
		}
		used[c] = true
		cls[k] = c
	}
	dir := g.pick([]string{"/static/", "/assets/", "/s/", "/res/", "/dist/"})
	return skin{cls: cls, dir: dir, css: dir + g.pick([]string{"site", "main", "style", "app", "theme"}) + "." + g.id(8) + ".css",
		maxW: 940 + 40*g.r.IntN(7), fs: 14 + g.r.IntN(3), head: 56 + 2*g.r.IntN(7), rad: 6 + 2*g.r.IntN(4),
		font: g.pick(fonts), bg: g.pick([]string{"#f7f8fa", "#fafaf8", "#f6f7fb", "#ffffff", "#f8f7f4"}), navLeft: g.r.IntN(4) == 0, kf: "k" + g.id(4),
		api:     g.pick([]string{"/api/v1/", "/api/", "/api/v2/", "/v1/", "/_api/"}),
		formKey: g.pick([]string{"api", "endpoint", "action", "submit-to", "post"}), playKey: g.pick([]string{"play", "video", "vid", "watch-id", "media"}),
		osKey: g.pick([]string{"os", "platform", "target", "dl-os"}), imgName: g.pick([]string{"%s-%d.jpg", "%s_%dw.jpg", "%s.%d.jpg", "%s-%dw.jpg", "%s@%d.jpg"}),
		errStyle: g.r.IntN(3), camel: g.r.IntN(2) == 0,
		lang: g.pick([]string{"en", "en", "en-US", "en-GB"}), sep: g.pick([]string{" - ", " | ", " · ", " — ", ": "}), siteFirst: g.r.IntN(3) == 0,
		icon:    g.pick([]string{"/favicon.svg", "/icon.svg", dir + "favicon.svg", dir + "icon." + g.id(6) + ".svg", "/logo.svg"}),
		robots:  g.pick([]string{"User-agent: *\nDisallow:\n", "User-agent: *\nAllow: /\n", "User-agent: *\nDisallow: /search\n", "# robots.txt\nUser-agent: *\nDisallow:\n", "User-agent: *\nCrawl-delay: 10\nDisallow:\n"}),
		copyFmt: g.pick([]string{"© %[1]d %[2]s", "Copyright © %[1]d %[2]s", "%[2]s © %[1]d", "© %[1]d %[2]s. All rights reserved.", "© %[2]s, %[1]d"}),
		mailPH:  g.pick([]string{"you@example.com", "name@example.com", "Email address", "Your email", "email@domain.com", "jane@example.org"}),
		dateFmt: g.pick([]string{"Jan 2, 2006", "Jan 2, 2006", "2 Jan 2006", "January 2, 2006", "2006-01-02", "2 January 2006", "Jan 02, 2006"}),
		dot:     g.pick([]string{" · ", " · ", " • ", " | ", " – ", ", "}),
		cache:   g.pick([]string{"public, max-age=3600", "max-age=3600", "public, max-age=600", "max-age=1800", "public, max-age=7200", "max-age=14400", "public, max-age=86400", "no-cache"}),
		nl:      g.pick([]string{"", "", "\n", "\n", "\r\n"}), ind: g.pick([]string{"", "", "  ", "    ", "\t"}),
		doctype: g.pick([]string{"<!doctype html>", "<!DOCTYPE html>", "<!DOCTYPE html>"}), charset: g.pick([]string{`<meta charset="utf-8">`, `<meta charset="UTF-8">`, `<meta charset="utf-8">`}),
		viewport: g.pick([]string{"width=device-width,initial-scale=1", "width=device-width, initial-scale=1", "width=device-width, initial-scale=1.0", "width=device-width, initial-scale=1, viewport-fit=cover"}),
		iconTag:  g.pick([]string{`<link rel="icon" href="%s">`, `<link rel="icon" type="image/svg+xml" href="%s">`, `<link rel="shortcut icon" href="%s">`, `<link href="%s" rel="icon">`}),
		headOrd:  g.r.IntN(4), og: g.r.IntN(4),
		watch: g.pick([]string{"/watch/", "/v/", "/video/", "/videos/", "/play/", "/film/", "/w/"}), posts: g.pick([]string{"/posts/", "/blog/", "/writing/", "/notes/", "/p/", "/journal/", "/entries/"}),
		feed: g.pick([]string{"/feed.xml", "/rss.xml", "/index.xml", "/feed/", "/rss/", "/posts.xml"}), subPage: g.pick([]string{"/subscribe/", "/newsletter/", "/updates/", "/email/", "/follow/", "/notify/"}),
		subAPI: g.pick([]string{"subscribe", "newsletter", "subscribers", "newsletter/subscribe", "subscriptions", "notify"}), rel: g.pick([]string{"/releases/", "/release/", "/versions/", "/changelog/", "/history/", "/archive/"}),
		nextKey:  g.pick([]string{"next", "return", "redirect", "r", "continue", "from", "back"}),
		imgStyle: g.r.IntN(5), inStyle: g.r.IntN(4),
		thumb:  [][2]int{{400, 225}, {426, 240}, {480, 270}, {512, 288}, {640, 360}}[g.r.IntN(5)],
		big:    [][2]int{{854, 480}, {896, 504}, {960, 540}, {1024, 576}}[g.r.IntN(4)],
		wide:   [][2]int{{1120, 490}, {1200, 525}, {1280, 560}, {1360, 595}}[g.r.IntN(4)],
		hero:   [][2]int{{880, 440}, {900, 450}, {960, 480}, {1000, 500}, {1040, 520}}[g.r.IntN(5)],
		pthumb: [][2]int{{300, 200}, {330, 220}, {360, 240}, {390, 260}, {420, 280}}[g.r.IntN(5)],
		chart:  [][2]int{{900, 140}, {920, 160}, {960, 150}, {1000, 160}, {1040, 150}}[g.r.IntN(5)]}
}

// img 一张图片的标记。属性的先后、带不带宽高、懒加载怎么写按种子定:同一段
// 「alt="" width="480" height="270" loading="lazy"」各台一字不差的话,和一句固定的话一样能拿去搜(审查 F-H1)。
// 宽高写不写都行 —— 各处图片的大小都由样式表定(容器的比例、object-fit)。w 为 0 时不写宽高。
func (g *gen) img(cls, src string, w, h int, lazy bool) string {
	c, lz, dim := "", "", ""
	if cls != "" {
		c = ` class="` + cls + `"`
	}
	if lazy {
		lz = ` loading="lazy"`
	}
	if w > 0 {
		dim = fmt.Sprintf(` width="%d" height="%d"`, w, h)
	}
	src = ` src="` + src + `"`
	switch g.sk.imgStyle {
	case 0:
		return "<img" + c + src + ` alt=""` + dim + lz + ">"
	case 1:
		return "<img" + c + lz + src + dim + ` alt="">`
	case 2:
		if lazy {
			lz += ` decoding="async"`
		}
		return "<img" + c + src + dim + ` alt=""` + lz + ">"
	case 3: // 不写宽高
		return "<img" + c + ` alt=""` + src + lz + ">"
	}
	return "<img" + src + c + lz + ` alt=""` + dim + ">"
}

// input 表单的一个输入框(属性先后按种子);more 是另外要加的属性(placeholder、aria-label)
func (g *gen) input(name, typ, auto, more string) string {
	switch g.sk.inStyle {
	case 0:
		return fmt.Sprintf(`<input name="%s" type="%s" autocomplete="%s"%s required>`, name, typ, auto, more)
	case 1:
		return fmt.Sprintf(`<input type="%s" name="%s"%s autocomplete="%s" required>`, typ, name, more, auto)
	case 2:
		return fmt.Sprintf(`<input type="%s" name="%s" required autocomplete="%s"%s>`, typ, name, auto, more)
	}
	return fmt.Sprintf(`<input%s required autocomplete="%s" type="%s" name="%s">`, more, auto, typ, name)
}

// c 逻辑类名 → 这个种子下的实际类名
func (g *gen) c(k string) string { return g.sk.cls[k] }

// 目录列表里的文件夹、文件小图标(CSS 遮罩,跟着链接颜色)
const iconDir = `url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Cpath d='M1.5 4A1.5 1.5 0 0 1 3 2.5h3.2l1.6 1.6H13A1.5 1.5 0 0 1 14.5 5.6v6.9A1.5 1.5 0 0 1 13 14H3a1.5 1.5 0 0 1-1.5-1.5z'/%3E%3C/svg%3E")`
const iconFile = `url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Cpath d='M4 1.5h5.2L13 5.3v8.2a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-11a1 1 0 0 1 1-1zm5 .8V6h3.7'/%3E%3C/svg%3E")`

// css 整站共用的样式表(单独一个文件,浏览器缓存;每页只有内容)
func (g *gen) css() []byte {
	c, k := g.c, g.sk
	acc, soft := g.accent(), g.accentSoft()
	nav := "margin-left:auto"
	if k.navLeft {
		nav = "margin-left:22px"
	}
	var b strings.Builder
	w := func(format string, a ...interface{}) { fmt.Fprintf(&b, format, a...) }
	w(`*{box-sizing:border-box}body{margin:0;font:%dpx/1.6 %s;color:#1c2330;background:%s;-webkit-font-smoothing:antialiased}a{color:%s;text-decoration:none}a:hover{text-decoration:underline}`, k.fs, k.font, k.bg, acc)
	w(`img{display:block;max-width:100%%}.%s{max-width:%dpx;margin:0 auto;padding:0 22px}`, c("wrap"), k.maxW)
	w(`header{position:sticky;top:0;z-index:10;background:rgba(255,255,255,.96);border-bottom:1px solid #e5e8ee}header .%s{display:flex;align-items:center;gap:12px;height:%dpx}`, c("wrap"), k.head)
	w(`.%s{display:flex;align-items:center;gap:10px;font-weight:650;font-size:16px;color:#1c2330}.%s:hover{text-decoration:none}`, c("name"), c("name"))
	w(`.%s{position:relative;width:26px;height:26px;border-radius:%dpx;background:%s;flex:none;object-fit:cover}.%s{border-radius:50%%}`, c("logo"), k.rad, acc, c("av"))
	w(`.%s.%s::after{content:"";position:absolute;left:10px;top:7px;border-left:9px solid #fff;border-top:6px solid transparent;border-bottom:6px solid transparent}`, c("logo"), c("play"))
	w(`nav{%s;display:flex;gap:20px;font-size:14px}nav a{color:#5f6878}nav a.%s{color:#1c2330;font-weight:600}main.%s{padding-top:34px;padding-bottom:10px;min-height:60vh}`, nav, c("on"), c("wrap"))
	b.WriteString(`h1{font-size:28px;line-height:1.25;margin:0 0 8px;letter-spacing:-.01em}h2{font-size:19px;margin:30px 0 12px}h3{font-size:16px;margin:0 0 4px}`)
	w(`.%s{color:#5f6878;margin:0 0 22px}.%s{background:#fff;border:1px solid #e5e8ee;border-radius:%dpx;overflow:hidden}`, c("sub"), c("card"), k.rad+4)
	b.WriteString(`table{width:100%;border-collapse:collapse;font-size:14px}th,td{text-align:left;padding:10px 14px;border-bottom:1px solid #e5e8ee}th{font-weight:600;color:#5f6878;font-size:12.5px;background:#fbfbfc}tr:last-child td{border-bottom:0}tbody tr:hover td{background:#fafbfc}`)
	w(`td.%s,th.%s{text-align:right}.%s{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px}`, c("r"), c("r"), c("mono"))
	w(`.%s{display:inline-block;padding:3px 10px;border-radius:999px;background:%s;color:%s;font-size:12.5px;font-weight:600;margin:0 6px 6px 0}.%s{color:#5f6878}`, c("chip"), soft, acc, c("muted"))
	w(`footer{color:#5f6878;font-size:13px;padding:34px 0 46px;border-top:1px solid #e5e8ee;margin-top:30px}footer .%s{display:flex;flex-wrap:wrap;justify-content:space-between;gap:6px 18px}`, c("wrap"))
	w(`.%s{text-align:center;padding:56px 0 24px}.%s b{display:block;font-size:84px;line-height:1;font-weight:800;color:%s;letter-spacing:-.04em;margin-bottom:14px}`, c("e404"), c("e404"), acc)
	// 镜像
	w(`.%s{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:12px;margin:0 0 22px}.%s{background:#fff;border:1px solid #e5e8ee;border-radius:%dpx;padding:14px 16px}`, c("stats"), c("stat"), k.rad+4)
	w(`.%s b{display:block;font-size:22px;letter-spacing:-.01em;line-height:1.3}.%s span{color:#5f6878;font-size:13px}`, c("stat"), c("stat"))
	w(`.%s,.%s{display:inline-flex;align-items:center;gap:6px;font-size:13px;white-space:nowrap}.%s{color:#23774a}.%s{color:#2a62b8}`, c("ok"), c("busy"), c("ok"), c("busy"))
	w(`.%s::before,.%s::before{content:"";width:7px;height:7px;border-radius:50%%;background:#2fb26b}.%s::before{background:#3b82f6}`, c("ok"), c("busy"), c("busy"))
	w(`a.%s::before,a.%s::before{content:"";display:inline-block;width:15px;height:15px;margin-right:8px;vertical-align:-2px;background:currentColor;-webkit-mask:%s center/contain no-repeat;mask:%s center/contain no-repeat}`, c("dir"), c("file"), iconDir, iconDir)
	w(`a.%s::before{-webkit-mask-image:%s;mask-image:%s;opacity:.75}`, c("file"), iconFile, iconFile)
	w(`.%s{font-size:14px;color:#5f6878;margin:0 0 6px}.%s a{color:#5f6878}.%s{margin:4px 0 26px}.%s img{width:100%%;height:auto}`, c("crumbs"), c("crumbs"), c("chart"), c("chart"))
	// 视频
	w(`.%s{display:flex;flex-wrap:wrap;gap:8px;margin:0 0 22px}.%s span{padding:5px 13px;border-radius:999px;background:#fff;border:1px solid #e5e8ee;font-size:13px;color:#3b4454}.%s span:first-child{background:#1c2330;border-color:#1c2330;color:#fff}`, c("cats"), c("cats"), c("cats"))
	w(`.%s{display:grid;grid-template-columns:repeat(auto-fill,minmax(230px,1fr));gap:22px 18px}.%s a{color:inherit}.%s a:hover{text-decoration:none}.%s a:hover .%s{color:%s}`, c("grid"), c("grid"), c("grid"), c("grid"), c("vt"), acc)
	w(`.%s{position:relative;aspect-ratio:16/9;border-radius:%dpx;overflow:hidden;background:#dfe3ea}.%s img{width:100%%;height:100%%;object-fit:cover;transition:transform .25s}.%s a:hover .%s img{transform:scale(1.03)}`, c("thumb"), k.rad+4, c("thumb"), c("grid"), c("thumb"))
	w(`.%s{position:absolute;right:8px;bottom:8px;background:rgba(0,0,0,.7);color:#fff;font-size:12px;padding:1px 6px;border-radius:5px}.%s{font-weight:600;margin-top:9px;line-height:1.35}`, c("dur"), c("vt"))
	w(`.%s{position:relative;aspect-ratio:16/9;border-radius:%dpx;overflow:hidden;background:#111}.%s img{width:100%%;height:100%%;object-fit:cover}`, c("hero"), k.rad+6, c("hero"))
	w(`.%s>a{position:absolute;inset:0;display:block}.%s>a>svg{position:absolute;left:50%%;top:50%%;width:76px;height:76px;margin:-38px 0 0 -38px;filter:drop-shadow(0 2px 8px rgba(0,0,0,.35));transition:transform .15s}.%s>a:hover>svg{transform:scale(1.06)}`, c("hero"), c("hero"), c("hero"))
	// 点了播放:先转一会儿圈,再说「会员才能看」(和「只给会员看的片库」这个说法对得上;纯 CSS,页面里没有脚本)。关键帧的名字也按种子换
	kf := g.sk.kf
	w(`.%s{position:absolute;inset:0;z-index:2;display:none;flex-direction:column;align-items:center;justify-content:center;gap:10px;padding:24px;text-align:center;background:rgba(10,12,18,.84);-webkit-backdrop-filter:blur(4px);backdrop-filter:blur(4px);color:#fff;scroll-margin-top:%dpx}.%s:target{display:flex}.%s:target~a,.%s:target~.%s{display:none}`, c("gate"), k.head+24, c("gate"), c("gate"), c("gate"), c("pbar"))
	w(`.%s .%s{width:34px;height:34px;border-radius:50%%;border:3px solid rgba(255,255,255,.25);border-top-color:#fff;animation:%sr .8s linear infinite,%sh 0s 1.3s forwards}`, c("gate"), c("spin"), kf, kf)
	w(`.%s .%s{display:flex;flex-direction:column;align-items:center;gap:8px;opacity:0;animation:%sf .3s 1.3s forwards}.%s .%s b{font-size:18px}.%s .%s a{color:#fff;opacity:.85;font-size:13px;text-decoration:underline}`, c("gate"), c("msg"), kf, c("gate"), c("msg"), c("gate"), c("msg"))
	w(`@keyframes %sr{to{transform:rotate(360deg)}}@keyframes %sh{to{width:0;height:0;border-width:0;opacity:0}}@keyframes %sf{to{opacity:1}}`, kf, kf, kf)
	w(`.%s{position:absolute;left:0;right:0;bottom:0;display:flex;align-items:center;gap:12px;padding:26px 16px 12px;background:linear-gradient(transparent,rgba(0,0,0,.62));color:#fff;font-size:12.5px}`, c("pbar"))
	w(`.%s i{flex:1;height:4px;border-radius:2px;background:rgba(255,255,255,.35);overflow:hidden}.%s i::before{content:"";display:block;width:0;height:100%%;background:%s}.%s s{width:0;height:0;border-left:10px solid #fff;border-top:6px solid transparent;border-bottom:6px solid transparent}`, c("pbar"), c("pbar"), acc, c("pbar"))
	w(`.%s{display:flex;align-items:center;gap:12px;margin:16px 0 14px}.%s>span:first-child{width:40px;height:40px;border-radius:50%%;background:%s;flex:none}.%s b{display:block;line-height:1.3}`, c("chan"), c("chan"), acc, c("chan"))
	// 博客
	w(`.%s{display:flex;align-items:center;gap:16px;margin:0 0 28px}.%s img{width:64px;height:64px;border-radius:50%%;flex:none}.%s h1{margin:0}`, c("author"), c("author"), c("author"))
	w(`.%s{display:grid;grid-template-columns:176px 1fr;gap:20px;padding:18px 0;border-bottom:1px solid #e5e8ee;align-items:center}.%s{aspect-ratio:3/2;border-radius:%dpx;overflow:hidden;background:#e7e9ee}.%s img{width:100%%;height:100%%;object-fit:cover}`, c("post"), c("pimg"), k.rad+2, c("pimg"))
	w(`.%s{display:flex;align-items:center;gap:10px;color:#5f6878;font-size:14px;margin:0 0 18px}.%s img{width:28px;height:28px;border-radius:50%%}`, c("byline"), c("byline"))
	w(`article .%s{aspect-ratio:2/1;margin:6px 0 24px}article p{line-height:1.75}.%s{margin:26px 0 10px}`, c("hero"), c("tags"))
	// 软件下载
	w(`.%s{display:flex;align-items:center;gap:18px;margin-bottom:6px}.%s img{width:64px;height:64px;flex:none}`, c("head2"), c("head2"))
	w(`.%s{display:grid;grid-template-columns:repeat(auto-fit,minmax(210px,1fr));gap:14px;margin:18px 0}.%s .%s{padding:18px 20px}`, c("dl"), c("dl"), c("card"))
	w(`.%s{display:inline-block;padding:8px 16px;border-radius:8px;background:%s;color:#fff;font-weight:600;font-size:14px}.%s:hover{text-decoration:none;filter:brightness(1.07)}`, c("btn"), acc, c("btn"))
	w(`.%s{margin:8px 0 6px;border-radius:%dpx;overflow:hidden;border:1px solid #e5e8ee;box-shadow:0 22px 44px -22px rgba(20,30,50,.38)}`, c("shot"), k.rad+6)
	w(`.%s{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:22px;margin:30px 0 8px}.%s p{margin:0;color:#5f6878;font-size:14px}`, c("feats"), c("feat"))
	w(`.%s{width:38px;height:38px;border-radius:10px;background:%s;color:%s;display:grid;place-items:center;margin-bottom:12px}.%s svg{width:20px;height:20px}`, c("ficon"), soft, acc, c("ficon"))
	g.cssMore(w, acc, soft)
	w(`@media(max-width:980px){.%s{grid-template-columns:1fr}.%s>a{grid-template-columns:150px minmax(0,1fr)}}`, c("watch"), c("up"))
	w(`p .%s{overflow-wrap:anywhere}`, c("mono"))
	w(`@media(max-width:640px){nav{gap:14px}h1{font-size:23px}.%s{display:none}.%s{grid-template-columns:1fr}.%s b{font-size:64px}`, c("hide"), c("post"), c("e404"))
	w(`header .%s{height:auto;min-height:%dpx;flex-wrap:wrap;justify-content:space-between;row-gap:0}.%s{min-height:%dpx}nav{margin-left:0;min-height:40px;align-items:center;white-space:nowrap}`, c("wrap"), k.head, c("name"), k.head)
	w(`.%s{aspect-ratio:4/3}.%s>div{left:18px;right:18px;bottom:18px}.%s h2{font-size:22px}.%s h2{font-size:21px}.%s .%s{padding:22px 18px}.%s{margin-left:auto;padding:5px 12px;font-size:13px}nav:has(+.%s){display:none}.%s{white-space:nowrap}`,
		c("lead2"), c("lead2"), c("lead2"), c("top1"), c("auth"), c("card"), c("cta"), c("cta"), c("name"))
	w(`.%s,.%s{grid-template-columns:1fr}.%s,.%s{display:none}.%s{min-height:0}.%s{grid-template-columns:repeat(2,minmax(0,1fr))}}`, c("wbody"), c("notes"), c("side"), c("nlist"), c("wbody"), c("det"))
	return []byte(b.String())
}

// cssMore 用户 11-19 精修加的样式:顶栏按钮、登录注册表单与提示、邮件订阅、视频站精选与右栏、博客头条、软件窗口、流量图坐标
func (g *gen) cssMore(w func(string, ...interface{}), acc, soft string) {
	c, k := g.c, g.sk
	ctaML := "18px"
	if k.navLeft {
		ctaML = "auto"
	}
	w(`.%s{margin-left:%s;padding:6px 15px;border:1px solid #d5dae2;border-radius:999px;background:#fff;color:#1c2330;font-size:13.5px;font-weight:600;white-space:nowrap}.%s:hover{text-decoration:none;border-color:%s;color:%s}`, c("cta"), ctaML, c("cta"), acc, acc)
	w(`.%s{max-width:420px;margin:14px auto 30px}.%s .%s{padding:30px 30px 26px}.%s h1{font-size:24px;text-align:center;margin:0 0 6px}.%s .%s{text-align:center;font-size:14px;margin:0 0 22px}`, c("auth"), c("auth"), c("card"), c("auth"), c("auth"), c("sub"))
	w(`.%s{display:block;margin:0 0 16px}.%s>span{display:flex;justify-content:space-between;align-items:baseline;gap:10px;font-size:13.5px;font-weight:600;margin:0 0 6px}.%s>span a{font-weight:500;font-size:13px}.%s small{display:block;color:#6b7383;font-size:12.5px;line-height:1.45;margin-top:6px}`, c("field"), c("field"), c("field"), c("field"))
	w(`.%s input,.%s input{display:block;width:100%%;height:42px;padding:0 12px;border:1px solid #d5dae2;border-radius:%dpx;background:#fff;color:inherit;font:inherit}.%s input:focus,.%s input:focus{outline:none;border-color:%s;box-shadow:0 0 0 3px %s}`, c("field"), c("srow"), k.rad, c("field"), c("srow"), acc, soft)
	w(`input.%s{border-color:#d64545!important}.%s{margin:0 0 16px;padding:10px 12px;border-radius:%dpx;border:1px solid #f3cccc;background:#fdeeee;color:#a12f2f;font-size:13.5px;line-height:1.45}.%s.%s{border-color:#c8e8d5;background:#ebf7f0;color:#1e6b43}.%s[hidden]{display:none}`, c("bad"), c("alert"), k.rad, c("alert"), c("okay"), c("alert"))
	w(`.%s button,.%s button{display:block;width:100%%;height:42px;border:0;border-radius:%dpx;background:%s;color:#fff;font:inherit;font-weight:600;cursor:pointer}.%s button:hover,.%s button:hover{filter:brightness(1.07)}.%s{opacity:.7;cursor:progress!important}.%s{text-align:center;color:#5f6878;font-size:13.5px;margin:18px 0 0}`, c("auth"), c("srow"), k.rad, acc, c("auth"), c("srow"), c("busy2"), c("alt"))
	w(`.%s{background:#fff;border:1px solid #e5e8ee;border-radius:%dpx;padding:20px 22px;margin:34px 0 14px}.%s b{font-size:16px}.%s>p{margin:4px 0 12px;font-size:14px}.%s{display:flex;gap:8px}.%s input{flex:1;min-width:0;height:40px}.%s button{width:auto;height:40px;padding:0 18px;flex:none}.%s{max-width:520px}`, c("subs"), k.rad+4, c("subs"), c("subs"), c("srow"), c("srow"), c("srow"), c("narrow"))
	// 视频站:精选、播放页右栏、会员那一层的按钮
	w(`.%s{position:relative;display:block;aspect-ratio:16/7;border-radius:%dpx;overflow:hidden;background:#151821;margin:0 0 26px;color:#fff}.%s:hover{text-decoration:none}.%s img{position:absolute;inset:0;width:100%%;height:100%%;object-fit:cover;transition:transform .5s}.%s:hover img{transform:scale(1.02)}`, c("lead2"), k.rad+6, c("lead2"), c("lead2"), c("lead2"))
	w(`.%s::after{content:"";position:absolute;inset:0;background:linear-gradient(90deg,rgba(8,10,16,.74),rgba(8,10,16,.28) 55%%,rgba(8,10,16,0) 82%%)}.%s>div{position:absolute;left:34px;bottom:30px;max-width:540px;z-index:1}.%s h2{font-size:32px;line-height:1.15;margin:8px 0;color:#fff;letter-spacing:-.01em}.%s p{margin:0 0 16px;color:rgba(255,255,255,.86);font-size:14px}`, c("lead2"), c("lead2"), c("lead2"), c("lead2"))
	w(`.%s{display:inline-block;font-size:12px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:%s}.%s .%s{color:#fff;opacity:.85}`, c("kick"), acc, c("lead2"), c("kick"))
	w(`.%s{display:grid;grid-template-columns:minmax(0,1fr) 336px;gap:28px;align-items:start}.%s h2{font-size:16px;margin:2px 0 12px}.%s>a{display:grid;grid-template-columns:168px minmax(0,1fr);gap:12px;margin:0 0 14px;color:inherit}.%s>a:hover{text-decoration:none}.%s>a:hover b{color:%s}`, c("watch"), c("up"), c("up"), c("up"), c("up"), acc)
	w(`.%s b{display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden;font-size:14px;line-height:1.35;margin:0 0 4px}.%s .%s{display:block;font-size:12.5px}.%s .%s{border-radius:%dpx}.%s .%s{font-size:11px;padding:0 5px}`, c("up"), c("up"), c("muted"), c("up"), c("thumb"), k.rad, c("up"), c("dur"))
	w(`.%s{display:flex;flex-wrap:wrap;justify-content:center;gap:10px;margin:6px 0 4px}.%s .%s .%s a{opacity:1;text-decoration:none;font-size:14px}.%s{display:inline-block;padding:8px 16px;border-radius:8px;border:1px solid rgba(255,255,255,.45);background:rgba(255,255,255,.14);color:#fff;font-weight:600;font-size:14px}.%s:hover{text-decoration:none;background:rgba(255,255,255,.22)}`, c("acts"), c("gate"), c("msg"), c("acts"), c("btn2"), c("btn2"))
	// 博客:正文一栏居中、头条
	w(`.%s{max-width:720px;margin:0 auto}.%s{display:block;margin:6px 0 30px;color:inherit}.%s:hover{text-decoration:none}.%s:hover h2{color:%s}.%s .%s{aspect-ratio:2/1;margin:0 0 16px}.%s h2{font-size:26px;line-height:1.25;margin:6px 0}.%s p{margin:8px 0 0}`, c("col"), c("top1"), c("top1"), c("top1"), acc, c("top1"), c("pimg"), c("top1"), c("top1"))
	// 软件窗口(下载页的「截图」)
	w(`.%s{background:#fff;color:#1c2330;font-size:13px;line-height:1.45;text-align:left;cursor:default;-webkit-user-select:none;user-select:none}.%s{display:flex;align-items:center;gap:7px;height:38px;padding:0 14px;background:#eceef2;border-bottom:1px solid #e0e3e9}.%s i{width:11px;height:11px;border-radius:50%%;flex:none}.%s span{flex:1;text-align:center;margin-right:52px;color:#5f6878;font-size:12.5px;font-weight:600}`, c("win"), c("wbar"), c("wbar"), c("wbar"))
	w(`.%s{display:grid;grid-template-columns:196px minmax(0,1fr);min-height:430px}.%s{background:#f5f6f9;border-right:1px solid #e7e9ee;padding:14px 10px}.%s b{display:block;margin:4px 8px 10px;font-size:11px;letter-spacing:.06em;text-transform:uppercase;color:#8a93a3;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}`, c("wbody"), c("side"), c("side"))
	w(`.%s>span{display:flex;align-items:center;gap:9px;padding:6px 8px;border-radius:7px;color:#3b4454;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.%s>span.%s{background:%s;color:%s;font-weight:600}.%s>span i{width:14px;height:14px;border-radius:4px;background:currentColor;opacity:.45;flex:none}`, c("side"), c("side"), c("on"), soft, acc, c("side"))
	w(`.%s{padding:14px 18px 16px;min-width:0}.%s{display:flex;align-items:center;gap:10px;margin:0 0 14px}.%s{flex:1;max-width:260px;height:30px;display:flex;align-items:center;padding:0 10px;border:1px solid #e1e4ea;border-radius:7px;background:#f8f9fb;color:#9aa2b1;font-size:12.5px}.%s .%s{margin-left:auto;padding:5px 12px;border-radius:7px;font-size:12.5px}`, c("wmain"), c("tool"), c("srch"), c("tool"), c("btn"))
	w(`.%s{font-size:12px;font-weight:600;color:#8a93a3;margin:2px 0 6px}.%s{display:grid;grid-template-columns:auto minmax(0,1fr) auto auto;align-items:center;gap:12px;padding:8px 6px;border-bottom:1px solid #f0f2f5}.%s:last-child{border-bottom:0}.%s>span:nth-child(2){overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.%s img{width:64px;height:36px;border-radius:5px;object-fit:cover}`, c("lhead"), c("lrow"), c("lrow"), c("lrow"), c("lrow"))
	w(`.%s{width:16px;height:16px;background:%s;-webkit-mask:%s center/contain no-repeat;mask:%s center/contain no-repeat}.%s.%s{-webkit-mask-image:%s;mask-image:%s;opacity:.7}.%s{display:block;width:110px;height:5px;border-radius:3px;background:#e6e9ee;overflow:hidden}.%s i{display:block;height:100%%;background:%s}`, c("ico"), acc, iconDir, iconDir, c("ico"), c("file"), iconFile, iconFile, c("prog"), c("prog"), acc)
	w(`.%s{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:10px}.%s img{width:100%%;aspect-ratio:16/9;object-fit:cover;border-radius:7px}.%s img.%s{outline:3px solid %s;outline-offset:2px}`, c("pgrid"), c("pgrid"), c("pgrid"), c("on"), acc)
	w(`.%s{position:relative;aspect-ratio:16/9;border-radius:8px;overflow:hidden;background:#111;margin:0 0 12px}.%s img{width:100%%;height:100%%;object-fit:cover}.%s{position:absolute;left:0;right:0;bottom:0;display:flex;align-items:center;gap:10px;padding:22px 14px 10px;background:linear-gradient(transparent,rgba(0,0,0,.66));color:#fff;font-size:11.5px}`, c("stage"), c("stage"), c("ctrl"))
	w(`.%s s{width:0;height:0;border-left:9px solid #fff;border-top:6px solid transparent;border-bottom:6px solid transparent}.%s i{flex:1;height:4px;border-radius:2px;background:rgba(255,255,255,.3);overflow:hidden}.%s i b{display:block;height:100%%;background:%s}`, c("ctrl"), c("ctrl"), c("ctrl"), acc)
	w(`.%s{display:grid;grid-template-columns:96px 22px 22px minmax(0,1fr);gap:6px;align-items:center;padding:6px 0;border-bottom:1px solid #f0f2f5}.%s>span:first-child{font-weight:600;font-size:12.5px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.%s{display:grid;place-items:center;height:20px;border-radius:4px;background:#eef0f4;color:#6b7383;font-size:10.5px;font-weight:700}.%s{display:block;width:100%%;height:38px;border-radius:5px}`, c("track"), c("track"), c("ms"), c("wave"))
	w(`.%s{display:grid;grid-template-columns:200px minmax(0,1fr);gap:16px}.%s>div{padding:8px 10px;border-radius:7px;margin:0 0 4px}.%s>div.%s{background:%s}.%s b{display:block;font-size:12.5px}.%s span{display:block;color:#6b7383;font-size:12px}.%s h4{margin:2px 0 8px;font-size:16px}.%s p{margin:0 0 8px;color:#3b4454;font-size:12.5px;line-height:1.6}`, c("notes"), c("nlist"), c("nlist"), c("on"), soft, c("nlist"), c("nlist"), c("ntext"), c("ntext"))
	w(`.%s{width:8px;height:8px;border-radius:50%%;background:#c4c9d2}.%s.%s{background:#2fb26b}.%s{position:relative;border-radius:8px;overflow:hidden;border:1px solid #e5e8ee;margin:0 0 12px}.%s img{width:100%%;aspect-ratio:16/9;object-fit:cover}.%s svg{position:absolute;inset:0;width:100%%;height:100%%}`, c("dot"), c("dot"), c("on"), c("canvas"), c("canvas"), c("canvas"))
	w(`.%s .%s.%s{border-color:%s;box-shadow:0 0 0 3px %s}.%s{display:flex;justify-content:space-between;color:#8a93a3;font-size:12px;margin:4px 2px 0}`, c("dl"), c("card"), c("on"), acc, soft, c("axis"))
	w(`.%s .%s{display:flex;flex-direction:column;align-items:flex-start}.%s .%s .%s{margin-top:auto}`, c("dl"), c("card"), c("dl"), c("card"), c("btn"))
	w(`.%s{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin:20px 0 0;padding:14px 18px;background:#fff;border:1px solid #e5e8ee;border-radius:%dpx}.%s span{font-size:14px}.%s b{display:block;margin:0 0 2px;color:#6b7383;font-size:12px;font-weight:600}`, c("det"), k.rad+4, c("det"), c("det"))
	w(`.%s>span:first-child{display:grid;place-items:center;color:#fff;font-weight:700;font-size:17px}`, c("chan"))
	// 原来写成内联样式的几处
	w(`.%s{font-size:13px}p.%s{font-size:13px;margin:8px 2px}p.%s{margin:-6px 0 6px;font-size:13px}.%s h1{margin-top:18px}.%s .%s{margin:0 0 0 auto}`, c("meta"), c("fine"), c("cap"), c("watch"), c("chan"), c("chip"))
	w(`.%s>div>a{font-weight:600;font-size:17px}.%s p{margin:6px 0 0;font-size:14px}.%s .%s{margin:4px 0 0}.%s{width:96px;height:96px;border-radius:50%%;margin-bottom:14px}`, c("post"), c("post"), c("author"), c("sub"), c("avbig"))
	w(`.%s .%s{margin:0}.%s .%s .%s{font-size:13px;margin:2px 0 12px}`, c("head2"), c("sub"), c("dl"), c("card"), c("muted"))
}

// asset 放一个静态文件(图片),返回它的路径(文件名像打包工具出的带哈希的名字)
func (s *Site) asset(g *gen, ext, ctype string, body []byte) string {
	p := g.sk.dir + g.id(10) + "." + ext
	s.add(p, ctype, body)
	return p
}

// layout 共用的页面外框:顶栏(logo、站名、导航,当前所在的那一项加重)、内容、页脚。path 是这一页的地址。
func layout(g *gen, s *Site, path, title string, nav [][2]string, body string) []byte {
	c := g.c
	cur := ""
	for _, n := range nav { // 地址前缀最长的那一项算当前(首页只认它自己)
		if (n[0] == "/" && path == "/") || (n[0] != "/" && strings.HasPrefix(path, n[0]) && len(n[0]) > len(cur)) {
			cur = n[0]
		}
	}
	// <head>:字符集总在最前,其余几样的先后、Open Graph 带不带、换不换行按种子定。
	// 原来各台的 <head> 除了标题和路径一字不差,「og:type 那一行紧跟着站点图标那一行」这样的一段标记就能搜出全部伪装站(审查 F-H1)
	k := g.sk
	var meta []string
	if s.desc != "" {
		meta = append(meta, `<meta name="description" content="`+esc(s.desc)+`">`)
		switch k.og {
		case 1:
			meta = append(meta, `<meta property="og:site_name" content="`+esc(s.name)+`">`, `<meta property="og:title" content="`+esc(title)+`">`, `<meta property="og:type" content="website">`)
		case 2:
			meta = append(meta, `<meta property="og:title" content="`+esc(title)+`">`, `<meta property="og:description" content="`+esc(s.desc)+`">`)
		case 3:
			meta = append(meta, `<meta property="og:type" content="website">`, `<meta property="og:title" content="`+esc(title)+`">`, `<meta name="twitter:card" content="summary">`)
		}
	}
	vp, tt := `<meta name="viewport" content="`+k.viewport+`">`, `<title>`+esc(title)+`</title>`
	icon, css := fmt.Sprintf(k.iconTag, s.icon), `<link rel="stylesheet" href="`+s.css+`">`
	var head []string
	switch k.headOrd {
	case 0:
		head = append(append([]string{k.charset, vp, tt}, meta...), icon, css)
	case 1:
		head = append(append([]string{k.charset, tt, vp}, meta...), css, icon)
	case 2:
		head = append(append([]string{k.charset, vp}, meta...), tt, icon, css)
	default:
		head = append([]string{k.charset, vp, tt, css, icon}, meta...)
	}
	if s.head != "" {
		head = append(head, s.head)
	}
	if s.js != "" {
		head = append(head, `<script src="`+s.js+`" defer></script>`)
	}
	nl := k.nl
	var b strings.Builder
	b.WriteString(k.doctype + nl + `<html lang="` + k.lang + `">` + nl + `<head>` + nl)
	for _, h := range head {
		if nl != "" {
			b.WriteString(k.ind)
		}
		b.WriteString(h + nl)
	}
	b.WriteString(`</head>` + nl + `<body>` + nl)
	fmt.Fprintf(&b, `<header><div class="%s"><a class="%s" href="/">%s%s</a><nav>`, c("wrap"), c("name"), s.logo, esc(s.name))
	for _, n := range nav {
		if n[0] == cur {
			fmt.Fprintf(&b, `<a class="%s" href="%s">%s</a>`, c("on"), n[0], esc(n[1]))
		} else {
			fmt.Fprintf(&b, `<a href="%s">%s</a>`, n[0], esc(n[1]))
		}
	}
	fmt.Fprintf(&b, `</nav>%s</div></header>%s<main class="%s">%s</main>%s<footer><div class="%s"><span>%s</span>%s</div></footer>%s</body>%s</html>%s`,
		s.cta, nl, c("wrap"), body, nl, c("wrap"), esc(fmt.Sprintf(k.copyFmt, g.base.Year(), s.name)), s.foot, nl, nl, nl)
	return []byte(b.String())
}

// picSize 一个封面位要的一个尺寸;pw / ph 不为 0 时,另要一张设置页预览里内联的小图(首页上的图)
type picSize struct{ w, h, pw, ph int }

// picture 一个封面位:按场景和天色从图包里挑一张(kind 可以写成「场景:天色」),见 pictureOf。
func (s *Site) picture(g *gen, slot int, kind, tod string, sizes ...picSize) []string {
	pk, pt := kind, tod
	if k, t, ok := strings.Cut(kind, ":"); ok { // 「场景:天色」的写法(博客题图)
		pk = k
		if pt == "" {
			pt = t
		}
	}
	return s.pictureOf(g, slot, photoFor(g.seed, slot, pk, pt, s.used), kind, tod, sizes...)
}

// pictureOf 一个封面位用图包里的 name(空 = 没有这一类照片):几个尺寸都处理好了就用照片(每个尺寸一份 JPEG),
// 否则画插图(一份 SVG,几个尺寸共用),用到的照片记进 used、缺的尺寸记进 wanted。
// 文件名不管用不用照片都先取、插图用自己的随机数:有没有照片,站上别的都不变。
func (s *Site) pictureOf(g *gen, slot int, name, kind, tod string, sizes ...picSize) []string {
	stem := g.id(10)
	out := make([]string, len(sizes))
	if name != "" {
		s.used[name] = true
		var miss []PhotoReq
		get := func(w, h int) []byte {
			var b []byte
			if s.photos != nil {
				b = s.photos.Photo(name, w, h)
			}
			if b == nil {
				miss = append(miss, PhotoReq{Name: name, W: w, H: h})
			}
			return b
		}
		full, small := make([][]byte, len(sizes)), make([][]byte, len(sizes))
		for i, sz := range sizes {
			full[i] = get(sz.w, sz.h)
			if sz.pw > 0 {
				small[i] = get(sz.pw, sz.ph)
			}
		}
		if len(miss) == 0 {
			for i, sz := range sizes {
				p := g.sk.dir + fmt.Sprintf(g.sk.imgName, stem, sz.w)
				s.add(p, jpegType, full[i])
				if small[i] != nil {
					s.small[p] = small[i]
				}
				out[i] = p
			}
			return out
		}
		s.wanted = append(s.wanted, miss...)
	}
	p := g.sk.dir + stem + ".svg"
	s.add(p, svgType, cover(g.fork(slot), kind, tod, float64(sizes[0].w), float64(sizes[0].h)))
	for i := range out {
		out[i] = p
	}
	return out
}

// notFoundBody 404 页的正文。各台的说法不一样:原来四种站、每一台都是同一段话,扫描器对着随便一个不存在的路径
// 拿到的就是它(审查 F-H1)。nfTitle 是这一页的标题(浏览器标签上的)。
func notFoundBody(g *gen) string {
	head := g.say("Page not found", "Not found", "This page doesn't exist", "Nothing here", "We can't find that page", "That page is gone", "No such page")
	text := g.say("The page you were looking for doesn't exist or has moved.", "The link may be broken, or the page may have been {removed|taken down}.",
		"There's nothing at this address. Check the URL and try again.", "Sorry, we couldn't find what you were looking for.",
		"That address doesn't match anything on this site.", "It may have been moved or deleted, or the address was mistyped.",
		"The page you requested could not be found on this server.")
	back := g.say("Back to the home page", "Go to the home page", "Return home", "Start again from the home page", "Take me home", "Home page", "Go back home")
	return `<div class="` + g.c("e404") + `"><b>404</b><h1>` + esc(head) + `</h1><p class="` + g.c("sub") + `">` + esc(text) + `</p><p><a href="/">` + esc(back) + `</a></p></div>`
}

// nfTitle 404 页的标题
func nfTitle(g *gen, site string) string {
	return g.title(g.pick([]string{"Not found", "Page not found", "404", "Error 404", "404 Not Found"}), site)
}

const htmlType = "text/html; charset=utf-8"
const textType = "text/plain; charset=utf-8"
const svgType = "image/svg+xml"

// slugify 文章标题 → 地址里的那一段
func slugify(t string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(t) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// ---- 大小的写法 ----

func size(gb int) string {
	if gb >= 1024 {
		return fmt.Sprintf("%.1f TB", float64(gb)/1024)
	}
	return fmt.Sprintf("%d GB", gb)
}
func mb(n int) string { return fmt.Sprintf("%d MB", n) }

// humanSize 目录列表里小文件的大小(和常见的目录列表一样:不到 1K 写字节数)
func humanSize(n int) string {
	if n < 1024 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%.1fK", float64(n)/1024)
}

// favicon 站点图标:强调色的小方块(个人站是圆的),中间可能有一个小记号。
// 圆角、深浅、记号都按种子变:原来只有「色相 × 方圆」七百多种,把这七百多个图标的哈希列出来就能按图标搜出全部伪装站(审查 F-H1)
func favicon(g *gen, round bool) []byte {
	rx := g.n(5, 11)
	if round {
		rx = 16
	}
	fill := hslHex(float64(g.hue+g.n(-5, 5)), float64(g.n(52, 72)), float64(g.n(38, 50)))
	mark := ""
	switch g.r.IntN(5) {
	case 0:
		mark = fmt.Sprintf(`<circle cx="16" cy="16" r="%d" fill="#fff"%s/>`, g.n(4, 7), op(.8+.2*g.r.Float64()))
	case 1:
		a := g.n(9, 12)
		mark = fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="%d" rx="%d" fill="#fff"%s/>`, a, a, 32-2*a, 32-2*a, g.n(1, 3), op(.8+.2*g.r.Float64()))
	case 2:
		mark = fmt.Sprintf(`<path d="M%d %dL%d %d" stroke="#fff" stroke-width="%d" stroke-linecap="round"/>`, g.n(9, 12), g.n(20, 23), g.n(20, 23), g.n(9, 12), g.n(3, 5))
	}
	return []byte(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="%d" fill="%s"/>%s</svg>`, rx, fill, mark))
}
