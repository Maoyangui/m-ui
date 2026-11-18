package camo

// 软件下载页上的「软件截图」(用户 11-19 精修:原来是灰条代替文字的示意图,像没做完的线框图)。
// 改成一个窗口:标题栏、侧栏、工具栏和按软件类别排的内容 —— 播放器(画面 + 接下来)、截图工具(图库)、录音(音轨波形)、
// 同步(文件和进度)、笔记(列表 + 正文)、远程桌面(电脑列表 + 画面)、绘图(画布 + 图层)。字都是真的字;
// 画面、图库用图包照片(没有时画插图)。整个窗口不能点(它只是一张「截图」),手机上收起侧栏。

import (
	"fmt"
	"strings"
)

var appSide = map[string][]string{
	"Player":  {"Library", "Recently played", "Movies", "TV shows", "Music videos", "Playlists", "Folders"},
	"Capture": {"All captures", "Screenshots", "Recordings", "Favorites", "Albums", "Shared"},
	"Studio":  {"Projects", "Recordings", "Mixes", "Samples", "Plug-ins", "Exports"},
	"Sync":    {"All files", "Recent", "Shared with me", "Offline", "Devices", "Trash"},
	"Notes":   {"All notes", "Pinned", "Journal", "Ideas", "Reading list", "Archive"},
	"Desk":    {"Computers", "Recent sessions", "Shared with me", "Address book", "File transfer"},
	"Draw":    {"Canvases", "Sketches", "Brushes", "Swatches", "Exports"},
}

var appAction = map[string]string{"Player": "Open file", "Capture": "New capture", "Studio": "Record", "Sync": "Upload",
	"Notes": "New note", "Desk": "New connection", "Draw": "New canvas"}

var syncNames = []string{"Photos", "Documents", "Projects", "Invoices", "Travel", "Music", "Backups", "Scans",
	"budget-2026.xlsx", "notes.md", "contract-final.pdf", "presentation.key", "family-trip.zip", "receipts-oct.pdf", "draft-v3.docx", "design-system.fig"}

var noteTitles = []string{"Weekly plan", "Book notes: slow travel", "Ideas for the garden", "Meeting with Sam", "Packing list",
	"Recipes to try", "Bike maintenance", "Reading list", "Q4 goals", "Trip to the coast", "Quotes I like", "Things to fix at home"}

var deskNames = []string{"studio-mac", "office-pc", "home-laptop", "media-server", "dad's-pc", "render-box", "nas", "workshop-pc"}
var deskOS = []string{"macOS 15", "Windows 11", "Ubuntu 24.04", "Windows 10", "Fedora 41", "macOS 14"}

var trackNames = []string{"Lead vocal", "Acoustic gtr", "Bass DI", "Drums (room)", "Piano", "Synth pad", "Backing vox", "Strings", "Click"}

// appMock 软件窗口(下载页上那张「截图」)
func appMock(s *Site, g *gen, kind, product string) string {
	c := g.c
	items := appSide[kind]
	if items == nil {
		items = appSide["Draw"]
	}
	var side strings.Builder
	fmt.Fprintf(&side, `<b>%s</b>`, esc(product))
	for i, it := range items {
		cls := ""
		if i == 0 {
			cls = ` class="` + c("on") + `"`
		}
		fmt.Fprintf(&side, `<span%s><i></i>%s</span>`, cls, esc(it))
	}
	action := appAction[kind]
	if action == "" {
		action = "New"
	}
	pic := func(slot int, kind2, tod string, w, h, pw, ph int) string {
		return s.picture(g, 100+slot, kind2, tod, picSize{w: w, h: h, pw: pw, ph: ph})[0]
	}
	scenes := []string{"sea", "city", "mountain", "lake", "forest", "harbor", "island", "desert", "clouds", "town"}
	tods := []string{"dawn", "day", "dusk"}
	var body strings.Builder
	switch kind {
	case "Player":
		k := g.pick(scenes)
		fmt.Fprintf(&body, `<div class="%s"><img src="%s" alt=""><div class="%s"><s></s><span>%d:%02d</span><i><b style="width:%d%%"></b></i><span>%d:%02d</span></div></div><div class="%s">Up next</div>`,
			c("stage"), pic(0, k, g.pick([]string{"dusk", "night", "dawn"}), 800, 450, 200, 112), c("ctrl"), g.n(3, 19), g.n(0, 59), g.n(18, 62), g.n(24, 58), g.n(0, 59), c("lhead"))
		titles := map[string]bool{}
		for i := 0; i < 3; i++ {
			ti, kind2, tod, photo := s.videoPick(g, 101+i, titles)
			fmt.Fprintf(&body, `<div class="%s"><img src="%s" alt=""><span>%s</span><span class="%s">%d:%02d</span><span></span></div>`,
				c("lrow"), s.pictureOf(g, 101+i, photo, kind2, tod, picSize{w: 192, h: 108, pw: 96, ph: 54})[0], esc(ti), c("muted"), g.n(4, 52), g.n(0, 59))
		}
	case "Capture":
		fmt.Fprintf(&body, `<div class="%s">%s · %d items</div><div class="%s">`, c("lhead"), esc(items[0]), g.n(40, 380), c("pgrid"))
		on := g.r.IntN(6)
		for i := 0; i < 6; i++ {
			cls := ""
			if i == on {
				cls = ` class="` + c("on") + `"`
			}
			fmt.Fprintf(&body, `<img%s src="%s" alt="">`, cls, pic(i, g.pick(scenes), g.pick(tods), 288, 162, 96, 54))
		}
		body.WriteString(`</div>`)
	case "Studio":
		fmt.Fprintf(&body, `<div class="%s">%s · %d BPM · 48 kHz · 00:0%d:%02d.%03d</div>`, c("lhead"), esc(g.pick([]string{"Session 14", "Late night take", "Room recording", "Demo v3"})), g.n(72, 140), g.n(1, 4), g.n(0, 59), g.n(0, 999))
		for _, name := range g.pickN(trackNames, 5) {
			hue := float64(g.hue + g.n(-40, 40))
			fmt.Fprintf(&body, `<div class="%s"><span>%s</span><span class="%s">M</span><span class="%s">S</span>%s</div>`,
				c("track"), esc(name), c("ms"), c("ms"), waveSVG(g, c("wave"), hslHex(hue, 55, 52), hslHex(hue, 60, 94)))
		}
	case "Sync":
		fmt.Fprintf(&body, `<div class="%s">Up to date · last synced %d min ago · %d.%d GB of %d GB used</div>`, c("lhead"), g.n(1, 9), g.n(12, 180), g.n(0, 9), []int{200, 500, 1000, 2000}[g.r.IntN(4)])
		for _, n := range g.pickN(syncNames, 7) {
			ico := c("ico")
			size := "—"
			if strings.Contains(n, ".") {
				ico += " " + c("file")
				size = fmt.Sprintf("%.1f MB", float64(g.n(2, 900))/10)
			} else {
				size = fmt.Sprintf("%d items", g.n(3, 480))
			}
			status := fmt.Sprintf(`<span class="%s">Synced</span>`, c("ok"))
			if g.r.IntN(5) == 0 {
				status = fmt.Sprintf(`<span class="%s"><i style="width:%d%%"></i></span>`, c("prog"), g.n(15, 85))
			}
			fmt.Fprintf(&body, `<div class="%s"><i class="%s"></i><span>%s</span><span class="%s">%s</span>%s</div>`, c("lrow"), ico, esc(n), c("muted"), size, status)
		}
	case "Notes":
		titles := g.pickN(noteTitles, 5)
		var list strings.Builder
		day := g.n(0, 2)
		for i, t := range titles { // 最近改过的在上面
			cls := ""
			if i == 0 {
				cls = ` class="` + c("on") + `"`
			}
			fmt.Fprintf(&list, `<div%s><b>%s</b><span>%s</span></div>`, cls, esc(t), g.dateAgo(day))
			day += g.n(1, 9)
		}
		var text strings.Builder
		for _, p := range blogParagraphs(g, 3) {
			fmt.Fprintf(&text, `<p>%s</p>`, esc(p))
		}
		fmt.Fprintf(&body, `<div class="%s"><div class="%s">%s</div><div class="%s"><h4>%s</h4>%s</div></div>`, c("notes"), c("nlist"), list.String(), c("ntext"), esc(titles[0]), text.String())
	case "Desk":
		names := g.pickN(deskNames, 4)
		fmt.Fprintf(&body, `<div class="%s"><img src="%s" alt=""><div class="%s"><s></s><span>%s · %s · 60 fps</span><i></i><span>Connected</span></div></div>`,
			c("stage"), pic(0, g.pick([]string{"mountain", "lake", "clouds", "sea"}), g.pick(tods), 800, 450, 200, 112), c("ctrl"), esc(names[0]), g.pick([]string{"2560×1440", "1920×1080", "3840×2160"}))
		for i, n := range names[1:] {
			dot := c("dot")
			seen := fmt.Sprintf("last seen %d min ago", g.n(2, 50))
			if i == 0 || g.r.IntN(2) == 0 {
				dot += " " + c("on")
				seen = "online"
			}
			fmt.Fprintf(&body, `<div class="%s"><i class="%s"></i><span>%s</span><span class="%s">%s · %s</span><span></span></div>`, c("lrow"), dot, esc(n), c("muted"), g.pick(deskOS), seen)
		}
	default: // Draw
		k := g.pick(scenes)
		fmt.Fprintf(&body, `<div class="%s"><img src="%s" alt="">%s</div><div class="%s">Layers</div>`, c("canvas"), pic(0, k, g.pick(tods), 800, 450, 200, 112), strokesSVG(g), c("lhead"))
		for i, n := range []string{"Ink", "Color", "Reference photo"} {
			dot := c("dot")
			if i < 2 {
				dot += " " + c("on")
			}
			fmt.Fprintf(&body, `<div class="%s"><i class="%s"></i><span>%s</span><span class="%s">%d%%</span><span></span></div>`, c("lrow"), dot, n, c("muted"), []int{100, 85, 60}[i])
		}
	}
	return fmt.Sprintf(`<div class="%s" role="img" aria-label="%s"><div class="%s"><div class="%s"><i style="background:#ff5f57"></i><i style="background:#febc2e"></i><i style="background:#28c840"></i><span>%s</span></div>`+
		`<div class="%s"><div class="%s">%s</div><div class="%s"><div class="%s"><span class="%s">Search</span><span class="%s">%s</span></div>%s</div></div></div></div>`,
		c("shot"), esc(product+" window"), c("win"), c("wbar"), esc(product),
		c("wbody"), c("side"), side.String(), c("wmain"), c("tool"), c("srch"), c("btn"), esc(action), body.String())
}

// waveSVG 一条音轨的波形(竖条,高低按随机起伏)
func waveSVG(g *gen, cls, fg, bg string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="%s" viewBox="0 0 300 40" preserveAspectRatio="none"><rect width="300" height="40" fill="%s"/>`, cls, bg)
	amp := .35 + .4*g.r.Float64()
	for x := 2; x < 298; x += 3 {
		v := amp * (0.25 + 0.75*g.r.Float64()) * 17
		if g.r.IntN(9) == 0 {
			v *= .3
		}
		fmt.Fprintf(&b, `<rect x="%d" y="%s" width="2" height="%s" fill="%s"/>`, x, ff(20-v), ff(2*v), fg)
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// strokesSVG 画布上的几笔(绘图软件)
func strokesSVG(g *gen) string {
	acc := g.accent()
	var b strings.Builder
	b.WriteString(`<svg viewBox="0 0 400 225" preserveAspectRatio="none" fill="none" stroke-linecap="round" stroke-linejoin="round">`)
	for i := 0; i < 3; i++ {
		x, y := 40+g.n(0, 120), 60+g.n(0, 110)
		fmt.Fprintf(&b, `<path d="M%d %dq%d %d %d %dt%d %d" stroke="%s" stroke-width="%d" opacity=".85"/>`,
			x, y, g.n(30, 80), -g.n(20, 60), g.n(80, 160), g.n(-20, 20), g.n(60, 120), g.n(-30, 30), []string{acc, "#ffffff", "#1c2330"}[i], []int{5, 3, 2}[i])
	}
	b.WriteString(`</svg>`)
	return b.String()
}
