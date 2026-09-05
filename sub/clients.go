package sub

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/brand"
)

// 客户端下载页:订阅页"一键导入"旁边那个下载箭头点开的就是这里。
//
// 页面本身不含任何用户数据,只是把各系统能用的客户端讲清楚 + 给可用的下载地址。
// 每个系统列几款都能吃这套订阅的客户端:第一款是推荐项(页面上有个小"推荐"标),其余按常用程度排;
// 每款都注明该粘贴哪种地址(Clash / sing-box / 通用),和订阅页上三个地址一一对应。
// 版本号写死是有意的:直链必须指向确实存在的文件,版本升级时改这一处即可;文件名不带版本的项目
// (Hiddify、v2rayN)走 releases/latest/download,永远是最新。每块都留了"全部版本"入口。
//
//go:embed clients.html
var clientsFS embed.FS

var clientsTmpl = template.Must(template.ParseFS(clientsFS, "clients.html"))

const (
	verVerge   = "2.5.2"   // Clash Verge Rev(Windows / macOS / Linux)
	verCMFA    = "2.11.33" // Clash Meta for Android
	verFlClash = "0.8.96"  // FlClash(Android / Windows / macOS / Linux)
	verSingBox = "1.14.0"  // sing-box 官方客户端 SFA(Android)/ SFW(Windows)与命令行版
	ghMirror   = "https://ghfast.top/"
)

// dl 是一个下载按钮。Primary 的那个是这款客户端里首选的包。
type dl struct {
	Text    string
	Href    template.URL
	Primary bool
	Muted   bool // 次要入口(镜像 / 全部版本),弱化显示
	Mirror  bool // 国内镜像:同一个文件经 ghfast.top 中转,大陆网络环境可直接下载
}

// clientApp 是某个系统上的一款客户端。
type clientApp struct {
	Name        string
	Recommended bool   // 该系统首选,页面上打一个小"推荐"标
	Format      string // 该粘贴订阅页上的哪个地址:Clash / sing-box / 通用
	Desc        string // 一两句话:怎么装、怎么导入
	Links       []dl
}

type clientTile struct {
	Key        string // ios / android / windows / macos / linux
	Icon       template.HTML
	OS         string // 图标下方那行系统名
	Apps       []clientApp
	MirrorNote string // 这块里有镜像链接时,面板底部的一行说明
}

type clientsData struct {
	Lang       string
	Icon       template.URL
	Title      string
	Back       string // 返回订阅页
	Tiles      []clientTile
	Year       int
	MirrorHint string // 镜像按钮的悬停提示
}

// mirrorOf 把 GitHub 直链换成镜像站直链;不是 GitHub 的(App Store)没有镜像。
func mirrorOf(href template.URL) template.URL {
	if !strings.HasPrefix(string(href), "https://github.com/") {
		return ""
	}
	return template.URL(ghMirror + string(href))
}

// addMirrors 给每款客户端补一个首选包的国内镜像(已经写了的不重复),并给有镜像的系统块配一行说明。
// 放在数据之后统一处理,新增客户端时不会漏掉。
func addMirrors(tiles []clientTile, zh bool) []clientTile {
	text := "国内镜像下载"
	note := "「国内镜像下载」经 ghfast.top 中转,大陆网络环境可直接下载,文件与官方发布页相同;App Store 应用没有镜像。"
	if !zh {
		text = "China mirror"
		note = "“China mirror” links are the same files proxied by ghfast.top, so they download from mainland China; App Store apps have no mirror."
	}
	for ti := range tiles {
		has := false
		for ai := range tiles[ti].Apps {
			app := &tiles[ti].Apps[ai]
			var primary template.URL
			hasMirror := false
			for _, l := range app.Links {
				if l.Primary {
					primary = l.Href
				}
				if l.Mirror {
					hasMirror = true
				}
			}
			if !hasMirror {
				if m := mirrorOf(primary); m != "" {
					// 插在"全部版本"之前:主包、次包、镜像、全部版本
					links := make([]dl, 0, len(app.Links)+1)
					inserted := false
					for _, l := range app.Links {
						if l.Muted && !inserted {
							links = append(links, dl{Text: text, Href: m, Muted: true, Mirror: true})
							inserted = true
						}
						links = append(links, l)
					}
					if !inserted {
						links = append(links, dl{Text: text, Href: m, Muted: true, Mirror: true})
					}
					app.Links = links
					hasMirror = true
				}
			}
			if hasMirror {
				has = true
			}
		}
		if has {
			tiles[ti].MirrorNote = note
		}
	}
	return tiles
}

func gh(repo, tag, file string) template.URL {
	return template.URL("https://github.com/" + repo + "/releases/download/" + tag + "/" + file)
}

func ghm(repo, tag, file string) template.URL {
	return template.URL(ghMirror + "https://github.com/" + repo + "/releases/download/" + tag + "/" + file)
}

// ghLatestDL:文件名不带版本号的项目,直接取最新 Release 里的那个文件。
func ghLatestDL(repo, file string) template.URL {
	return template.URL("https://github.com/" + repo + "/releases/latest/download/" + file)
}

func ghLatest(repo string) template.URL {
	return template.URL("https://github.com/" + repo + "/releases/latest")
}

// 图标用各系统自己的标志,线条统一 currentColor,和面板的图标风格一致。
const (
	iconApple = `<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M17.05 12.9c-.03-2.4 1.96-3.55 2.05-3.61-1.12-1.63-2.86-1.86-3.48-1.88-1.48-.15-2.89.87-3.64.87s-1.91-.85-3.14-.83c-1.61.02-3.1.94-3.93 2.38-1.68 2.91-.43 7.22 1.2 9.58.8 1.15 1.75 2.45 3 2.4 1.2-.05 1.66-.78 3.11-.78s1.86.78 3.13.75c1.29-.02 2.11-1.17 2.9-2.33.91-1.33 1.29-2.62 1.31-2.69-.03-.01-2.51-.96-2.53-3.82ZM14.7 5.85c.66-.8 1.11-1.92.99-3.03-.95.04-2.11.63-2.8 1.43-.61.71-1.15 1.85-1.01 2.94 1.07.08 2.15-.54 2.82-1.34Z"/></svg>`
	iconMac   = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3.5" y="5" width="17" height="11" rx="1.8"/><path d="M2 19h20"/><path d="M10.5 19h3"/></svg>`
	iconAndro = `<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M6.9 10.4h10.2a.6.6 0 0 1 .6.6v6.6a1.8 1.8 0 0 1-1.8 1.8H8.1a1.8 1.8 0 0 1-1.8-1.8V11a.6.6 0 0 1 .6-.6Z"/><rect x="3" y="10.6" width="2.4" height="6.4" rx="1.2"/><rect x="18.6" y="10.6" width="2.4" height="6.4" rx="1.2"/><rect x="8.4" y="19.2" width="2.4" height="4.2" rx="1.2"/><rect x="13.2" y="19.2" width="2.4" height="4.2" rx="1.2"/><path d="M8.2 9.2a4.2 4.2 0 0 1 7.6 0Z"/><path d="M8.6 5 7.5 3.3M15.4 5l1.1-1.7" stroke="currentColor" stroke-width="1.1" stroke-linecap="round"/><circle cx="9.8" cy="7.4" r=".62" fill="var(--card)"/><circle cx="14.2" cy="7.4" r=".62" fill="var(--card)"/></svg>`
	iconWin   = `<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M3 5.9 10.6 4.8v6.6H3V5.9Zm8.9-1.3L21 3.3v8.1h-9.1V4.6ZM3 12.6h7.6v6.6L3 18.1v-5.5Zm8.9 0H21v8.1l-9.1-1.1v-7Z"/></svg>`
	iconLinux = `<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M12 2.2c-2.4 0-4 1.8-4 4.2v1.5c0 .8-.3 1.4-.8 2.1-1.2 1.7-2 3.4-2.4 5.2-.2.9-.5 1.6-1 2.2-.6.8-.1 1.9.9 2 1.4.2 2.6-.3 3.5-1.2A5.4 5.4 0 0 0 12 21.8c1.4 0 2.7-.6 3.8-1.8.9.9 2.1 1.4 3.5 1.2 1-.1 1.5-1.2.9-2-.5-.6-.8-1.3-1-2.2-.4-1.8-1.2-3.5-2.4-5.2-.5-.7-.8-1.3-.8-2.1V6.4c0-2.4-1.6-4.2-4-4.2Z"/><ellipse cx="10.15" cy="6.5" rx="1.05" ry="1.4" fill="var(--card)"/><ellipse cx="13.85" cy="6.5" rx="1.05" ry="1.4" fill="var(--card)"/><circle cx="10.35" cy="6.75" r=".52"/><circle cx="13.65" cy="6.75" r=".52"/><path d="M12 8c.9 0 1.6.48 1.6 1.05S12.9 10.1 12 10.1s-1.6-.48-1.6-1.05S11.1 8 12 8Z" fill="var(--card)"/><path d="M12 12.1c2 0 3.45 1.75 3.45 3.95 0 2.1-1.5 3.6-3.45 3.6s-3.45-1.5-3.45-3.6c0-2.2 1.45-3.95 3.45-3.95Z" fill="var(--card)"/></svg>`
)

func clientTiles(lang string) []clientTile {
	zh := lang == "zh"
	pick := func(a, b string) string {
		if zh {
			return a
		}
		return b
	}
	fClash, fSing, fAny := pick("Clash 地址", "Clash link"), pick("sing-box 地址", "sing-box link"), pick("通用地址", "universal link")
	mirror, all := pick("国内镜像下载", "China mirror"), pick("全部版本", "All releases")
	const verge, cmfa, flclash, singbox, hiddify, v2rayn = "clash-verge-rev/clash-verge-rev", "MetaCubeX/ClashMetaForAndroid", "chen08209/FlClash", "SagerNet/sing-box", "hiddify/hiddify-app", "2dust/v2rayN"
	tiles := []clientTile{
		{
			Key: "ios", Icon: template.HTML(iconApple), OS: "iOS / iPadOS / Apple TV",
			Apps: []clientApp{
				{Name: "Nextin", Recommended: true, Format: fAny,
					Desc: pick("Nextin 在中国大陆区 App Store 搜不到:把 App Store 切到非大陆区(如美区),或用非大陆 Apple ID 登录,再搜索安装。装好后回订阅页点「Nextin」或粘贴通用地址。",
						"Nextin is not listed in the mainland China App Store. Switch the App Store to another region (e.g. the US) or sign in with a non-mainland Apple ID, then install it and tap Nextin on the subscription page or paste the universal link."),
					Links: []dl{{Text: pick("App Store(美区)", "App Store (US)"), Href: "https://apps.apple.com/us/app/nextin/id6754002454", Primary: true}}},
				{Name: "Shadowrocket", Format: fAny,
					Desc: pick("付费,同样需要非大陆区 Apple ID。添加订阅时粘贴通用地址;订阅名以添加那一刻为准,之后改标题不会跟着变。",
						"Paid, and also needs a non-mainland Apple ID. Paste the universal link when adding the subscription; the name is fixed at the moment you add it."),
					Links: []dl{{Text: pick("App Store(美区)", "App Store (US)"), Href: "https://apps.apple.com/us/app/shadowrocket/id932747118", Primary: true}}},
				{Name: "Hiddify", Format: fAny,
					Desc: pick("免费开源,非大陆区 App Store 可装。新建配置 → 从链接添加,粘贴通用地址或 sing-box 地址都行。",
						"Free and open source, available in non-mainland App Stores. New profile → Add from link, then paste the universal link (the sing-box link works too)."),
					Links: []dl{{Text: pick("App Store(美区)", "App Store (US)"), Href: "https://apps.apple.com/us/app/hiddify-proxy-vpn/id6596777532", Primary: true}}},
			},
		},
		{
			Key: "android", Icon: template.HTML(iconAndro), OS: "Android / Android TV",
			Apps: []clientApp{
				{Name: "Clash Meta for Android", Recommended: true, Format: fClash,
					Desc: pick("下载 APK 安装(系统会提示「允许安装未知来源」,同意即可)。通用版任何机型都能装,机型确定是 64 位手机/电视盒子可以选更小的 arm64 版。",
						"Install the APK (Android will ask you to allow installs from this source). The universal build works on any device; pick arm64 if you know your phone or TV box is 64-bit."),
					Links: []dl{
						{Text: pick("APK 通用版 v"+verCMFA, "APK universal v"+verCMFA), Href: gh(cmfa, "v"+verCMFA, "cmfa-"+verCMFA+"-meta-universal-release.apk"), Primary: true},
						{Text: pick("APK arm64 版", "APK arm64"), Href: gh(cmfa, "v"+verCMFA, "cmfa-"+verCMFA+"-meta-arm64-v8a-release.apk")},
						{Text: mirror, Mirror: true, Href: ghm(cmfa, "v"+verCMFA, "cmfa-"+verCMFA+"-meta-universal-release.apk"), Muted: true},
						{Text: all, Href: ghLatest(cmfa), Muted: true},
					}},
				{Name: "sing-box (SFA)", Format: fSing,
					Desc: pick("sing-box 官方客户端。新建配置 → 远程,粘贴 sing-box 地址。",
						"The official sing-box client. New profile → Remote, paste the sing-box link."),
					Links: []dl{
						{Text: pick("APK 通用版 v"+verSingBox, "APK universal v"+verSingBox), Href: gh(singbox, "v"+verSingBox, "SFA-"+verSingBox+"-universal.apk"), Primary: true},
						{Text: pick("APK arm64 版", "APK arm64"), Href: gh(singbox, "v"+verSingBox, "SFA-"+verSingBox+"-arm64-v8a.apk")},
						{Text: all, Href: ghLatest(singbox), Muted: true},
					}},
				{Name: "FlClash", Format: fClash,
					Desc: pick("界面简洁的 Clash 客户端,支持 Android TV。配置 → 添加 → URL,粘贴 Clash 地址。",
						"A clean Clash client that also runs on Android TV. Profiles → Add → URL, paste the Clash link."),
					Links: []dl{
						{Text: "APK arm64 v" + verFlClash, Href: gh(flclash, "v"+verFlClash, "FlClash-"+verFlClash+"-android-arm64-v8a.apk"), Primary: true},
						{Text: all, Href: ghLatest(flclash), Muted: true},
					}},
				{Name: "Hiddify", Format: fAny,
					Desc: pick("免费开源,一键式。新建配置 → 从链接添加,粘贴通用地址。",
						"Free and open source, one-tap style. New profile → Add from link, paste the universal link."),
					Links: []dl{
						{Text: pick("APK 通用版(最新)", "APK universal (latest)"), Href: ghLatestDL(hiddify, "Hiddify-Android-universal.apk"), Primary: true},
						{Text: pick("APK arm64 版", "APK arm64"), Href: ghLatestDL(hiddify, "Hiddify-Android-arm64.apk")},
						{Text: all, Href: ghLatest(hiddify), Muted: true},
					}},
			},
		},
		{
			Key: "windows", Icon: template.HTML(iconWin), OS: "Windows",
			Apps: []clientApp{
				{Name: "Clash Verge Rev", Recommended: true, Format: fClash,
					Desc: pick("下载安装包一路下一步即可。若提示「Windows 已保护你的电脑」,点「更多信息」→「仍要运行」。装好后在「订阅」里粘贴 Clash 地址。",
						"Run the installer and follow the wizard. If Windows SmartScreen warns you, choose More info → Run anyway, then paste the Clash link under Profiles."),
					Links: []dl{
						{Text: pick("Windows x64 安装包 v"+verVerge, "Windows x64 installer v"+verVerge), Href: gh(verge, "v"+verVerge, "Clash.Verge_"+verVerge+"_x64-setup.exe"), Primary: true},
						{Text: pick("ARM64 安装包", "ARM64 installer"), Href: gh(verge, "v"+verVerge, "Clash.Verge_"+verVerge+"_arm64-setup.exe")},
						{Text: mirror, Mirror: true, Href: ghm(verge, "v"+verVerge, "Clash.Verge_"+verVerge+"_x64-setup.exe"), Muted: true},
						{Text: all, Href: ghLatest(verge), Muted: true},
					}},
				{Name: "FlClash", Format: fClash,
					Desc: pick("界面简洁的 Clash 客户端。配置 → 添加 → URL,粘贴 Clash 地址。",
						"A clean Clash client. Profiles → Add → URL, paste the Clash link."),
					Links: []dl{
						{Text: pick("x64 安装包 v"+verFlClash, "x64 installer v"+verFlClash), Href: gh(flclash, "v"+verFlClash, "FlClash-"+verFlClash+"-windows-amd64-setup.exe"), Primary: true},
						{Text: pick("ARM64 安装包", "ARM64 installer"), Href: gh(flclash, "v"+verFlClash, "FlClash-"+verFlClash+"-windows-arm64-setup.exe")},
						{Text: all, Href: ghLatest(flclash), Muted: true},
					}},
				{Name: "v2rayN", Format: fAny,
					Desc: pick("老牌客户端,解压即用,内置 sing-box 与 Xray 内核。订阅分组 → 添加订阅,粘贴通用地址。",
						"A long-standing client, unzip and run, ships both sing-box and Xray cores. Subscription group → Add, paste the universal link."),
					Links: []dl{
						{Text: pick("x64(最新,zip)", "x64 (latest, zip)"), Href: ghLatestDL(v2rayn, "v2rayN-windows-64-desktop.zip"), Primary: true},
						{Text: pick("ARM64(zip)", "ARM64 (zip)"), Href: ghLatestDL(v2rayn, "v2rayN-windows-arm64-desktop.zip")},
						{Text: all, Href: ghLatest(v2rayn), Muted: true},
					}},
				{Name: "Hiddify", Format: fAny,
					Desc: pick("免费开源,一键式。新建配置 → 从链接添加,粘贴通用地址。",
						"Free and open source, one-tap style. New profile → Add from link, paste the universal link."),
					Links: []dl{
						{Text: pick("x64 安装包(最新)", "x64 installer (latest)"), Href: ghLatestDL(hiddify, "Hiddify-Windows-Setup-x64.exe"), Primary: true},
						{Text: pick("便携版(zip)", "Portable (zip)"), Href: ghLatestDL(hiddify, "Hiddify-Windows-Portable-x64.zip")},
						{Text: all, Href: ghLatest(hiddify), Muted: true},
					}},
				{Name: "sing-box (SFW)", Format: fSing,
					Desc: pick("sing-box 官方客户端。新建配置 → 远程,粘贴 sing-box 地址。",
						"The official sing-box client. New profile → Remote, paste the sing-box link."),
					Links: []dl{
						{Text: pick("x64 安装包 v"+verSingBox, "x64 installer v"+verSingBox), Href: gh(singbox, "v"+verSingBox, "SFW-"+verSingBox+"-x64.exe"), Primary: true},
						{Text: all, Href: ghLatest(singbox), Muted: true},
					}},
			},
		},
		{
			Key: "macos", Icon: template.HTML(iconMac), OS: "macOS",
			Apps: []clientApp{
				{Name: "Clash Verge Rev", Recommended: true, Format: fClash,
					Desc: pick("按芯片选:2020 年后的机型基本都是 Apple 芯片。打开 dmg 把图标拖进「应用程序」即可;首次打开若提示来源不明,到「系统设置 → 隐私与安全性」点「仍要打开」。",
						"Pick the build for your chip (Macs from 2020 on are Apple silicon). Open the dmg and drag the app into Applications; on first launch allow it under System Settings → Privacy & Security."),
					Links: []dl{
						{Text: pick("Apple 芯片 (dmg) v"+verVerge, "Apple silicon (dmg) v"+verVerge), Href: gh(verge, "v"+verVerge, "Clash.Verge_"+verVerge+"_aarch64.dmg"), Primary: true},
						{Text: pick("Intel 芯片 (dmg)", "Intel (dmg)"), Href: gh(verge, "v"+verVerge, "Clash.Verge_"+verVerge+"_x64.dmg")},
						{Text: mirror, Mirror: true, Href: ghm(verge, "v"+verVerge, "Clash.Verge_"+verVerge+"_aarch64.dmg"), Muted: true},
						{Text: all, Href: ghLatest(verge), Muted: true},
					}},
				{Name: "FlClash", Format: fClash,
					Desc: pick("界面简洁的 Clash 客户端。配置 → 添加 → URL,粘贴 Clash 地址。",
						"A clean Clash client. Profiles → Add → URL, paste the Clash link."),
					Links: []dl{
						{Text: pick("Apple 芯片 (dmg) v"+verFlClash, "Apple silicon (dmg) v"+verFlClash), Href: gh(flclash, "v"+verFlClash, "FlClash-"+verFlClash+"-macos-arm64.dmg"), Primary: true},
						{Text: pick("Intel 芯片 (dmg)", "Intel (dmg)"), Href: gh(flclash, "v"+verFlClash, "FlClash-"+verFlClash+"-macos-amd64.dmg")},
						{Text: all, Href: ghLatest(flclash), Muted: true},
					}},
				{Name: "v2rayN", Format: fAny,
					Desc: pick("内置 sing-box 与 Xray 内核。订阅分组 → 添加订阅,粘贴通用地址。",
						"Ships both sing-box and Xray cores. Subscription group → Add, paste the universal link."),
					Links: []dl{
						{Text: pick("Apple 芯片 (dmg,最新)", "Apple silicon (dmg, latest)"), Href: ghLatestDL(v2rayn, "v2rayN-macos-arm64.dmg"), Primary: true},
						{Text: pick("Intel 芯片 (dmg)", "Intel (dmg)"), Href: ghLatestDL(v2rayn, "v2rayN-macos-64.dmg")},
						{Text: all, Href: ghLatest(v2rayn), Muted: true},
					}},
				{Name: "Hiddify", Format: fAny,
					Desc: pick("免费开源,一键式。新建配置 → 从链接添加,粘贴通用地址。",
						"Free and open source, one-tap style. New profile → Add from link, paste the universal link."),
					Links: []dl{
						{Text: pick("dmg(最新,通用)", "dmg (latest, universal)"), Href: ghLatestDL(hiddify, "Hiddify-MacOS.dmg"), Primary: true},
						{Text: all, Href: ghLatest(hiddify), Muted: true},
					}},
			},
		},
		{
			Key: "linux", Icon: template.HTML(iconLinux), OS: "Linux",
			Apps: []clientApp{
				{Name: "Clash Verge Rev", Recommended: true, Format: fClash,
					Desc: pick("Debian / Ubuntu 装 deb,Fedora / RHEL 装 rpm。装好后在「订阅」里粘贴 Clash 地址。",
						"Use the deb on Debian/Ubuntu or the rpm on Fedora/RHEL, then paste the Clash link under Profiles."),
					Links: []dl{
						{Text: "deb (amd64) v" + verVerge, Href: gh(verge, "v"+verVerge, "Clash.Verge_"+verVerge+"_amd64.deb"), Primary: true},
						{Text: "rpm (x86_64) v" + verVerge, Href: gh(verge, "v"+verVerge, "Clash.Verge-"+verVerge+"-1.x86_64.rpm")},
						{Text: mirror, Mirror: true, Href: ghm(verge, "v"+verVerge, "Clash.Verge_"+verVerge+"_amd64.deb"), Muted: true},
						{Text: all, Href: ghLatest(verge), Muted: true},
					}},
				{Name: "FlClash", Format: fClash,
					Desc: pick("不想装包就用 AppImage:下载后加执行权限直接双击运行。配置 → 添加 → URL,粘贴 Clash 地址。",
						"Prefer no install? Grab the AppImage, make it executable and run it. Profiles → Add → URL, paste the Clash link."),
					Links: []dl{
						{Text: "AppImage (amd64) v" + verFlClash, Href: gh(flclash, "v"+verFlClash, "FlClash-"+verFlClash+"-linux-amd64.AppImage"), Primary: true},
						{Text: "deb (amd64)", Href: gh(flclash, "v"+verFlClash, "FlClash-"+verFlClash+"-linux-amd64.deb")},
						{Text: all, Href: ghLatest(flclash), Muted: true},
					}},
				{Name: "Hiddify", Format: fAny,
					Desc: pick("免费开源,一键式。新建配置 → 从链接添加,粘贴通用地址。",
						"Free and open source, one-tap style. New profile → Add from link, paste the universal link."),
					Links: []dl{
						{Text: pick("AppImage (x64,最新)", "AppImage (x64, latest)"), Href: ghLatestDL(hiddify, "Hiddify-Linux-x64-AppImage.AppImage"), Primary: true},
						{Text: "deb (x64)", Href: ghLatestDL(hiddify, "Hiddify-Debian-x64.deb")},
						{Text: all, Href: ghLatest(hiddify), Muted: true},
					}},
				{Name: "v2rayN", Format: fAny,
					Desc: pick("内置 sing-box 与 Xray 内核。订阅分组 → 添加订阅,粘贴通用地址。",
						"Ships both sing-box and Xray cores. Subscription group → Add, paste the universal link."),
					Links: []dl{
						{Text: pick("deb (amd64,最新)", "deb (amd64, latest)"), Href: ghLatestDL(v2rayn, "v2rayN-linux-64.deb"), Primary: true},
						{Text: "rpm (amd64)", Href: ghLatestDL(v2rayn, "v2rayN-linux-rhel-64.rpm")},
						{Text: all, Href: ghLatest(v2rayn), Muted: true},
					}},
				{Name: pick("sing-box(命令行)", "sing-box (command line)"), Format: fSing,
					Desc: pick("服务器或路由器上直接跑内核:把 sing-box 地址返回的 JSON 存成配置文件,sing-box run -c 即可。",
						"Run the core itself on a server or router: save the JSON from the sing-box link as the config and start it with sing-box run -c."),
					Links: []dl{
						{Text: "deb (amd64) v" + verSingBox, Href: gh(singbox, "v"+verSingBox, "sing-box_"+verSingBox+"_linux_amd64.deb"), Primary: true},
						{Text: "deb (arm64)", Href: gh(singbox, "v"+verSingBox, "sing-box_"+verSingBox+"_linux_arm64.deb")},
						{Text: all, Href: ghLatest(singbox), Muted: true},
					}},
			},
		},
	}
	return addMirrors(tiles, zh)
}

// serveClients 输出客户端下载页。页面不带任何用户信息,Back 指回订阅页。
func (s *Server) serveClients(w http.ResponseWriter, r *http.Request, subPath, key, title string) {
	lang := pageLang(r)
	hint := "经 ghfast.top 镜像中转,大陆网络环境可直接下载;文件与官方发布页相同"
	if lang != "zh" {
		hint = "Same file proxied by ghfast.top; downloads from mainland China"
	}
	d := clientsData{
		Lang: lang, Icon: template.URL(brand.DataURI), Title: title,
		Back: publicBase(r, subPath, key), Tiles: clientTiles(lang), Year: time.Now().Year(), MirrorHint: hint,
	}
	var buf bytes.Buffer
	if err := clientsTmpl.Execute(&buf, d); err != nil {
		http.Error(w, "page error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
