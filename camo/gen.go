package camo

import (
	"fmt"
	"html"
	"math/rand/v2"
	"net/http"
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
	return g.base.Add(-time.Duration(d) * 24 * time.Hour).Format("Jan 2, 2006")
}

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

var words = []string{"Lumen", "Northgate", "Cedar", "Harbor", "Quarry", "Juniper", "Meridian", "Granite", "Kestrel", "Tidal",
	"Copper", "Orchard", "Summit", "Vesper", "Alder", "Basalt", "Fernway", "Halcyon", "Larch", "Pinecrest", "Riverside", "Saltmarsh"}

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
	style, pre := g.r.IntN(3), g.id(2)
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
		errStyle: g.r.IntN(3), camel: g.r.IntN(2) == 0}
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
	w(`.%s{width:38px;height:38px;border-radius:10px;background:%s;display:grid;place-items:center;margin-bottom:12px}.%s svg{width:20px;height:20px}`, c("ficon"), soft, c("ficon"))
	g.cssMore(w, acc, soft)
	w(`@media(max-width:980px){.%s{grid-template-columns:1fr}.%s>a{grid-template-columns:150px minmax(0,1fr)}}`, c("watch"), c("up"))
	w(`@media(max-width:640px){nav{gap:14px}h1{font-size:23px}.%s{display:none}.%s{grid-template-columns:1fr}.%s b{font-size:64px}`, c("hide"), c("post"), c("e404"))
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
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>`, esc(title))
	if s.desc != "" {
		fmt.Fprintf(&b, `<meta name="description" content="%s"><meta property="og:site_name" content="%s"><meta property="og:title" content="%s"><meta property="og:type" content="website">`, esc(s.desc), esc(s.name), esc(title))
	}
	fmt.Fprintf(&b, `<link rel="icon" href="%s"><link rel="stylesheet" href="%s">%s`, s.icon, s.css, s.head)
	if s.js != "" {
		fmt.Fprintf(&b, `<script src="%s" defer></script>`, s.js)
	}
	fmt.Fprintf(&b, `</head><body><header><div class="%s"><a class="%s" href="/">%s%s</a><nav>`, c("wrap"), c("name"), s.logo, esc(s.name))
	for _, n := range nav {
		if n[0] == cur {
			fmt.Fprintf(&b, `<a class="%s" href="%s">%s</a>`, c("on"), n[0], esc(n[1]))
		} else {
			fmt.Fprintf(&b, `<a href="%s">%s</a>`, n[0], esc(n[1]))
		}
	}
	fmt.Fprintf(&b, `</nav>%s</div></header><main class="%s">%s</main><footer><div class="%s"><span>© %d %s</span>%s</div></footer></body></html>`,
		s.cta, c("wrap"), body, c("wrap"), g.base.Year(), esc(s.name), s.foot)
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

func notFoundBody(g *gen) string {
	return `<div class="` + g.c("e404") + `"><b>404</b><h1>Page not found</h1><p class="` + g.c("sub") + `">The page you were looking for doesn't exist or has moved.</p><p><a href="/">Back to the home page</a></p></div>`
}

const htmlType = "text/html; charset=utf-8"
const textType = "text/plain; charset=utf-8"
const svgType = "image/svg+xml"

// ---- 下载镜像 ----

var mirrorProjects = [][2]string{
	{"almalinux", "AlmaLinux OS"}, {"alpine", "Alpine Linux"}, {"archlinux", "Arch Linux packages and images"}, {"centos-stream", "CentOS Stream"},
	{"debian", "Debian archive"}, {"debian-cd", "Debian installer images"}, {"fedora", "Fedora releases"}, {"freebsd", "FreeBSD releases"},
	{"gentoo", "Gentoo distfiles"}, {"kali", "Kali Linux"}, {"linuxmint", "Linux Mint images"}, {"manjaro", "Manjaro packages"},
	{"opensuse", "openSUSE distributions"}, {"openwrt", "OpenWrt firmware"}, {"raspberrypi", "Raspberry Pi OS images"}, {"rocky", "Rocky Linux"},
	{"ubuntu", "Ubuntu archive"}, {"ubuntu-releases", "Ubuntu release images"},
}

var mirrorDirs = []string{"dists", "iso", "images", "pool", "releases", "snapshots", "updates"}

// mirrorDirFiles 每个子目录里列出(也真能打开)的几个小文件
func mirrorDirFiles(d string) []string {
	switch d {
	case "dists":
		return []string{"InRelease", "Release", "Release.gpg"}
	case "pool":
		return []string{"README", "SHA256SUMS"}
	}
	return []string{"SHA256SUMS", "SHA256SUMS.sig"}
}

func genMirror(s *Site, g *gen) {
	s.name = g.pick(words) + " " + g.pick([]string{"Mirror", "Open Mirror", "Software Mirror", "Mirrors"})
	s.logo = `<span class="` + g.c("logo") + `"></span>`
	every := g.pick([]string{"4", "6", "8", "12"})
	s.foot = `<span>Synchronized every ` + every + ` hours from the official upstream servers</span>`
	nav := [][2]string{{"/", "Projects"}, {"/about/", "About"}}
	var names []string
	desc := map[string]string{}
	for _, p := range mirrorProjects {
		names = append(names, p[0])
		desc[p[0]] = p[1]
	}
	r, mono, dirc, filec := g.c("r"), g.c("mono"), g.c("dir"), g.c("file")
	s.desc = s.name + ": open-source software mirror, synchronized with the official upstream servers every " + every + " hours."
	everyN, _ := strconv.Atoi(every)
	statusPath := g.pick([]string{"/status.json", "/api/status", "/mirrors.json", "/api/v1/status"})
	if strings.HasPrefix(statusPath, "/api/") {
		s.apiPre = statusPath[:strings.LastIndex(statusPath, "/")+1]
	}
	chosen := g.pickN(names, g.n(9, 14))
	syncing := g.r.IntN(len(chosen)) // 有一个项目正在同步
	var rows strings.Builder
	var projects []map[string]any
	total := 0
	for i, name := range chosen {
		gb := g.n(18, 2400)
		total += gb
		ago := g.n(8, everyN*60-12) // 分钟:同步间隔以内
		status := fmt.Sprintf(`<span class="%s">Synced %dh ago</span>`, g.c("ok"), ago/60)
		if ago < 60 {
			status = fmt.Sprintf(`<span class="%s">Synced %d min ago</span>`, g.c("ok"), ago)
		}
		state := "synced"
		if i == syncing {
			status = fmt.Sprintf(`<span class="%s">Syncing</span>`, g.c("busy"))
			state = "syncing"
		}
		projects = append(projects, map[string]any{"name": name, "status": state, g.jsonKey("last_sync"): g.base.Add(-time.Duration(ago) * time.Minute).UTC().Format(time.RFC3339),
			g.jsonKey("size_bytes"): int64(gb) << 30, "path": "/" + name + "/"})
		fmt.Fprintf(&rows, `<tr><td><a class="%s" href="/%s/">%s/</a></td><td class="%s %s">%s</td><td class="%s %s">%s</td><td class="%s">%s</td></tr>`,
			dirc, name, name, g.c("hide"), g.c("muted"), esc(desc[name]), r, mono, size(gb), r, status)
		ver := fmt.Sprintf("%d.%d", g.n(3, 42), g.n(0, 9))
		var list strings.Builder
		for _, d := range g.pickN(mirrorDirs, g.n(2, 4)) {
			date := g.daysAgo(g.n(0, 9))
			fmt.Fprintf(&list, `<tr><td><a class="%s" href="/%s/%s/">%s/</a></td><td class="%s %s">-</td><td class="%s %s">%s</td></tr>`, dirc, name, d, d, r, mono, r, mono, date)
			var sub strings.Builder
			for _, fn := range mirrorDirFiles(d) {
				body, ctype := mirrorFile(g, name, desc[name], ver, d, fn)
				p := "/" + name + "/" + d + "/" + fn
				s.add(p, ctype, body)
				fmt.Fprintf(&sub, `<tr><td><a class="%s" href="%s">%s</a></td><td class="%s %s">%s</td><td class="%s %s">%s</td></tr>`, filec, p, fn, r, mono, humanSize(len(body)), r, mono, date)
			}
			dp := "/" + name + "/" + d + "/"
			s.add(dp, htmlType, layout(g, s, dp, "Index of "+dp+" - "+s.name, nav, indexBody(g, dp, "/"+name+"/", desc[name], sub.String())))
		}
		for _, fn := range []string{"README", "SHA256SUMS", "SHA256SUMS.sig"} {
			body, ctype := mirrorFile(g, name, desc[name], ver, "", fn)
			p := "/" + name + "/" + fn
			s.add(p, ctype, body)
			fmt.Fprintf(&list, `<tr><td><a class="%s" href="%s">%s</a></td><td class="%s %s">%s</td><td class="%s %s">%s</td></tr>`, filec, p, fn, r, mono, humanSize(len(body)), r, mono, g.daysAgo(g.n(1, 30)))
		}
		pp := "/" + name + "/"
		s.add(pp, htmlType, layout(g, s, pp, "Index of "+pp+" - "+s.name, nav, indexBody(g, pp, "/", desc[name], list.String())))
	}
	chart, peak, avg := trafficChart(g, 960, 150)
	chartImg := s.asset(g, "svg", svgType, chart)
	var axis strings.Builder
	for i := 0; i < 5; i++ {
		d := 29 - (i*29+2)/4
		label := g.base.Add(-time.Duration(d) * 24 * time.Hour).Format("Jan 2")
		if d == 0 {
			label = "Today"
		}
		fmt.Fprintf(&axis, `<span>%s</span>`, label)
	}
	s.getJSON(statusPath, http.StatusOK, map[string]any{"name": s.name, g.jsonKey("updated_at"): g.base.UTC().Format(time.RFC3339), g.jsonKey("sync_interval_hours"): everyN, "projects": projects})
	stat := func(v, label string) string {
		return fmt.Sprintf(`<div class="%s"><b>%s</b><span>%s</span></div>`, g.c("stat"), v, label)
	}
	body := fmt.Sprintf(`<h1>%s</h1><p class="%s">Open-source software mirror, synchronized with upstream every %s hours.</p>`+
		`<div class="%s">%s%s%s%s</div>`+
		`<div class="%s"><table><thead><tr><th>Project</th><th class="%s">Description</th><th class="%s">Size</th><th class="%s">Status</th></tr></thead><tbody>%s</tbody></table></div>`+
		`<p class="%s" style="font-size:13px;margin:8px 2px 0">Machine-readable status: <a href="%s">%s</a></p>`+
		`<h2>Traffic, last 30 days</h2><p class="%s" style="margin:-6px 0 6px;font-size:13px">Peak %.1f TB / day · average %.1f TB / day</p><div class="%s"><img src="%s" alt="" width="960" height="150"><div class="%s">%s</div></div>`,
		esc(s.name), g.c("sub"), every, g.c("stats"),
		stat(strconv.Itoa(len(chosen)), "projects"), stat(size(total), "total size"), stat(g.pick([]string{"10 Gbps", "20 Gbps", "25 Gbps"}), "uplink"), stat("IPv4 / IPv6", "HTTP and HTTPS"),
		g.c("card"), g.c("hide"), r, r, rows.String(), g.c("muted"), statusPath, statusPath, g.c("muted"), peak, avg, g.c("chart"), chartImg, g.c("axis"), axis.String())
	s.add("/", htmlType, layout(g, s, "/", s.name, nav, body))
	s.add("/about/", htmlType, layout(g, s, "/about/", "About - "+s.name, nav,
		`<h1>About this mirror</h1><p class="`+g.c("sub")+`">A public mirror for open-source projects.</p>`+mirrorAbout(g)))
	s.setNotFound(layout(g, s, "", "Not found - "+s.name, nav, notFoundBody(g)))
}

// indexBody 目录页:面包屑、标题、文件列表
func indexBody(g *gen, path, parent, desc, rows string) string {
	var crumbs strings.Builder
	crumbs.WriteString(`<a href="/">Projects</a>`)
	acc := "/"
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		acc += part + "/"
		if acc == path {
			fmt.Fprintf(&crumbs, ` / %s`, esc(part))
		} else {
			fmt.Fprintf(&crumbs, ` / <a href="%s">%s</a>`, acc, esc(part))
		}
	}
	return fmt.Sprintf(`<p class="%s">%s</p><h1>Index of %s</h1><p class="%s">%s</p><div class="%s"><table><thead><tr><th>Name</th><th class="%s">Size</th><th class="%s">Last modified</th></tr></thead><tbody><tr><td><a class="%s" href="%s">../</a></td><td></td><td></td></tr>%s</tbody></table></div>`,
		g.c("crumbs"), crumbs.String(), esc(path), g.c("sub"), esc(desc), g.c("card"), g.c("r"), g.c("r"), g.c("dir"), parent, rows)
}

// mirrorFile 镜像里那几个小文件的内容:说明、校验和、签名、Release —— 和真镜像上的格式一样
func mirrorFile(g *gen, proj, desc, ver, dir, name string) ([]byte, string) {
	arch := []string{"amd64", "arm64"}
	ext := map[string]string{"iso": "iso", "images": "img.xz", "releases": "tar.xz", "snapshots": "tar.zst", "updates": "tar.gz", "pool": "deb"}[dir]
	if ext == "" {
		ext = "iso"
	}
	sig := func() string {
		var b strings.Builder
		b.WriteString("-----BEGIN PGP SIGNATURE-----\n\n")
		for i := 0; i < 5; i++ {
			b.WriteString(g.b64(64) + "\n")
		}
		b.WriteString(g.b64(28) + "\n=" + g.b64(4) + "\n-----END PGP SIGNATURE-----\n")
		return b.String()
	}
	switch name {
	case "README":
		return []byte(fmt.Sprintf("%s\n%s\n\nThis directory is synchronized from the official %s servers several times a day.\n"+
			"Files are served exactly as published upstream. Please verify what you download with the\nchecksums and signatures that come with each release.\n\nLast sync: %s UTC\n",
			desc, strings.Repeat("=", len(desc)), desc, g.daysAgo(g.n(0, 2)))), textType
	case "SHA256SUMS":
		var b strings.Builder
		for _, a := range arch {
			for _, kind := range []string{"", "-minimal"} {
				fmt.Fprintf(&b, "%s  %s-%s%s-%s.%s\n", g.hex(64), proj, ver, kind, a, ext)
			}
		}
		return []byte(b.String()), textType
	case "SHA256SUMS.sig", "Release.gpg":
		return []byte(sig()), "application/pgp-signature"
	}
	// Release / InRelease
	var b strings.Builder
	codename := strings.ToLower(g.pick(words))
	fmt.Fprintf(&b, "Origin: %s\nLabel: %s\nSuite: stable\nCodename: %s\nDate: %s\nArchitectures: amd64 arm64\nComponents: main contrib\nDescription: %s %s\nSHA256:\n",
		desc, desc, codename, g.base.Add(-time.Duration(g.n(1, 40))*time.Hour).Format(time.RFC1123), desc, ver)
	for _, comp := range []string{"main", "contrib"} {
		for _, a := range arch {
			fmt.Fprintf(&b, " %s %8d %s/binary-%s/Packages\n %s %8d %s/binary-%s/Packages.xz\n", g.hex(64), g.n(200000, 9000000), comp, a, g.hex(64), g.n(40000, 1500000), comp, a)
		}
	}
	if name == "InRelease" {
		return []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\n" + b.String() + sig()), textType
	}
	return []byte(b.String()), textType
}

// ---- 软件下载 ----

// featureIcons 功能介绍里的小图标(描边,跟着强调色),和 featureTexts 一一对应
var featureIcons = []string{
	`<path d="M3 12h4l3-8 4 16 3-8h4"/>`,
	`<rect x="4" y="4" width="16" height="16" rx="3"/><path d="M4 10h16M10 10v10"/>`,
	`<path d="M12 3l8 4v5c0 5-3.5 8-8 9-4.5-1-8-4-8-9V7z"/>`,
	`<path d="M4 17l5-5 4 4 7-8"/><path d="M14 8h6v6"/>`,
	`<circle cx="12" cy="12" r="8"/><path d="M12 7v5l3 2"/>`,
	`<path d="M7 18a4 4 0 0 1-.4-8A6 6 0 0 1 18 8.5 4.5 4.5 0 0 1 17.5 18z"/>`,
}

func genDownloads(s *Site, g *gen) {
	kind := g.pick([]string{"Studio", "Sync", "Desk", "Capture", "Notes", "Player", "Draw"})
	product := g.pick(words) + " " + kind
	s.name = product
	maker := g.pick(words) + " Labs"
	s.foot = `<span>Made by ` + esc(maker) + `</span>`
	icon := s.asset(g, "svg", svgType, appIcon(g, kind))
	s.apiPre = g.sk.api
	s.desc = product + g.pick([]string{" for Windows, macOS and Linux. Free for personal use.", " is a small, fast app for Windows, macOS and Linux.", ": download the latest version for your computer."})
	s.addScript(g, scriptParts{forms: true, os: true, latest: "releases/latest", formKey: g.sk.formKey, osKey: g.sk.osKey})
	mock := appMock(s, g, kind, product)
	s.logo = fmt.Sprintf(`<img class="%s" src="%s" alt="" width="26" height="26">`, g.c("logo"), icon)
	nav := [][2]string{{"/", "Download"}, {"/releases/", "Releases"}, {"/about/", "About"}}
	major, minor := g.n(2, 6), g.n(0, 9)
	type rel struct {
		ver, date string
		at        time.Time
	}
	var rels []rel
	day := g.n(3, 20)
	for i := 0; i < 6; i++ {
		at := g.base.Add(-time.Duration(day) * 24 * time.Hour)
		rels = append(rels, rel{fmt.Sprintf("%d.%d.%d", major, minor, 6-i), at.Format("Jan 2, 2006"), at})
		day += g.n(12, 40)
	}
	slug := strings.ToLower(strings.ReplaceAll(product, " ", "-"))
	notes := []string{"Faster startup on large libraries", "Improved memory usage when syncing many files", "Fixed a crash when opening very large projects",
		"Updated translations", "New keyboard shortcuts for common actions", "Better handling of slow network connections", "Smoother scrolling on high-refresh displays",
		"Fixed export of files with long names", "Security and stability improvements"}
	oses := [][2]string{{"win", "Windows"}, {"mac", "macOS"}, {"linux", "Linux"}}
	type file struct {
		name, size, sha, os, key string
		mb                       int
	}
	files := func(ver string) []file {
		var out []file
		for _, o := range oses {
			f := file{os: o[1], key: o[0], sha: g.hex(64)}
			switch o[0] {
			case "win":
				f.name, f.mb = fmt.Sprintf("%s-%s-x64-setup.exe", slug, ver), g.n(90, 260)
			case "mac":
				f.name, f.mb = fmt.Sprintf("%s-%s-universal.dmg", slug, ver), g.n(110, 320)
			default:
				f.name, f.mb = fmt.Sprintf("%s-%s-x86_64.AppImage", slug, ver), g.n(95, 280)
			}
			f.size = mb(f.mb)
			out = append(out, f)
		}
		return out
	}
	r, mono, sub := g.c("r"), g.c("mono"), g.c("sub")
	var cards, relList strings.Builder
	var latest []map[string]any
	var history []map[string]any
	jk := g.jsonKey
	for i, rl := range rels {
		fmt.Fprintf(&relList, `<tr><td><a href="/releases/%s/">%s %s</a></td><td class="%s %s">%s</td></tr>`, rl.ver, esc(product), rl.ver, r, g.c("muted"), rl.date)
		var rows, changes strings.Builder
		for _, f := range files(rl.ver) {
			p := "/releases/" + rl.ver + "/" + f.name
			fmt.Fprintf(&rows, `<tr><td><a class="%s" href="%s">%s</a></td><td class="%s">%s</td><td class="%s %s %s">%s…</td></tr>`, g.c("file"), p, esc(f.name), r, f.size, g.c("hide"), mono, g.c("muted"), f.sha[:16])
			// 点了下载:一页「谢谢下载」(下载链接本身是给浏览器的,这里不放安装包)
			s.add(p, htmlType, layout(g, s, p, "Downloading "+f.name, nav,
				fmt.Sprintf(`<div class="%s"><img src="%s" alt="" width="64" height="64"><div><h1>Thanks for downloading %s</h1><p class="%s" style="margin:0">%s · version %s · %s</p></div></div><p>Your download should begin in a few seconds. If it doesn't, check your connection and <a href="/releases/%s/">start it again from the release page</a>.</p><p class="%s">SHA-256: <span class="%s">%s</span></p>`,
					g.c("head2"), icon, esc(product), sub, f.os, rl.ver, f.size, rl.ver, g.c("muted"), mono, f.sha)))
			if i == 0 {
				fmt.Fprintf(&cards, `<div class="%s" data-%s="%s"><b>%s</b><div class="%s" style="font-size:13px;margin:2px 0 12px">%s · %s</div><a class="%s" href="%s">Download</a></div>`,
					g.c("card"), g.sk.osKey, f.key, f.os, g.c("muted"), esc(f.name), f.size, g.c("btn"), p)
				latest = append(latest, map[string]any{"platform": f.key, jk("file_name"): f.name, jk("size_bytes"): f.mb<<20 + g.n(0, 1<<20), "sha256": f.sha, "url": p})
			}
		}
		history = append(history, map[string]any{"version": rl.ver, jk("released_at"): rl.at.UTC().Format(time.RFC3339), "url": "/releases/" + rl.ver + "/"})
		for _, n := range g.pickN(notes, g.n(2, 4)) {
			fmt.Fprintf(&changes, `<li>%s</li>`, esc(n))
		}
		rp := "/releases/" + rl.ver + "/"
		s.add(rp, htmlType, layout(g, s, rp, product+" "+rl.ver, nav,
			fmt.Sprintf(`<h1>%s %s</h1><p class="%s">Released %s</p><h2>What's new</h2><ul>%s</ul><h2>Files</h2><div class="%s"><table><thead><tr><th>File</th><th class="%s">Size</th><th class="%s">SHA-256</th></tr></thead><tbody>%s</tbody></table></div>`,
				esc(product), rl.ver, sub, rl.date, changes.String(), g.c("card"), r, g.c("hide"), rows.String())))
	}
	var feats strings.Builder
	for _, i := range g.r.Perm(len(featureTexts))[:3] {
		fmt.Fprintf(&feats, `<div class="%s"><div class="%s"><svg viewBox="0 0 24 24" fill="none" stroke="%s" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">%s</svg></div><h3>%s</h3><p>%s</p></div>`,
			g.c("feat"), g.c("ficon"), g.accent(), featureIcons[i], esc(g.alt(featureTexts[i][0])), esc(g.alt(featureTexts[i][1])))
	}
	subNote := g.pick([]string{"One email when a new version is out. No spam.", "Get the release notes by email when we ship an update.", "We'll email you when a new version is available, about once a month."})
	subForm := subscribePage(g, s, nav, "subscribe", "Release notes by email", subNote)
	s.subscribeAPI(g, s.apiPre+"subscribe", subForm)
	s.getJSON(s.apiPre+"releases/latest", http.StatusOK, map[string]any{"name": product, "version": rels[0].ver, jk("released_at"): rels[0].at.UTC().Format(time.RFC3339), "files": latest})
	s.getJSON(s.apiPre+"releases", http.StatusOK, map[string]any{"data": history})
	s.add("/", htmlType, layout(g, s, "/", "Download "+product, nav,
		fmt.Sprintf(`<div class="%s"><img src="%s" alt="" width="64" height="64"><div><h1>Download %s</h1><p class="%s" style="margin:0">Version %s · released %s · free for personal use</p></div></div>`+
			`<div class="%s">%s</div>%s<div class="%s">%s</div>%s<h2>Release history</h2><div class="%s"><table><tbody>%s</tbody></table></div>`,
			g.c("head2"), icon, esc(product), sub, rels[0].ver, rels[0].date, g.c("dl"), cards.String(), mock, g.c("feats"), feats.String(),
			subscribeBox(g, s, "subscribe", "Release notes by email", subNote), g.c("card"), relList.String())))
	s.add("/releases/", htmlType, layout(g, s, "/releases/", "Releases - "+product, nav,
		fmt.Sprintf(`<h1>Releases</h1><p class="%s">All versions of %s.</p><div class="%s"><table><tbody>%s</tbody></table></div>`, sub, esc(product), g.c("card"), relList.String())))
	s.add("/about/", htmlType, layout(g, s, "/about/", "About - "+product, nav,
		fmt.Sprintf(`<h1>About %s</h1><p class="%s">Made by %s.</p>%s`, esc(product), sub, esc(maker), productAbout(g, esc(product)))))
	s.setNotFound(layout(g, s, "", "Not found - "+product, nav, notFoundBody(g)))
}

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

// favicon 站点图标:强调色的小方块(个人站是圆的)
func favicon(g *gen, round bool) []byte {
	rx := "8"
	if round {
		rx = "16"
	}
	return []byte(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="%s" fill="%s"/></svg>`, rx, g.accent()))
}
