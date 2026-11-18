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
}

// newGen 日期的基准是 now 所在那一周的周一(UTC)再往前 0~2 天:页面上「最近同步」「发布于」这些日期一直是最近的,
// 内容每周最多变一次(固定成某一天的话,过几个月就像一个没人管的站)。
func newGen(seed uint64, now time.Time) *gen {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	base := WeekOf(now).Add(-time.Duration(r.IntN(3)) * 24 * time.Hour)
	g := &gen{r: r, hue: r.IntN(360), base: base}
	g.sk = newSkin(g)
	return g
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
	navLeft bool // 导航紧跟站名(否则靠右)
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
		font: g.pick(fonts), bg: g.pick([]string{"#f7f8fa", "#fafaf8", "#f6f7fb", "#ffffff", "#f8f7f4"}), navLeft: g.r.IntN(4) == 0}
}

// c 逻辑类名 → 这个种子下的实际类名
func (g *gen) c(k string) string { return g.sk.cls[k] }

// css 整站共用的样式表(单独一个文件,浏览器缓存;每页只有内容)
func (g *gen) css() []byte {
	c, k := g.c, g.sk
	acc, soft := g.accent(), g.accentSoft()
	nav := "margin-left:auto"
	if k.navLeft {
		nav = "margin-left:22px"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `*{box-sizing:border-box}body{margin:0;font:%dpx/1.6 %s;color:#1c2330;background:%s}a{color:%s;text-decoration:none}a:hover{text-decoration:underline}`, k.fs, k.font, k.bg, acc)
	fmt.Fprintf(&b, `img{display:block;max-width:100%%}.%s{max-width:%dpx;margin:0 auto;padding:0 22px}`, c("wrap"), k.maxW)
	fmt.Fprintf(&b, `header{background:#fff;border-bottom:1px solid #e5e8ee}header .%s{display:flex;align-items:center;gap:12px;height:%dpx}`, c("wrap"), k.head)
	fmt.Fprintf(&b, `.%s{display:flex;align-items:center;gap:10px;font-weight:650;font-size:16px;color:#1c2330}.%s:hover{text-decoration:none}`, c("name"), c("name"))
	fmt.Fprintf(&b, `.%s{width:26px;height:26px;border-radius:%dpx;background:%s;flex:none;object-fit:cover}.%s{border-radius:50%%}`, c("logo"), k.rad, acc, c("av"))
	fmt.Fprintf(&b, `nav{%s;display:flex;gap:20px;font-size:14px}nav a{color:#5f6878}main.%s{padding-top:34px;padding-bottom:10px}`, nav, c("wrap"))
	b.WriteString(`h1{font-size:28px;line-height:1.25;margin:0 0 8px;letter-spacing:-.01em}h2{font-size:19px;margin:30px 0 12px}`)
	fmt.Fprintf(&b, `.%s{color:#5f6878;margin:0 0 22px}.%s{background:#fff;border:1px solid #e5e8ee;border-radius:%dpx;overflow:hidden}`, c("sub"), c("card"), k.rad+4)
	b.WriteString(`table{width:100%;border-collapse:collapse;font-size:14px}th,td{text-align:left;padding:10px 14px;border-bottom:1px solid #e5e8ee}th{font-weight:600;color:#5f6878;font-size:12.5px;background:#fbfbfc}tr:last-child td{border-bottom:0}`)
	fmt.Fprintf(&b, `td.%s,th.%s{text-align:right}.%s{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px}`, c("r"), c("r"), c("mono"))
	fmt.Fprintf(&b, `.%s{display:inline-block;padding:3px 10px;border-radius:999px;background:%s;color:%s;font-size:12.5px;font-weight:600;margin:0 6px 6px 0}.%s{color:#5f6878}`, c("chip"), soft, acc, c("muted"))
	b.WriteString(`footer{color:#5f6878;font-size:13px;padding:34px 0 46px}`)
	// 视频
	fmt.Fprintf(&b, `.%s{display:grid;grid-template-columns:repeat(auto-fill,minmax(230px,1fr));gap:22px 18px}.%s a{color:inherit}.%s a:hover{text-decoration:none}.%s a:hover .%s{color:%s}`, c("grid"), c("grid"), c("grid"), c("grid"), c("vt"), acc)
	fmt.Fprintf(&b, `.%s{position:relative;aspect-ratio:16/9;border-radius:%dpx;overflow:hidden;background:#dfe3ea}.%s img{width:100%%;height:100%%;object-fit:cover}`, c("thumb"), k.rad+4, c("thumb"))
	fmt.Fprintf(&b, `.%s{position:absolute;right:8px;bottom:8px;background:rgba(0,0,0,.68);color:#fff;font-size:12px;padding:1px 6px;border-radius:5px}.%s{font-weight:600;margin-top:9px;line-height:1.35}`, c("dur"), c("vt"))
	fmt.Fprintf(&b, `.%s{position:relative;aspect-ratio:16/9;border-radius:%dpx;overflow:hidden;background:#111}.%s img{width:100%%;height:100%%;object-fit:cover}`, c("hero"), k.rad+6, c("hero"))
	fmt.Fprintf(&b, `.%s svg{position:absolute;left:50%%;top:50%%;width:76px;height:76px;margin:-38px 0 0 -38px;filter:drop-shadow(0 2px 8px rgba(0,0,0,.35))}`, c("hero"))
	// 博客
	fmt.Fprintf(&b, `.%s{display:grid;grid-template-columns:176px 1fr;gap:20px;padding:18px 0;border-bottom:1px solid #e5e8ee;align-items:center}.%s{aspect-ratio:3/2;border-radius:%dpx;overflow:hidden;background:#e7e9ee}.%s img{width:100%%;height:100%%;object-fit:cover}`, c("post"), c("pimg"), k.rad+2, c("pimg"))
	fmt.Fprintf(&b, `article .%s{aspect-ratio:2/1;margin:6px 0 24px}`, c("hero"))
	// 软件下载
	fmt.Fprintf(&b, `.%s{display:flex;align-items:center;gap:18px;margin-bottom:6px}.%s img{width:64px;height:64px;flex:none}`, c("head2"), c("head2"))
	fmt.Fprintf(&b, `.%s{display:grid;grid-template-columns:repeat(auto-fit,minmax(210px,1fr));gap:14px;margin:18px 0}.%s .%s{padding:18px 20px}`, c("dl"), c("dl"), c("card"))
	fmt.Fprintf(&b, `.%s{display:inline-block;padding:8px 16px;border-radius:8px;background:%s;color:#fff;font-weight:600;font-size:14px}.%s:hover{text-decoration:none;filter:brightness(1.07)}`, c("btn"), acc, c("btn"))
	fmt.Fprintf(&b, `.%s{margin:8px 0 6px;border-radius:%dpx;overflow:hidden;border:1px solid #e5e8ee;box-shadow:0 22px 44px -22px rgba(20,30,50,.38)}.%s img{width:100%%;height:auto}`, c("shot"), k.rad+6, c("shot"))
	fmt.Fprintf(&b, `@media(max-width:640px){nav{gap:14px}h1{font-size:23px}.%s{display:none}.%s{grid-template-columns:1fr}}`, c("hide"), c("post"))
	return []byte(b.String())
}

// asset 放一个静态文件(图片),返回它的路径(文件名像打包工具出的带哈希的名字)
func (s *Site) asset(g *gen, ext, ctype string, body []byte) string {
	p := g.sk.dir + g.id(10) + "." + ext
	s.add(p, ctype, body)
	return p
}

// layout 共用的页面外框:顶栏(logo、站名、导航)、内容、页脚
func layout(g *gen, s *Site, title string, nav [][2]string, body string) []byte {
	c := g.c
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>`, esc(title))
	fmt.Fprintf(&b, `<link rel="icon" href="%s"><link rel="stylesheet" href="%s"></head><body>`, s.icon, s.css)
	fmt.Fprintf(&b, `<header><div class="%s"><a class="%s" href="/">%s%s</a><nav>`, c("wrap"), c("name"), s.logo, esc(s.name))
	for _, n := range nav {
		fmt.Fprintf(&b, `<a href="%s">%s</a>`, n[0], esc(n[1]))
	}
	fmt.Fprintf(&b, `</nav></div></header><main class="%s">%s</main><footer><div class="%s">© %d %s</div></footer></body></html>`, c("wrap"), body, c("wrap"), g.base.Year(), esc(s.name))
	return []byte(b.String())
}

func notFoundBody(g *gen) string {
	return `<h1>Page not found</h1><p class="` + g.c("sub") + `">The page you were looking for doesn't exist or has moved.</p><p><a href="/">Back to the home page</a></p>`
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
	nav := [][2]string{{"/", "Projects"}, {"/about/", "About"}}
	var names []string
	desc := map[string]string{}
	for _, p := range mirrorProjects {
		names = append(names, p[0])
		desc[p[0]] = p[1]
	}
	r, mono := g.c("r"), g.c("mono")
	chosen := g.pickN(names, g.n(9, 14))
	var rows strings.Builder
	total := 0
	for _, name := range chosen {
		gb := g.n(18, 2400)
		total += gb
		fmt.Fprintf(&rows, `<tr><td><a href="/%s/">%s/</a></td><td class="%s %s">%s</td><td class="%s %s">%s</td><td class="%s %s">%s</td></tr>`,
			name, name, g.c("hide"), g.c("muted"), esc(desc[name]), r, mono, size(gb), r, mono, g.daysAgo(g.n(0, 2)))
		ver := fmt.Sprintf("%d.%d", g.n(3, 42), g.n(0, 9))
		var list strings.Builder
		for _, d := range g.pickN(mirrorDirs, g.n(2, 4)) {
			date := g.daysAgo(g.n(0, 9))
			fmt.Fprintf(&list, `<tr><td><a href="/%s/%s/">%s/</a></td><td class="%s %s">-</td><td class="%s %s">%s</td></tr>`, name, d, d, r, mono, r, mono, date)
			var sub strings.Builder
			for _, fn := range mirrorDirFiles(d) {
				body, ctype := mirrorFile(g, name, desc[name], ver, d, fn)
				p := "/" + name + "/" + d + "/" + fn
				s.add(p, ctype, body)
				fmt.Fprintf(&sub, `<tr><td><a href="%s">%s</a></td><td class="%s %s">%s</td><td class="%s %s">%s</td></tr>`, p, fn, r, mono, humanSize(len(body)), r, mono, date)
			}
			s.add("/"+name+"/"+d+"/", htmlType, layout(g, s, "Index of /"+name+"/"+d+"/ - "+s.name, nav, indexBody(g, "/"+name+"/"+d+"/", "/"+name+"/", desc[name], sub.String())))
		}
		for _, fn := range []string{"README", "SHA256SUMS", "SHA256SUMS.sig"} {
			body, ctype := mirrorFile(g, name, desc[name], ver, "", fn)
			p := "/" + name + "/" + fn
			s.add(p, ctype, body)
			fmt.Fprintf(&list, `<tr><td><a href="%s">%s</a></td><td class="%s %s">%s</td><td class="%s %s">%s</td></tr>`, p, fn, r, mono, humanSize(len(body)), r, mono, g.daysAgo(g.n(1, 30)))
		}
		s.add("/"+name+"/", htmlType, layout(g, s, "Index of /"+name+"/ - "+s.name, nav, indexBody(g, "/"+name+"/", "/", desc[name], list.String())))
	}
	every := g.pick([]string{"4", "6", "8", "12"})
	body := fmt.Sprintf(`<h1>%s</h1><p class="%s">Open-source software mirror, synchronized with upstream every %s hours.</p>`+
		`<p><span class="%s">%d projects</span><span class="%s">%s total</span><span class="%s">HTTP / HTTPS</span></p>`+
		`<div class="%s"><table><tr><th>Project</th><th class="%s">Description</th><th class="%s">Size</th><th class="%s">Last sync</th></tr>%s</table></div>`,
		esc(s.name), g.c("sub"), every, g.c("chip"), len(chosen), g.c("chip"), size(total), g.c("chip"), g.c("card"), g.c("hide"), r, r, rows.String())
	s.add("/", htmlType, layout(g, s, s.name, nav, body))
	s.add("/about/", htmlType, layout(g, s, "About - "+s.name, nav,
		`<h1>About this mirror</h1><p class="`+g.c("sub")+`">A public mirror for open-source projects.</p>`+
			`<p>This mirror is run as a public service. Projects are synchronized from their official upstream servers several times a day, and files are served exactly as published.</p>`+
			`<p>Please verify downloads with the checksums and signatures provided by each project. Bandwidth is shared, so large or repeated downloads may be slower at peak times.</p>`+
			`<p>Access is available over HTTP and HTTPS only. There is no rsync or FTP service.</p>`))
	s.setNotFound(layout(g, s, "Not found - "+s.name, nav, notFoundBody(g)))
}

func indexBody(g *gen, path, parent, desc, rows string) string {
	return fmt.Sprintf(`<h1>Index of %s</h1><p class="%s">%s</p><div class="%s"><table><tr><th>Name</th><th class="%s">Size</th><th class="%s">Last modified</th></tr><tr><td><a href="%s">../</a></td><td></td><td></td></tr>%s</table></div>`,
		esc(path), g.c("sub"), esc(desc), g.c("card"), g.c("r"), g.c("r"), parent, rows)
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

func (g *gen) b64(n int) string {
	const abc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := make([]byte, n)
	for i := range b {
		b[i] = abc[g.r.IntN(64)]
	}
	return string(b)
}

// ---- 软件下载 ----

func genDownloads(s *Site, g *gen) {
	kind := g.pick([]string{"Studio", "Sync", "Desk", "Capture", "Notes", "Player", "Draw"})
	product := g.pick(words) + " " + kind
	s.name = product
	maker := g.pick(words) + " Labs"
	icon := s.asset(g, "svg", svgType, appIcon(g, kind))
	shot := s.asset(g, "svg", svgType, appShot(g, kind))
	s.logo = fmt.Sprintf(`<img class="%s" src="%s" alt="" width="26" height="26">`, g.c("logo"), icon)
	nav := [][2]string{{"/", "Download"}, {"/releases/", "Releases"}, {"/about/", "About"}}
	major, minor := g.n(2, 6), g.n(0, 9)
	type rel struct{ ver, date string }
	var rels []rel
	day := g.n(3, 20)
	for i := 0; i < 6; i++ {
		rels = append(rels, rel{fmt.Sprintf("%d.%d.%d", major, minor, 6-i), g.dateAgo(day)})
		day += g.n(12, 40)
	}
	slug := strings.ToLower(strings.ReplaceAll(product, " ", "-"))
	notes := []string{"Faster startup on large libraries", "Improved memory usage when syncing many files", "Fixed a crash when opening very large projects",
		"Updated translations", "New keyboard shortcuts for common actions", "Better handling of slow network connections", "Smoother scrolling on high-refresh displays",
		"Fixed export of files with long names", "Security and stability improvements"}
	oses := [][2]string{{"win", "Windows"}, {"mac", "macOS"}, {"linux", "Linux"}}
	type file struct{ name, size, sha, os string }
	files := func(ver string) []file {
		var out []file
		for _, o := range oses {
			f := file{os: o[1], sha: g.hex(64)}
			switch o[0] {
			case "win":
				f.name, f.size = fmt.Sprintf("%s-%s-x64-setup.exe", slug, ver), mb(g.n(90, 260))
			case "mac":
				f.name, f.size = fmt.Sprintf("%s-%s-universal.dmg", slug, ver), mb(g.n(110, 320))
			default:
				f.name, f.size = fmt.Sprintf("%s-%s-x86_64.AppImage", slug, ver), mb(g.n(95, 280))
			}
			out = append(out, f)
		}
		return out
	}
	r, mono, sub := g.c("r"), g.c("mono"), g.c("sub")
	var cards, relList strings.Builder
	for i, rl := range rels {
		fmt.Fprintf(&relList, `<tr><td><a href="/releases/%s/">%s %s</a></td><td class="%s %s">%s</td></tr>`, rl.ver, esc(product), rl.ver, r, g.c("muted"), rl.date)
		var rows, changes strings.Builder
		for _, f := range files(rl.ver) {
			p := "/releases/" + rl.ver + "/" + f.name
			fmt.Fprintf(&rows, `<tr><td><a href="%s">%s</a></td><td class="%s">%s</td><td class="%s %s %s">%s…</td></tr>`, p, esc(f.name), r, f.size, g.c("hide"), mono, g.c("muted"), f.sha[:16])
			// 点了下载:一页「谢谢下载」(下载链接本身是给浏览器的,这里不放安装包)
			s.add(p, htmlType, layout(g, s, "Downloading "+f.name, nav,
				fmt.Sprintf(`<h1>Thanks for downloading %s</h1><p class="%s">%s · version %s · %s</p><p>Your download should begin in a few seconds. If it doesn't, check your connection and <a href="/releases/%s/">start it again from the release page</a>.</p><p class="%s">SHA-256: <span class="%s">%s</span></p>`,
					esc(product), sub, f.os, rl.ver, f.size, rl.ver, g.c("muted"), mono, f.sha)))
			if i == 0 {
				fmt.Fprintf(&cards, `<div class="%s"><b>%s</b><div class="%s" style="font-size:13px;margin:2px 0 12px">%s · %s</div><a class="%s" href="%s">Download</a></div>`,
					g.c("card"), f.os, g.c("muted"), esc(f.name), f.size, g.c("btn"), p)
			}
		}
		for _, n := range g.pickN(notes, g.n(2, 4)) {
			fmt.Fprintf(&changes, `<li>%s</li>`, esc(n))
		}
		s.add("/releases/"+rl.ver+"/", htmlType, layout(g, s, product+" "+rl.ver, nav,
			fmt.Sprintf(`<h1>%s %s</h1><p class="%s">Released %s</p><h2>What's new</h2><ul>%s</ul><h2>Files</h2><div class="%s"><table><tr><th>File</th><th class="%s">Size</th><th class="%s">SHA-256</th></tr>%s</table></div>`,
				esc(product), rl.ver, sub, rl.date, changes.String(), g.c("card"), r, g.c("hide"), rows.String())))
	}
	s.add("/", htmlType, layout(g, s, "Download "+product, nav,
		fmt.Sprintf(`<div class="%s"><img src="%s" alt="" width="64" height="64"><div><h1>Download %s</h1><p class="%s" style="margin:0">Version %s · released %s</p></div></div>`+
			`<div class="%s">%s</div><div class="%s"><img src="%s" alt="%s" width="960" height="600"></div><h2>Release history</h2><div class="%s"><table>%s</table></div>`,
			g.c("head2"), icon, esc(product), sub, rels[0].ver, rels[0].date, g.c("dl"), cards.String(), g.c("shot"), shot, esc(product), g.c("card"), relList.String())))
	s.add("/releases/", htmlType, layout(g, s, "Releases - "+product, nav,
		fmt.Sprintf(`<h1>Releases</h1><p class="%s">All versions of %s.</p><div class="%s"><table>%s</table></div>`, sub, esc(product), g.c("card"), relList.String())))
	s.add("/about/", htmlType, layout(g, s, "About - "+product, nav,
		fmt.Sprintf(`<h1>About %s</h1><p class="%s">Made by %s.</p><p>%s is a small desktop app for working with large files on any computer. Downloads are served from our own servers; please use the checksums listed with each release to verify them.</p><p>Older releases stay available for people who need them, but only the latest version receives fixes.</p>`,
			esc(product), sub, esc(maker), esc(product))))
	s.setNotFound(layout(g, s, "Not found - "+product, nav, notFoundBody(g)))
}

// ---- 视频站 ----

var videoTitles = []string{"Coastline at First Light", "City Lights Timelapse", "Mountain Trail in 4K", "Studio Session", "Rainy Afternoon in the Old Town",
	"Harbor Walk at Dusk", "Night Train North", "Forest Ambience", "Desert Road Trip", "Winter Lake", "Market Morning", "Rooftop Sunset",
	"Behind the Scenes", "Workshop Recording", "Island Hopping", "Cloud Inversion"}

var videoNotes = []string{"Recorded on location in a single take, with the original ambient sound.", "Best watched full screen with the sound on.",
	"Filmed over two days and edited down to the quiet moments.", "No narration, no music: just the place as it sounded that day.",
	"Shot handheld; the colours are left close to how they looked.", "Part of a longer series we keep adding to every few weeks."}

func genVideo(s *Site, g *gen) {
	s.name = g.pick(words) + " " + g.pick([]string{"Video", "Stream", "Screen", "Reels"})
	s.logo = `<span class="` + g.c("logo") + `"></span>`
	nav := [][2]string{{"/", "Library"}, {"/about/", "About"}}
	type vid struct{ id, title, dur, date, views, thumb string }
	var vids []vid
	for _, ti := range g.pickN(videoTitles, 12) {
		vids = append(vids, vid{id: g.id(11), title: ti, dur: fmt.Sprintf("%d:%02d", g.n(2, 48), g.n(0, 59)), date: g.dateAgo(g.n(1, 220)),
			views: views(g), thumb: s.asset(g, "svg", svgType, cover(g, ti, 320, 180))})
	}
	card := func(v vid) string {
		return fmt.Sprintf(`<a href="/watch/%s"><div class="%s"><img src="%s" alt="" width="320" height="180" loading="lazy"><span class="%s">%s</span></div><div class="%s">%s</div><div class="%s" style="font-size:13px">%s views · %s</div></a>`,
			v.id, g.c("thumb"), v.thumb, g.c("dur"), v.dur, g.c("vt"), esc(v.title), g.c("muted"), v.views, v.date)
	}
	var grid strings.Builder
	for i, v := range vids {
		grid.WriteString(card(v))
		var more strings.Builder
		for k := 1; k <= 4; k++ {
			more.WriteString(card(vids[(i+k)%len(vids)]))
		}
		s.add("/watch/"+v.id, htmlType, layout(g, s, v.title+" - "+s.name, nav,
			fmt.Sprintf(`<div class="%s"><img src="%s" alt="" width="1280" height="720"><svg viewBox="0 0 80 80"><circle cx="40" cy="40" r="38" fill="rgba(0,0,0,.45)"/><path d="M32 25v30l24-15z" fill="#fff"/></svg></div>`+
				`<h1 style="margin-top:18px">%s</h1><p class="%s">%s views · %s · %s</p><p>%s Playback is available to signed-in members in the app.</p><h2>More videos</h2><div class="%s">%s</div>`,
				g.c("hero"), v.thumb, esc(v.title), g.c("sub"), v.views, v.date, v.dur, esc(g.pick(videoNotes)), g.c("grid"), more.String())))
	}
	s.add("/", htmlType, layout(g, s, s.name, nav,
		fmt.Sprintf(`<h1>%s</h1><p class="%s">A private video library for our members.</p><div class="%s">%s</div>`, esc(s.name), g.c("sub"), g.c("grid"), grid.String())))
	s.add("/about/", htmlType, layout(g, s, "About - "+s.name, nav,
		fmt.Sprintf(`<h1>About %s</h1><p class="%s">Recordings, travel films and studio sessions.</p><p>%s hosts long recordings for a small community. Videos stream to members through our apps; this site only shows the library.</p><p>New videos are added every week.</p>`, esc(s.name), g.c("sub"), esc(s.name))))
	s.setNotFound(layout(g, s, "Not found - "+s.name, nav, notFoundBody(g)))
}

func views(g *gen) string {
	switch n := g.n(3, 280); {
	case n < 10:
		return strconv.Itoa(n*100 + g.n(0, 99))
	case n < 100:
		return fmt.Sprintf("%.1fK", float64(n)/10)
	default:
		return fmt.Sprintf("%dK", n/2)
	}
}

// ---- 个人站 ----

var firstNames = []string{"Mara", "Theo", "Iris", "Noah", "Lena", "Owen", "Sana", "Felix", "Nora", "Elias", "Clara", "Jonas"}
var lastNames = []string{"Lindqvist", "Hart", "Calder", "Brandt", "Okafor", "Price", "Ruiz", "Moreau", "Vance", "Holm", "Arden", "Keller"}

var blogPosts = []string{"Notes on long exposure at dusk", "A week of cycling the coast", "Rebuilding my home server", "What I learned from a hundred rolls of film",
	"Packing light for a month on the road", "Small tools I use every day", "Printing photos again", "Walking the old railway line",
	"Keeping a notebook for ten years", "Learning to repair my own bike", "A quiet weekend in the mountains", "Why I still burn photos to disc"}

// blogLead 每篇文章自己的开头一句(列表里的摘要就是它):各篇不重样
var blogLead = map[string]string{
	"Notes on long exposure at dusk":              "I spent most evenings last month on the same stretch of shore with a tripod and a cheap ND filter, trying to get the water to go flat and silky.",
	"A week of cycling the coast":                 "Seven days, a bit over five hundred kilometres, and more headwind than I would like to admit.",
	"Rebuilding my home server":                   "The old box finally gave up after six years, so I used the excuse to start again from scratch and keep things simpler this time.",
	"What I learned from a hundred rolls of film": "A hundred rolls sounds like a lot, but spread over three years it was really just one roll every week or so.",
	"Packing light for a month on the road":       "Everything I carried this time fit into one small backpack, and I did not miss anything I left behind.",
	"Small tools I use every day":                 "These are the few things that live on my desk and in my bag, and that I would buy again without thinking.",
	"Printing photos again":                       "After years of keeping everything on screens I bought a small printer, and it has changed which pictures I keep.",
	"Walking the old railway line":                "The line closed decades ago, but most of the route is still there if you know where to look.",
	"Keeping a notebook for ten years":            "My first notebook is falling apart now, but it is still the one I open most often.",
	"Learning to repair my own bike":              "The first repair took me a whole Saturday; the tenth took about twenty minutes.",
	"A quiet weekend in the mountains":            "We left on Friday evening with no real plan beyond a map, two days of food and a rough idea of the trail.",
	"Why I still burn photos to disc":             "Every year I copy the photos I care about onto a couple of discs and put them in a drawer, and people still ask me why.",
}

var blogParas = []string{
	"I started this a few months ago without much of a plan, and it has slowly turned into a habit. Most evenings I spend half an hour on it, and the small progress adds up faster than I expected.",
	"The hardest part was not the equipment but the patience. Light changes quickly near sunset, and the best frames usually come in the few minutes after you think it is over.",
	"I wrote down everything I packed and everything I actually used. The second list was much shorter, and next time I will leave half of the first one at home.",
	"There is a particular kind of quiet on early mornings that I have only found outside the city. It is worth the alarm clock, every time.",
	"Most of the work was undoing my own earlier shortcuts. Labeling cables and writing a short note for every change would have saved me a whole weekend.",
	"I kept the setup simple on purpose: fewer moving parts, fewer things to break, and nothing I cannot fix myself with a screwdriver and an afternoon.",
	"Looking back through old pages is the best part. Small details that felt unimportant at the time are now the ones I am happiest to have kept.",
	"If you are thinking about trying this, start small. One route, one roll, one evening a week is enough to find out whether you enjoy it.",
	"The weather did not cooperate for most of the trip, but the grey days produced some of my favorite pictures. Flat light is kinder than it looks.",
	"I am still not sure I got it right, but it works, and that is good enough for now. I will write a follow-up once I have used it for a season.",
}

func genBlog(s *Site, g *gen) {
	s.name = g.pick(firstNames) + " " + g.pick(lastNames)
	av := s.asset(g, "svg", svgType, avatar(g))
	s.logo = fmt.Sprintf(`<img class="%s %s" src="%s" alt="" width="26" height="26">`, g.c("logo"), g.c("av"), av)
	nav := [][2]string{{"/", "Writing"}, {"/about/", "About"}}
	topic := g.pick([]string{"Photography, travel and small software projects", "Notes on cycling, film cameras and home servers",
		"Walking, photographs and things I fix myself", "Slow travel, notebooks and the occasional bit of code"})
	var list strings.Builder
	day := g.n(2, 15)
	for _, ti := range g.pickN(blogPosts, 8) {
		slug := strings.ToLower(strings.NewReplacer(" ", "-", ",", "").Replace(ti))
		date := g.dateAgo(day)
		day += g.n(9, 45)
		lead := blogLead[ti]
		var body strings.Builder
		fmt.Fprintf(&body, `<p>%s</p>`, esc(lead))
		for _, p := range g.pickN(blogParas, g.n(2, 4)) {
			fmt.Fprintf(&body, `<p>%s</p>`, esc(p))
		}
		mins := g.n(3, 9)
		img := s.asset(g, "svg", svgType, cover(g, ti, 640, 320))
		fmt.Fprintf(&list, `<div class="%s"><a class="%s" href="/posts/%s/"><img src="%s" alt="" width="640" height="320" loading="lazy"></a><div><a href="/posts/%s/" style="font-weight:600;font-size:17px">%s</a><div class="%s" style="font-size:13px">%s · %d min read</div><p style="margin:6px 0 0;font-size:14px">%s</p></div></div>`,
			g.c("post"), g.c("pimg"), slug, img, slug, esc(ti), g.c("muted"), date, mins, esc(lead))
		s.add("/posts/"+slug+"/", htmlType, layout(g, s, ti+" - "+s.name, nav,
			fmt.Sprintf(`<article style="max-width:700px"><h1>%s</h1><p class="%s">%s · %d min read</p><div class="%s"><img src="%s" alt="" width="640" height="320"></div>%s<p><a href="/">← All writing</a></p></article>`,
				esc(ti), g.c("sub"), date, mins, g.c("hero"), img, body.String())))
	}
	s.add("/", htmlType, layout(g, s, s.name, nav,
		fmt.Sprintf(`<div style="max-width:760px"><h1>%s</h1><p class="%s">%s</p>%s</div>`, esc(s.name), g.c("sub"), esc(topic), list.String())))
	s.add("/about/", htmlType, layout(g, s, "About - "+s.name, nav,
		fmt.Sprintf(`<div style="max-width:700px"><img class="%s" src="%s" alt="" width="96" height="96" style="width:96px;height:96px;border-radius:50%%;margin-bottom:14px"><h1>About</h1><p>Hi, I'm %s. %s.</p><p>This site is where I keep notes and pictures from things I am working on. It is a personal site with no ads and no tracking.</p></div>`,
			g.c("av"), av, esc(strings.Fields(s.name)[0]), esc(topic))))
	s.setNotFound(layout(g, s, "Not found - "+s.name, nav, notFoundBody(g)))
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
