package camo

// 下载镜像(伪装站的一种):项目列表、目录页、几个真能打开的小文件、流量图、同步状态接口。
//
// 页面上给人看的句子都按种子挑说法(text.go 的 say):所有 m-ui 共用这一份代码,写死的句子拿一句去搜就能找出全部伪装站。
// 原来首页的「Machine-readable status:」「Traffic, last 30 days」和那句带同步间隔的介绍各台一字不差(审查 F-H1)。
// 项目也从 18 个加到 36 个、每个站挑 9~14 个,每个项目的说明有两三种写法:目录名是真镜像上都有的名字,本身不是特征。

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var mirrorProjects = []struct {
	dir  string
	desc []string
}{
	{"almalinux", []string{"AlmaLinux OS", "AlmaLinux", "AlmaLinux releases"}},
	{"alpine", []string{"Alpine Linux", "Alpine Linux packages", "Alpine releases"}},
	{"archlinux", []string{"Arch Linux packages and images", "Arch Linux", "Arch Linux repositories"}},
	{"centos-stream", []string{"CentOS Stream", "CentOS Stream releases", "CentOS Stream packages"}},
	{"debian", []string{"Debian archive", "Debian packages", "Debian package archive"}},
	{"debian-cd", []string{"Debian installer images", "Debian CD and DVD images", "Debian ISO images"}},
	{"fedora", []string{"Fedora releases", "Fedora Linux", "Fedora packages and images"}},
	{"freebsd", []string{"FreeBSD releases", "FreeBSD", "FreeBSD release images"}},
	{"gentoo", []string{"Gentoo distfiles", "Gentoo", "Gentoo sources"}},
	{"kali", []string{"Kali Linux", "Kali Linux packages", "Kali images and packages"}},
	{"linuxmint", []string{"Linux Mint images", "Linux Mint", "Linux Mint ISO images"}},
	{"manjaro", []string{"Manjaro packages", "Manjaro Linux", "Manjaro repositories"}},
	{"opensuse", []string{"openSUSE distributions", "openSUSE", "openSUSE Leap and Tumbleweed"}},
	{"openwrt", []string{"OpenWrt firmware", "OpenWrt", "OpenWrt releases and packages"}},
	{"raspberrypi", []string{"Raspberry Pi OS images", "Raspberry Pi OS", "Raspberry Pi images"}},
	{"rocky", []string{"Rocky Linux", "Rocky Linux releases", "Rocky Linux packages"}},
	{"ubuntu", []string{"Ubuntu archive", "Ubuntu packages", "Ubuntu package archive"}},
	{"ubuntu-releases", []string{"Ubuntu release images", "Ubuntu ISO images", "Ubuntu installation images"}},
	{"void", []string{"Void Linux", "Void Linux packages", "Void packages and live images"}},
	{"nixos", []string{"NixOS channels", "NixOS", "NixOS images"}},
	{"mxlinux", []string{"MX Linux", "MX Linux images", "MX Linux packages"}},
	{"endeavouros", []string{"EndeavourOS", "EndeavourOS images", "EndeavourOS repository"}},
	{"slackware", []string{"Slackware", "Slackware releases", "Slackware Linux"}},
	{"tails", []string{"Tails images", "Tails", "Tails releases"}},
	{"armbian", []string{"Armbian images", "Armbian", "Armbian packages and images"}},
	{"termux", []string{"Termux packages", "Termux", "Termux repositories"}},
	{"cygwin", []string{"Cygwin packages", "Cygwin", "Cygwin mirror"}},
	{"ctan", []string{"CTAN (TeX archive)", "CTAN", "TeX archive network"}},
	{"cpan", []string{"CPAN (Perl archive)", "CPAN", "Perl module archive"}},
	{"gnu", []string{"GNU software", "GNU", "GNU project releases"}},
	{"kernel", []string{"Linux kernel sources", "kernel.org", "Linux kernel archives"}},
	{"videolan", []string{"VideoLAN downloads", "VideoLAN", "VLC and VideoLAN releases"}},
	{"libreoffice", []string{"LibreOffice releases", "LibreOffice", "LibreOffice installers"}},
	{"mariadb", []string{"MariaDB packages", "MariaDB", "MariaDB server releases"}},
	{"kde", []string{"KDE software", "KDE", "KDE releases"}},
	{"qt", []string{"Qt releases", "Qt", "Qt downloads"}},
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
	s.name = g.pick(words) + " " + g.pick([]string{"Mirror", "Open Mirror", "Software Mirror", "Mirrors", "Mirror Service", "FOSS Mirror", "Linux Mirror", "Download Mirror", "Archive", "Mirror Network"})
	s.logo = `<span class="` + g.c("logo") + `"></span>`
	every := g.pick([]string{"4", "6", "8", "12"})
	s.foot = `<span>` + esc(fmt.Sprintf(g.say("Synchronized every %s hours from the official upstream servers", "Synced with upstream every %s hours",
		"Mirrors refresh every %s hours", "Updated from upstream every %s h", "Sync interval: %s hours", "Files are synced from the official servers every %s hours",
		"Re-synced every %s hours"), every)) + `</span>`
	home := g.say("Projects", "Mirrors", "Distros", "Browse", "Index", "All projects")
	aboutNav := g.say("About", "About", "About us", "Info", "Help")
	nav := [][2]string{{"/", home}, {"/about/", aboutNav}}
	r, mono, dirc, filec := g.c("r"), g.c("mono"), g.c("dir"), g.c("file")
	lead := fmt.Sprintf(g.say("Open-source software mirror, synchronized with upstream every %s hours.",
		"A public mirror of Linux distributions and free software, updated every %s hours.",
		"Free software mirror. Everything here is synced from upstream every %s hours.",
		"Mirror of open-source projects, refreshed from the official servers every %s hours.",
		"Linux and BSD mirror, kept in sync with upstream every %s hours.",
		"We mirror {popular|widely used} open-source projects and re-sync them every %s hours.",
		"Packages and install images for {Linux|open-source} projects, re-synced every %s hours."), every)
	s.desc = s.name + ": " + strings.ToLower(lead[:1]) + lead[1:]
	everyN, _ := strconv.Atoi(every)
	statusPath := g.pick([]string{"/status.json", "/api/status", "/mirrors.json", "/api/v1/status", "/sync.json", "/api/mirrors"})
	if strings.HasPrefix(statusPath, "/api/") {
		s.apiPre = statusPath[:strings.LastIndex(statusPath, "/")+1]
	}
	// 同步状态的写法(一个站里各行同一种)
	syncStyle := g.r.IntN(4)
	syncing := g.say("Syncing", "In progress", "Updating", "Sync running", "Syncing now")
	synced := func(min int) string {
		h := min / 60
		switch {
		case syncStyle == 0 && min < 60:
			return fmt.Sprintf("Synced %d min ago", min)
		case syncStyle == 0:
			return fmt.Sprintf("Synced %dh ago", h)
		case syncStyle == 1 && min < 60:
			return fmt.Sprintf("%d minutes ago", min)
		case syncStyle == 1 && h == 1:
			return "1 hour ago"
		case syncStyle == 1:
			return fmt.Sprintf("%d hours ago", h)
		case syncStyle == 2 && min < 60:
			return fmt.Sprintf("OK · %dm", min)
		case syncStyle == 2:
			return fmt.Sprintf("OK · %dh %02dm", h, min%60)
		case min < 60:
			return fmt.Sprintf("Up to date (%d min)", min)
		}
		return fmt.Sprintf("Up to date (%d h)", h)
	}
	slash := g.pick([]string{"/", "/", ""}) // 列表里目录名后面带不带斜杠
	ix := indexTexts{                       // 目录页上那几处固定的字(一个站里各页一致)
		head:  g.pick([]string{"Index of %s", "Index of %s", "Directory listing for %s", "%s", "Contents of %s", "Files in %s"}),
		cols:  [3]string{g.say("Name", "File", "File name", "Filename"), g.say("Size", "Size", "File size"), g.say("Last modified", "Modified", "Date", "Last change", "Updated")},
		up:    g.pick([]string{"../", "..", "Parent directory", "Parent Directory", "[parent]", "Up one level"}),
		blank: g.pick([]string{"", "-", "", "—"}),
	}
	chosen := g.r.Perm(len(mirrorProjects))[:g.n(9, 14)]
	sortInts(chosen) // 按目录名的先后列(mirrorProjects 本身是排好的)
	busy := g.r.IntN(len(chosen))
	var rows strings.Builder
	var projects []map[string]any
	total := 0
	for i, pi := range chosen {
		name, desc := mirrorProjects[pi].dir, g.pick(mirrorProjects[pi].desc)
		gb := g.n(18, 2400)
		total += gb
		ago := g.n(8, everyN*60-12) // 分钟:同步间隔以内
		status := fmt.Sprintf(`<span class="%s">%s</span>`, g.c("ok"), esc(synced(ago)))
		state := "synced"
		if i == busy {
			status = fmt.Sprintf(`<span class="%s">%s</span>`, g.c("busy"), esc(syncing))
			state = "syncing"
		}
		projects = append(projects, map[string]any{"name": name, "status": state, g.jsonKey("last_sync"): g.base.Add(-time.Duration(ago) * time.Minute).UTC().Format(time.RFC3339),
			g.jsonKey("size_bytes"): int64(gb) << 30, "path": "/" + name + "/"})
		fmt.Fprintf(&rows, `<tr><td><a class="%s" href="/%s/">%s%s</a></td><td class="%s %s">%s</td><td class="%s %s">%s</td><td class="%s">%s</td></tr>`,
			dirc, name, name, slash, g.c("hide"), g.c("muted"), esc(desc), r+" "+g.c("hide"), mono, size(gb), r, status)
		ver := fmt.Sprintf("%d.%d", g.n(3, 42), g.n(0, 9))
		var list strings.Builder
		for _, d := range g.pickN(mirrorDirs, g.n(2, 4)) {
			date := g.daysAgo(g.n(0, 9))
			fmt.Fprintf(&list, `<tr><td><a class="%s" href="/%s/%s/">%s%s</a></td><td class="%s %s">%s</td><td class="%s %s">%s</td></tr>`, dirc, name, d, d, slash, r, mono, ix.blank, r, mono, date)
			var sub strings.Builder
			for _, fn := range mirrorDirFiles(d) {
				body, ctype := mirrorFile(g, name, desc, ver, d, fn)
				p := "/" + name + "/" + d + "/" + fn
				s.add(p, ctype, body)
				fmt.Fprintf(&sub, `<tr><td><a class="%s" href="%s">%s</a></td><td class="%s %s">%s</td><td class="%s %s">%s</td></tr>`, filec, p, fn, r, mono, humanSize(len(body)), r, mono, date)
			}
			dp := "/" + name + "/" + d + "/"
			s.add(dp, htmlType, layout(g, s, dp, g.title(fmt.Sprintf(ix.head, dp), s.name), nav, indexBody(g, ix, home, dp, "/"+name+"/", desc, sub.String())))
		}
		for _, fn := range []string{"README", "SHA256SUMS", "SHA256SUMS.sig"} {
			body, ctype := mirrorFile(g, name, desc, ver, "", fn)
			p := "/" + name + "/" + fn
			s.add(p, ctype, body)
			fmt.Fprintf(&list, `<tr><td><a class="%s" href="%s">%s</a></td><td class="%s %s">%s</td><td class="%s %s">%s</td></tr>`, filec, p, fn, r, mono, humanSize(len(body)), r, mono, g.daysAgo(g.n(1, 30)))
		}
		pp := "/" + name + "/"
		s.add(pp, htmlType, layout(g, s, pp, g.title(fmt.Sprintf(ix.head, pp), s.name), nav, indexBody(g, ix, home, pp, "/", desc, list.String())))
	}
	cw, ch := g.sk.chart[0], g.sk.chart[1]
	chart, peak, avg := trafficChart(g, float64(cw), float64(ch))
	chartImg := s.asset(g, "svg", svgType, chart)
	var axis strings.Builder
	today := g.say("Today", "Now", "Today", "Latest")
	ticks, axisFmt, last := g.n(4, 6), g.pick([]string{"Jan 2", "2 Jan", "01-02", "Jan 02", "2.1.", "1/2"}), g.n(0, 4) // 最右边是几天前(0 = 今天)
	for i := 0; i < ticks; i++ {
		d := last + 29 - (i*29+(ticks-1)/2)/(ticks-1)
		label := g.base.Add(-time.Duration(d) * 24 * time.Hour).Format(axisFmt)
		if d == 0 {
			label = today
		}
		fmt.Fprintf(&axis, `<span>%s</span>`, label)
	}
	s.getJSON(statusPath, http.StatusOK, map[string]any{"name": s.name, g.jsonKey("updated_at"): g.base.UTC().Format(time.RFC3339), g.jsonKey("sync_interval_hours"): everyN, "projects": projects})
	stat := func(v, label string) string {
		return fmt.Sprintf(`<div class="%s"><b>%s</b><span>%s</span></div>`, g.c("stat"), esc(v), esc(label))
	}
	// 第四张统计卡各台不一样(原来都是「IPv4 / IPv6 · HTTP and HTTPS」)
	extra := [][2]string{{"IPv4 / IPv6", "HTTP and HTTPS"}, {"Dual-stack", "IPv4 and IPv6"}, {"HTTPS", "also over plain HTTP"}, {every + " h", "sync interval"},
		{"99.9%", "availability, 90 days"}, {"IPv6 ready", "and IPv4"}}[g.r.IntN(6)]
	heads := [4]string{g.say("Project", "Name", "Distribution", "Mirror"), g.say("Description", "About", "Contents", "What it is"), g.say("Size", "On disk", "Size"), g.say("Status", "Last sync", "Sync", "State")}
	top := fmt.Sprintf(`<h1>%s</h1><p class="%s">%s</p><div class="%s">%s%s%s%s</div>`, esc(s.name), g.c("sub"), esc(lead), g.c("stats"),
		stat(strconv.Itoa(len(chosen)), g.say("projects", "mirrored projects", "distributions", "projects hosted")), stat(size(total), g.say("total size", "on disk", "mirrored", "of files")),
		stat(g.pick([]string{"10 Gbps", "20 Gbps", "25 Gbps", "40 Gbps"}), g.say("uplink", "bandwidth", "network", "connection")), stat(extra[0], extra[1]))
	table := fmt.Sprintf(`<div class="%s"><table><thead><tr><th>%s</th><th class="%s">%s</th><th class="%s">%s</th><th class="%s">%s</th></tr></thead><tbody>%s</tbody></table></div>`,
		g.c("card"), esc(heads[0]), g.c("hide"), esc(heads[1]), r+" "+g.c("hide"), esc(heads[2]), r, esc(heads[3]), rows.String())
	note := fmt.Sprintf(`<p class="%s %s">%s <a href="%s">%s</a></p>`, g.c("muted"), g.c("fine"),
		esc(g.say("Machine-readable status:", "Status as JSON:", "For scripts:", "Sync status feed:", "Mirror status (JSON):", "Automation endpoint:")), statusPath, statusPath)
	traffic := fmt.Sprintf(`<h2>%s</h2><p class="%s %s">%s</p><div class="%s">%s<div class="%s">%s</div></div>`,
		esc(g.say("Traffic, last 30 days", "Outbound traffic (30 days)", "Bandwidth over the last month", "The last 30 days", "Served in the past 30 days", "Monthly traffic")),
		g.c("muted"), g.c("cap"), esc(fmt.Sprintf(g.say("Peak %.1f TB / day · average %.1f TB / day", "%.1f TB/day at peak, %.1f TB/day on average",
			"Daily average %.1[2]f TB (peak %.1[1]f TB)", "Avg %.1[2]f TB · max %.1[1]f TB per day", "Busiest day %.1f TB; typical day %.1f TB"), peak, avg)),
		g.c("chart"), g.img("", chartImg, cw, ch, false), g.c("axis"), axis.String())
	// 「同步状态接口」那一行放在表格下面、表格上面还是页面最后,按种子定(放的位置固定的话,表格收尾接着这一行的那段标记各台一字不差)
	body := top + table + note + traffic
	switch g.r.IntN(3) {
	case 1:
		body = top + note + table + traffic
	case 2:
		body = top + table + traffic + note
	}
	s.add("/", htmlType, layout(g, s, "/", s.name, nav, body))
	aboutHead := g.say("About this mirror", "About", "About the service", "Who runs this", "About us", "What this is")
	s.add("/about/", htmlType, layout(g, s, "/about/", g.title(aboutNav, s.name), nav,
		`<h1>`+esc(aboutHead)+`</h1><p class="`+g.c("sub")+`">`+esc(g.say("A public mirror for open-source projects.", "A free mirror for the open-source community.",
			"Run by volunteers, open to everyone.", "Open-source software, close to you.", "A community mirror, free to use.", "Public mirror service."))+`</p>`+mirrorAbout(g)))
	s.setNotFound(layout(g, s, "", nfTitle(g, s.name), nav, notFoundBody(g)))
}

// sortInts 小切片从小到大排(项目下标)
func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// indexTexts 目录页上固定的那几处:标题的写法(%s 是路径)、三列的表头、回上一级那一行、空着的格子里写什么
type indexTexts struct {
	head  string
	cols  [3]string
	up    string
	blank string
}

// indexBody 目录页:面包屑(root 是首页在导航里的叫法)、标题、文件列表
func indexBody(g *gen, ix indexTexts, root, path, parent, desc, rows string) string {
	var crumbs strings.Builder
	crumbs.WriteString(`<a href="/">` + esc(root) + `</a>`)
	acc := "/"
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		acc += part + "/"
		if acc == path {
			fmt.Fprintf(&crumbs, ` / %s`, esc(part))
		} else {
			fmt.Fprintf(&crumbs, ` / <a href="%s">%s</a>`, acc, esc(part))
		}
	}
	return fmt.Sprintf(`<p class="%s">%s</p><h1>%s</h1><p class="%s">%s</p><div class="%s"><table><thead><tr><th>%s</th><th class="%s">%s</th><th class="%s">%s</th></tr></thead><tbody><tr><td><a class="%s" href="%s">%s</a></td><td class="%s">%s</td><td class="%s">%s</td></tr>%s</tbody></table></div>`,
		g.c("crumbs"), crumbs.String(), esc(fmt.Sprintf(ix.head, path)), g.c("sub"), esc(desc), g.c("card"), esc(ix.cols[0]), g.c("r"), esc(ix.cols[1]), g.c("r"), esc(ix.cols[2]),
		g.c("dir"), parent, esc(ix.up), g.c("r"), ix.blank, g.c("r"), ix.blank, rows)
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
		return []byte(fmt.Sprintf("%s\n%s\n\n%s\n%s\n\nLast sync: %s UTC\n", desc, strings.Repeat("=", len(desc)),
			fmt.Sprintf(g.say("This directory is synchronized from the official %s servers several times a day.", "Mirror of the official %s servers, updated {several times a day|every few hours}.",
				"These files come straight from the %s project; we re-sync {a few|several} times a day.", "Synced from upstream (%s). Nothing here is modified."), desc),
			g.say("Files are served exactly as published upstream. Please verify what you download with the\nchecksums and signatures that come with each release.",
				"Always check downloads against the checksums and signatures published by the project.",
				"We serve the files unchanged. Verify them with the project's checksums before use.",
				"Use SHA256SUMS and the detached signature in each directory to verify what you download."),
			g.daysAgo(g.n(0, 2)))), textType
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
