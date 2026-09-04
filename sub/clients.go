package sub

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"time"

	"github.com/Maoyangui/m-ui/brand"
)

// 客户端下载页:订阅页"一键导入"旁边那个下载箭头点开的就是这里。
//
// 页面本身不含任何用户数据,只是把各系统该装哪个客户端讲清楚 + 给可用的下载地址。
// 版本号写死是有意的:直链必须指向确实存在的文件,版本升级时改这一处即可,
// 每块同时留了"全部版本"入口,即便这里的版本旧了也点得到最新的。
//
//go:embed clients.html
var clientsFS embed.FS

var clientsTmpl = template.Must(template.ParseFS(clientsFS, "clients.html"))

const (
	verVerge   = "2.5.2"   // Clash Verge Rev(Windows / macOS / Linux)
	verCMFA    = "2.11.33" // Clash Meta for Android
	verFlClash = "0.8.96"  // FlClash(Linux AppImage,免安装)
	ghMirror   = "https://ghfast.top/"
)

// dl 是一个下载按钮。Primary 的那个是推荐项。
type dl struct {
	Text    string
	Href    template.URL
	Primary bool
	Muted   bool // 次要入口(镜像 / 全部版本),弱化显示
}

type clientTile struct {
	Key   string // ios / android / windows / macos / linux
	Icon  template.HTML
	OS    string // 图标下方那行系统名
	App   string // 用哪个客户端
	Desc  string // 展开后的说明,一两句话
	Links []dl
}

type clientsData struct {
	Lang  string
	Icon  template.URL
	Title string
	Back  string // 返回订阅页
	Tiles []clientTile
	Year  int
}

func gh(repo, tag, file string) template.URL {
	return template.URL("https://github.com/" + repo + "/releases/download/" + tag + "/" + file)
}

func ghm(repo, tag, file string) template.URL {
	return template.URL(ghMirror + "https://github.com/" + repo + "/releases/download/" + tag + "/" + file)
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
	return []clientTile{
		{
			Key: "ios", Icon: template.HTML(iconApple), OS: pick("iOS / iPadOS / Apple TV", "iOS / iPadOS / Apple TV"), App: "Nextin",
			Desc: pick("Nextin 在中国大陆区 App Store 搜不到:把 App Store 切到非大陆区(如美区),或用非大陆 Apple ID 登录,再搜索安装。装好后回订阅页点「Nextin」或粘贴订阅地址。",
				"Nextin is not listed in the mainland China App Store. Switch the App Store to another region (e.g. the US) or sign in with a non-mainland Apple ID, then install it and import your subscription link."),
			Links: []dl{
				{Text: pick("App Store(美区)", "App Store (US)"), Href: "https://apps.apple.com/us/app/nextin/id6754002454", Primary: true},
			},
		},
		{
			Key: "android", Icon: template.HTML(iconAndro), OS: pick("Android / Android TV", "Android / Android TV"), App: "Clash Meta for Android",
			Desc: pick("下载 APK 安装(系统会提示「允许安装未知来源」,同意即可)。通用版任何机型都能装,机型确定是 64 位手机/电视盒子可以选更小的 arm64 版。",
				"Install the APK (Android will ask you to allow installs from this source). The universal build works on any device; pick arm64 if you know your phone or TV box is 64-bit."),
			Links: []dl{
				{Text: pick("APK 通用版 v"+verCMFA, "APK universal v"+verCMFA), Href: gh("MetaCubeX/ClashMetaForAndroid", "v"+verCMFA, "cmfa-"+verCMFA+"-meta-universal-release.apk"), Primary: true},
				{Text: pick("APK arm64 版", "APK arm64"), Href: gh("MetaCubeX/ClashMetaForAndroid", "v"+verCMFA, "cmfa-"+verCMFA+"-meta-arm64-v8a-release.apk")},
				{Text: pick("国内镜像下载", "China mirror"), Href: ghm("MetaCubeX/ClashMetaForAndroid", "v"+verCMFA, "cmfa-"+verCMFA+"-meta-universal-release.apk"), Muted: true},
				{Text: pick("全部版本", "All releases"), Href: ghLatest("MetaCubeX/ClashMetaForAndroid"), Muted: true},
			},
		},
		{
			Key: "windows", Icon: template.HTML(iconWin), OS: "Windows", App: "Clash Verge Rev",
			Desc: pick("下载安装包一路下一步即可。若提示「Windows 已保护你的电脑」,点「更多信息」→「仍要运行」。装好后在软件里粘贴订阅地址。",
				"Run the installer and follow the wizard. If Windows SmartScreen warns you, choose More info → Run anyway, then paste your subscription link into the app."),
			Links: []dl{
				{Text: pick("Windows x64 安装包 v"+verVerge, "Windows x64 installer v"+verVerge), Href: gh("clash-verge-rev/clash-verge-rev", "v"+verVerge, "Clash.Verge_"+verVerge+"_x64-setup.exe"), Primary: true},
				{Text: pick("ARM64 安装包", "ARM64 installer"), Href: gh("clash-verge-rev/clash-verge-rev", "v"+verVerge, "Clash.Verge_"+verVerge+"_arm64-setup.exe")},
				{Text: pick("国内镜像下载", "China mirror"), Href: ghm("clash-verge-rev/clash-verge-rev", "v"+verVerge, "Clash.Verge_"+verVerge+"_x64-setup.exe"), Muted: true},
				{Text: pick("全部版本", "All releases"), Href: ghLatest("clash-verge-rev/clash-verge-rev"), Muted: true},
			},
		},
		{
			Key: "macos", Icon: template.HTML(iconMac), OS: "macOS", App: "Clash Verge Rev",
			Desc: pick("按芯片选:2020 年后的机型基本都是 Apple 芯片。打开 dmg 把图标拖进「应用程序」即可;首次打开若提示来源不明,到「系统设置 → 隐私与安全性」点「仍要打开」。",
				"Pick the build for your chip (Macs from 2020 on are Apple silicon). Open the dmg and drag the app into Applications; on first launch allow it under System Settings → Privacy & Security."),
			Links: []dl{
				{Text: pick("Apple 芯片 (dmg) v"+verVerge, "Apple silicon (dmg) v"+verVerge), Href: gh("clash-verge-rev/clash-verge-rev", "v"+verVerge, "Clash.Verge_"+verVerge+"_aarch64.dmg"), Primary: true},
				{Text: pick("Intel 芯片 (dmg)", "Intel (dmg)"), Href: gh("clash-verge-rev/clash-verge-rev", "v"+verVerge, "Clash.Verge_"+verVerge+"_x64.dmg")},
				{Text: pick("国内镜像下载", "China mirror"), Href: ghm("clash-verge-rev/clash-verge-rev", "v"+verVerge, "Clash.Verge_"+verVerge+"_aarch64.dmg"), Muted: true},
				{Text: pick("全部版本", "All releases"), Href: ghLatest("clash-verge-rev/clash-verge-rev"), Muted: true},
			},
		},
		{
			Key: "linux", Icon: template.HTML(iconLinux), OS: "Linux", App: "Clash Verge Rev · FlClash",
			Desc: pick("Debian / Ubuntu 装 deb,Fedora / RHEL 装 rpm。不想装包就用 FlClash 的 AppImage:下载后加执行权限直接双击运行。",
				"Use the deb on Debian/Ubuntu or the rpm on Fedora/RHEL. Prefer no install? Grab the FlClash AppImage, make it executable and run it."),
			Links: []dl{
				{Text: "deb (amd64) v" + verVerge, Href: gh("clash-verge-rev/clash-verge-rev", "v"+verVerge, "Clash.Verge_"+verVerge+"_amd64.deb"), Primary: true},
				{Text: "rpm (x86_64) v" + verVerge, Href: gh("clash-verge-rev/clash-verge-rev", "v"+verVerge, "Clash.Verge-"+verVerge+"-1.x86_64.rpm")},
				{Text: "AppImage v" + verFlClash, Href: gh("chen08209/FlClash", "v"+verFlClash, "FlClash-"+verFlClash+"-linux-amd64.AppImage")},
				{Text: pick("国内镜像下载", "China mirror"), Href: ghm("clash-verge-rev/clash-verge-rev", "v"+verVerge, "Clash.Verge_"+verVerge+"_amd64.deb"), Muted: true},
				{Text: pick("全部版本", "All releases"), Href: ghLatest("clash-verge-rev/clash-verge-rev"), Muted: true},
			},
		},
	}
}

// serveClients 输出客户端下载页。页面不带任何用户信息,Back 指回订阅页。
func (s *Server) serveClients(w http.ResponseWriter, r *http.Request, subPath, key, title string) {
	lang := pageLang(r)
	d := clientsData{
		Lang: lang, Icon: template.URL(brand.DataURI), Title: title,
		Back: publicBase(r, subPath, key), Tiles: clientTiles(lang), Year: time.Now().Year(),
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
