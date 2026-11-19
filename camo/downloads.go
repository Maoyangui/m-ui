package camo

// 软件下载页(伪装站的一种):产品首页(下载卡、软件窗口、功能介绍、邮件订阅、发布历史)、各版本页、「谢谢下载」页、关于页,
// 外加检查更新和订阅两个接口。
//
// 页面上给人看的句子都按种子挑说法(text.go 的 say)。原来「Release notes by email」「Release history」「Your download should
// begin in a few seconds…」这些各台一字不差,更新说明只有 9 条固定的句子,功能介绍的小图标是固定的几段路径(审查 F-H1)。

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// featureIcon 功能介绍里的小图标(描边,跟着强调色)。形状就那十几种,坐标每台差一点:
// 这些路径是自己画的,别的网站上没有,写死的话拿任意一段去搜就只搜得出伪装站。
func featureIcon(g *gen, i int) string {
	n := g.n
	switch i % 13 {
	case 0: // 脉冲
		a, b := n(6, 9), n(6, 9)
		return fmt.Sprintf(`<path d="M3 12h4l3-%d 4 %d 3-%dh4"/>`, a, a+b, b)
	case 1: // 窗口
		x, y := n(3, 4), n(9, 11)
		return fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="%d" rx="%d"/><path d="M%d %dh%dM%d %dv%d"/>`, x, x, 24-2*x, 24-2*x, n(2, 4), x, y, 24-2*x, y, y, 24-x-y)
	case 2: // 盾
		w, a, b, c := n(7, 8), n(3, 4), n(5, 6), n(5, 6)
		d := c + n(2, 3)
		return fmt.Sprintf(`<path d="M12 3l%d %dv%dq0 %d-%d %dq-%d-%d-%d-%dv-%dz"/>`, w, a, b, c, w, d, w, d-c, w, d, b)
	case 3: // 上升
		y, a, b, c := n(17, 18), n(4, 6), n(3, 4), n(7, 9)
		return fmt.Sprintf(`<path d="M4 %dl5-%d 4 %d 7-%d"/><path d="M15 %dh5v5"/>`, y, a, b, c, y-a+b-c)
	case 4: // 钟
		return fmt.Sprintf(`<circle cx="12" cy="12" r="%d"/><path d="M12 %dv%dl%d %d"/>`, n(8, 9), n(7, 8), n(4, 5), n(2, 3), n(1, 2))
	case 5: // 云
		return fmt.Sprintf(`<path d="M7 18a4 4 0 0 1-.%d-8A6 6 0 0 1 18 8.%d 4.5 4.5 0 0 1 17.%d 18z"/>`, n(3, 6), n(3, 7), n(3, 7))
	case 6: // 对勾
		return fmt.Sprintf(`<path d="M%d 12l4 4 %d-%d"/>`, n(4, 5), n(10, 11), n(9, 10))
	case 7: // 下载
		a := n(4, 5)
		return fmt.Sprintf(`<path d="M12 %dv%dm-%d-%dl%d %d %d-%dM5 %dh14"/>`, n(3, 4), n(11, 12), a, a, a, a, a, a, n(19, 20))
	case 8: // 显示器
		y, h := n(4, 5), n(11, 12)
		return fmt.Sprintf(`<rect x="3" y="%d" width="18" height="%d" rx="%d"/><path d="M8 21h8M12 %dv%d"/>`, y, h, n(1, 3), y+h, 21-y-h)
	case 9: // 历史
		r := n(8, 9)
		return fmt.Sprintf(`<path d="M12 %da%d %d 0 1 0 %d %d"/><path d="M12 %dv5h%d"/>`, 12-r, r, r, r, r, n(7, 8), n(4, 5))
	case 10: // 锁
		y, t := n(10, 11), n(7, 8)
		return fmt.Sprintf(`<rect x="5" y="%d" width="14" height="%d" rx="2"/><path d="M8 %dV%da4 4 0 0 1 8 0v%d"/>`, y, n(9, 10), y, t, y-t)
	case 11: // 图层
		w, h := n(8, 9), n(4, 5)
		return fmt.Sprintf(`<path d="M12 %dl%d %d-%d %d-%d-%dz"/><path d="M%d %dl%d 4 %d-4"/>`, n(3, 4), w, h, w, h, w, h, 12-w, n(16, 17), w, w)
	}
	// 闪电
	a := n(6, 7)
	return fmt.Sprintf(`<path d="M13 %dL%d %dh%dl-1 %d 9-%dh-%dz"/>`, n(2, 3), n(4, 5), n(13, 14), a, n(7, 8), n(10, 11), a)
}

// releaseNotes 更新说明里的句子(每个版本挑两到四条;带可选词)
var releaseNotes = []string{
	"{Faster|Quicker} startup on large libraries", "{Improved|Lower} memory usage when syncing many files", "Fixed a crash when opening very large projects",
	"Updated translations", "New keyboard shortcuts for {common|frequent} actions", "Better handling of slow network connections",
	"Smoother scrolling on high-refresh displays", "Fixed export of files with long names", "Security and stability improvements",
	"Fixed a rare hang on {wake from sleep|resume}", "The settings window opens {noticeably|a lot} faster", "Search now matches {accented characters|partial words}",
	"Reduced CPU usage in the background", "Fixed drag and drop on multi-monitor setups", "{Clearer|More helpful} error messages when the disk is full",
	"Updated bundled libraries", "Fixed window position not being remembered", "Dark mode fixes in the sidebar",
	"Imports are {about twice as fast|much faster} for folders with many small files", "Fixed an issue with file names containing emoji",
	"Thumbnails are generated in the background", "Added an option to start minimized", "Fixed incorrect progress when a transfer is paused",
	"Better support for HiDPI screens on Linux", "Accessibility improvements for screen readers", "Fixed a memory leak after long sessions",
	"Fixed a crash when {resuming from sleep|switching displays}", "Improved rendering of very long lists", "{Reduced|Cut} install size by removing unused assets",
	"Updated the app icon for {newer|recent} systems", "Fixed tooltips appearing {off-screen|behind other windows}", "Added {natural|numeric} sorting for file names",
	"The update check no longer blocks startup", "Fixed a layout glitch at {125%|150%} display scaling", "Improved import from older versions",
	"Preferences are now searchable", "Fixed copy and paste of {rich text|multiple items}", "Better error reporting when a file is locked",
	"Fixed {a rare|an occasional} freeze when closing the last window", "The recent files list now remembers {20|30|50} entries", "Improved contrast in the light theme",
	"Fixed undo history being lost after autosave", "Native builds for {Apple silicon|ARM64} are now the default", "Renamed a few menu items for clarity",
}

// productKinds 每类软件在产品名里的几种叫法
var productKinds = map[string][]string{
	"Studio": {"Studio", "Audio", "Mix", "Record"}, "Sync": {"Sync", "Drive", "Cloud", "Backup"}, "Desk": {"Desk", "Remote", "Connect", "Access"},
	"Capture": {"Capture", "Snap", "Shot", "Grab"}, "Notes": {"Notes", "Write", "Journal", "Pages"}, "Player": {"Player", "Play", "Media", "View"},
	"Draw": {"Draw", "Sketch", "Paint", "Canvas"},
}

// osTokens 三个系统在页面属性、脚本、接口里的写法(按种子挑一套)
var osTokens = [][3]string{{"win", "mac", "linux"}, {"windows", "macos", "linux"}, {"win32", "darwin", "linux"}, {"w", "m", "l"}, {"win64", "osx", "linux64"}}

// iconOpen 功能介绍里小图标的 <svg …>:几种常见图标库的写法(%s 是线宽)。颜色跟着文字(currentColor),在样式表里给
var iconOpen = []string{
	`<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="%s" stroke-linecap="round" stroke-linejoin="round">`,
	`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="%s" stroke-linecap="round" stroke-linejoin="round">`,
	`<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="%s" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">`,
	`<svg aria-hidden="true" fill="none" viewBox="0 0 24 24" stroke-width="%s" stroke="currentColor" stroke-linecap="round" stroke-linejoin="round">`,
	`<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="%s" stroke="currentColor" stroke-linecap="round" stroke-linejoin="round">`,
}

func genDownloads(s *Site, g *gen) {
	kind := g.pick([]string{"Studio", "Sync", "Desk", "Capture", "Notes", "Player", "Draw"}) // 软件的类别(窗口里画什么、图标按它)
	product := g.pick(words) + " " + g.pick(productKinds[kind])
	s.name = product
	maker := g.pick(words) + " " + g.pick([]string{"Labs", "Software", "Works", "Apps", "Studio", "Systems", "& Co.", "Digital", "Tools", "Interactive"})
	s.foot = `<span>` + esc(fmt.Sprintf(g.say("Made by %s", "A product of %s", "Built by %s", "%s", "From the team at %s", "Developed by %s"), maker)) + `</span>`
	icon := s.asset(g, "svg", svgType, appIcon(g, kind))
	s.apiPre = g.sk.api
	rel, dot := g.sk.rel, g.sk.dot
	free := g.say("free for personal use", "free for home use", "free to use", "no account needed", "free, with an optional paid plan", "free for individuals")
	s.desc = product + g.say(" for Windows, macOS and Linux. Free for personal use.", " is a small, fast app for Windows, macOS and Linux.", ": download the latest version for your computer.",
		" runs on Windows, macOS and Linux. Get the latest release here.", ", a {lightweight|small} desktop app. Downloads for Windows, macOS and Linux.", ": official downloads and release notes.")
	// (脚本在下面挑好三个系统的写法以后再加)
	mock := appMock(s, g, kind, product)
	s.logo = g.img(g.c("logo"), icon, 0, 0, false)
	dlNav, relNav, aboutNav := g.say("Download", "Downloads", "Get it", "Install"), g.say("Releases", "Changelog", "Versions", "What's new"), g.say("About", "About", "Company", "Info")
	nav := [][2]string{{"/", dlNav}, {rel, relNav}, {"/about/", aboutNav}}
	major, minor := g.n(2, 6), g.n(0, 9)
	type release struct {
		ver, date string
		at        time.Time
	}
	var rels []release
	day := g.n(3, 20)
	for i := 0; i < 6; i++ {
		at := g.base.Add(-time.Duration(day) * 24 * time.Hour)
		rels = append(rels, release{fmt.Sprintf("%d.%d.%d", major, minor, 6-i), g.date(at), at})
		day += g.n(12, 40)
	}
	slug := strings.ToLower(strings.ReplaceAll(product, " ", "-"))
	tok := osTokens[g.r.IntN(len(osTokens))]
	oses := [][2]string{{tok[0], "Windows"}, {tok[1], "macOS"}, {tok[2], "Linux"}}
	s.addScript(g, scriptParts{forms: true, os: true, osTok: tok, latest: "releases/latest", formKey: g.sk.formKey, osKey: g.sk.osKey})
	bold := g.pick([]string{"b", "b", "strong"})
	type file struct {
		name, size, sha, os, key string
		mb                       int
	}
	// 安装包文件名的写法也按种子换
	names := [][3]string{{"%s-%s-x64-setup.exe", "%s-%s-universal.dmg", "%s-%s-x86_64.AppImage"}, {"%s-setup-%s.exe", "%s-%s.dmg", "%s-%s.AppImage"},
		{"%s_%s_x64.msi", "%s_%s_universal.dmg", "%s_%s_amd64.deb"}, {"%s-%s-win64.exe", "%s-%s-macos.dmg", "%s-%s-linux-x64.tar.gz"},
		{"%s-%s-windows-x64.zip", "%s-%s-mac.zip", "%s-%s-linux.tar.xz"}}[g.r.IntN(5)]
	files := func(ver string) []file {
		var out []file
		for i, o := range oses {
			f := file{os: o[1], key: o[0], sha: g.hex(64), name: fmt.Sprintf(names[i], slug, ver)}
			f.mb = g.n(90, 260) + 20*i
			f.size = mb(f.mb)
			out = append(out, f)
		}
		return out
	}
	r, mono, sub := g.c("r"), g.c("mono"), g.c("sub")
	// 这个站里反复出现的几句,先挑好
	dlBtn := g.say("Download", "Download", "Get", "Download now", "Install")
	thanks := g.say("Thanks for downloading %s", "Downloading %s", "Your download of %s is starting", "%s is on its way", "Thank you for trying %s", "Almost there: %s")
	thanksText := [2]string{g.say("Your download should begin in a few seconds. If it doesn't, check your connection and ", "The download starts automatically. If nothing happens, ",
		"Your browser should start the download shortly. If it does not, ", "Hang on, the file is being prepared. If it takes more than a minute, ", "The download will start by itself. Stuck? You can "),
		g.say("start it again from the release page", "try again from the release page", "go back to the release page and retry", "restart it from the list of files", "pick the file again on the release page")}
	relWord := g.say("Released %s", "Published %s", "Out since %s", "Release date: %s", "Shipped %s")
	newHead, filesHead := g.say("What's new", "Changes", "In this release", "Release notes", "Highlights"), g.say("Files", "Downloads", "Installers", "Packages", "Get this version")
	cols := [3]string{g.say("File", "Name", "Package", "Installer"), g.say("Size", "Size", "Download size"), g.say("SHA-256", "Checksum (SHA-256)", "SHA-256 checksum", "sha256")}
	var cards, relList strings.Builder
	var latest []map[string]any
	var history []map[string]any
	jk := g.jsonKey
	for i, rl := range rels {
		fmt.Fprintf(&relList, `<tr><td><a href="%s%s/">%s %s</a></td><td class="%s %s">%s</td></tr>`, rel, rl.ver, esc(product), rl.ver, r, g.c("muted"), rl.date)
		var rows, changes strings.Builder
		for _, f := range files(rl.ver) {
			p := rel + rl.ver + "/" + f.name
			fmt.Fprintf(&rows, `<tr><td><a class="%s" href="%s">%s</a></td><td class="%s">%s</td><td class="%s %s %s">%s…</td></tr>`, g.c("file"), p, esc(f.name), r, f.size, g.c("hide"), mono, g.c("muted"), f.sha[:16])
			// 点了下载:一页「谢谢下载」(下载链接本身是给浏览器的,这里不放安装包)
			s.add(p, htmlType, layout(g, s, p, g.title(f.name, s.name), nav,
				fmt.Sprintf(`<div class="%s">%s<div><h1>%s</h1><p class="%s">%s%s%s%s%s</p></div></div><p>%s<a href="%s%s/">%s</a>.</p><p class="%s">%s: <span class="%s">%s</span></p>`,
					g.c("head2"), g.img("", icon, 0, 0, false), esc(fmt.Sprintf(thanks, product)), sub, f.os, dot, rl.ver, dot, f.size, esc(thanksText[0]), rel, rl.ver, esc(thanksText[1]), g.c("muted"), esc(cols[2]), mono, f.sha)))
			if i == 0 {
				fmt.Fprintf(&cards, `<div class="%s" data-%s="%s"><%s>%s</%s><div class="%s">%s%s%s</div><a class="%s" href="%s">%s</a></div>`,
					g.c("card"), g.sk.osKey, f.key, bold, f.os, bold, g.c("muted"), esc(f.name), dot, f.size, g.c("btn"), p, esc(dlBtn))
				latest = append(latest, map[string]any{"platform": f.key, jk("file_name"): f.name, jk("size_bytes"): f.mb<<20 + g.n(0, 1<<20), "sha256": f.sha, "url": p})
			}
		}
		history = append(history, map[string]any{"version": rl.ver, jk("released_at"): rl.at.UTC().Format(time.RFC3339), "url": rel + rl.ver + "/"})
		for _, n := range g.pickN(releaseNotes, g.n(2, 4)) {
			fmt.Fprintf(&changes, `<li>%s</li>`, esc(g.alt(n)))
		}
		rp := rel + rl.ver + "/"
		s.add(rp, htmlType, layout(g, s, rp, g.title(product+" "+rl.ver, s.name), nav,
			fmt.Sprintf(`<h1>%s %s</h1><p class="%s">%s</p><h2>%s</h2><ul>%s</ul><h2>%s</h2><div class="%s"><table><thead><tr><th>%s</th><th class="%s">%s</th><th class="%s">%s</th></tr></thead><tbody>%s</tbody></table></div>`,
				esc(product), rl.ver, sub, esc(fmt.Sprintf(relWord, rl.date)), esc(newHead), changes.String(), esc(filesHead), g.c("card"), esc(cols[0]), r, esc(cols[1]), g.c("hide"), esc(cols[2]), rows.String())))
	}
	var feats strings.Builder
	svgOpen := fmt.Sprintf(g.pick(iconOpen), g.pick([]string{"2", "2", "1.5", "1.8", "1.75", "2.2"})) // 一个站里几个图标同一种写法
	for _, i := range g.r.Perm(len(featureTexts))[:3] {
		fmt.Fprintf(&feats, `<div class="%s"><div class="%s">%s%s</svg></div><h3>%s</h3><p>%s</p></div>`,
			g.c("feat"), g.c("ficon"), svgOpen, featureIcon(g, i), esc(g.alt(featureTexts[i][0])), esc(g.alt(featureTexts[i][1])))
	}
	st := subTexts{
		title: g.say("Release notes by email", "Get notified about updates", "Update announcements", "Hear about new versions", "Stay up to date", "New version alerts"),
		note: g.say("One email when a new version is out. No spam.", "Get the release notes by email when we ship an update.", "We'll email you when a new version is available, about once a month.",
			"A short email for each release, nothing else.", "Leave your email and we'll tell you when {an update|the next version} is ready.", "Occasional emails about new releases. Unsubscribe any time."),
		button: g.say("Subscribe", "Notify me", "Sign up", "Subscribe", "Keep me posted", "Join the list"),
		label:  g.say("Email", "Email address", "Your email", "Email"),
	}
	subForm := subscribePage(g, s, nav, g.say("Subscribe", "Release notes by email", "Update notifications", "Mailing list", "Get notified"), st)
	s.subscribeAPI(g, subForm)
	s.getJSON(s.apiPre+"releases/latest", http.StatusOK, map[string]any{"name": product, "version": rels[0].ver, jk("released_at"): rels[0].at.UTC().Format(time.RFC3339), "files": latest})
	s.getJSON(s.apiPre+"releases", http.StatusOK, map[string]any{"data": history})
	headline := fmt.Sprintf(g.say("Download %s", "Get %s", "%s for desktop", "Install %s", "%s", "%s for Windows, macOS and Linux"), product)
	verLine := fmt.Sprintf(g.say("Version %[1]s%[5]sreleased %[2]s%[5]s%[3]s", "v%[1]s, released %[2]s. %[4]s.", "Latest: %[1]s (%[2]s)%[5]s%[3]s", "Current version %[1]s%[5]s%[2]s", "%[1]s%[5]s%[2]s%[5]s%[3]s"),
		rels[0].ver, rels[0].date, free, capFirst(free), dot)
	s.add("/", htmlType, layout(g, s, "/", g.title(dlNav, product), nav,
		fmt.Sprintf(`<div class="%s">%s<div><h1>%s</h1><p class="%s">%s</p></div></div>`+
			`<div class="%s">%s</div>%s<div class="%s">%s</div>%s<h2>%s</h2><div class="%s"><table><tbody>%s</tbody></table></div>`,
			g.c("head2"), g.img("", icon, 0, 0, false), esc(headline), sub, esc(verLine), g.c("dl"), cards.String(), mock, g.c("feats"), feats.String(),
			subscribeBox(g, s, st),
			esc(g.say("Release history", "Previous versions", "Changelog", "All releases", "Older releases", "Version history")), g.c("card"), relList.String())))
	s.add(rel, htmlType, layout(g, s, rel, g.title(relNav, product), nav,
		fmt.Sprintf(`<h1>%s</h1><p class="%s">%s</p><div class="%s"><table><tbody>%s</tbody></table></div>`, esc(relNav), sub,
			esc(fmt.Sprintf(g.say("All versions of %s.", "Every release of %s, newest first.", "%s release archive.", "Older builds of %s stay available here.", "Version history for %s."), product)), g.c("card"), relList.String())))
	s.add("/about/", htmlType, layout(g, s, "/about/", g.title(aboutNav, product), nav,
		fmt.Sprintf(`<h1>%s</h1><p class="%s">%s</p>%s`, esc(fill(g.say("About %s", "About", "About the app", "What is %s?", "The story of %s"), product)), sub,
			esc(fmt.Sprintf(g.say("Made by %s.", "A product of %s.", "Built and maintained by %s.", "%s makes this.", "Developed by %s."), strings.TrimSuffix(maker, "."))) /* 「& Co.」后面不再加句号 */, productAbout(g, product))))
	s.setNotFound(layout(g, s, "", nfTitle(g, s.name), nav, notFoundBody(g)))
}
