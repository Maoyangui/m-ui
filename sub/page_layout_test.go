package sub

import (
	"net/http"
	"strings"
	"testing"
)

// 落地页和客户端下载页重做了版式(0.8.0):这里把两页上原有的功能逐项钉住,以后再改样式不能把哪一项改丢。

func TestLandingKeepsEveryFeature(t *testing.T) {
	s, db := shareServer(t)
	body := doReqZh(s, "/sub/alice").Body.String()
	base := "http://hk.example:2056/sub/alice"
	for _, want := range []string{
		// 三条订阅地址,各有一个复制按钮
		`<code id="l1">` + base + `?format=clash</code>`, `onclick="cp('l1')"`,
		`<code id="l2">` + base + `</code>`, `onclick="cp('l2')"`,
		`<code id="l3">` + base + `?format=json</code>`, `onclick="cp('l3')"`,
		// 三个订阅二维码常驻在页面上
		`<img src="` + base + `/qr?format=clash"`, `<img src="` + base + `/qr?format=link"`, `<img src="` + base + `/qr?format=json"`,
		// 六个一键导入
		"godusevpn://import?url=", "clash://install-config?url=", "shadowrocket://add/sub://", "nextin://install-config?url=", "sing-box://import-remote-profile?url=", "hiddify://import/",
		// 客户端下载入口
		`class="getapp" href="` + base + `?clients=1"`,
		// 我的订阅:状态、用量、到期、自动更新
		"我的订阅", `<span class="badge ok">正常</span>`, `class="usage"`, `class="bar `, "到期", "自动更新",
		// 用量图(24 小时 / 7 天 / 30 天)
		`<details id="usage">`, `id="u-seg"`, `data-h="24"`, `data-h="168"`, `data-h="720"`, `id="u-chart"`, "function loadUsage(", "?stats=",
		// 临时共享
		`id="share"`, `action="?share=on"`, "生成链接",
		// 节点
		`<details class="nodes">`, "function openLine(",
		// 复制提示、二维码小浮层
		`id="toast"`, `id="qrp"`, "function showQR(", "function cpText(",
		// 明暗两套
		"prefers-color-scheme:dark",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("落地页缺少 %q", want)
		}
	}
	// 六个同一个样式(佛跳墙那一格不单独带底色)
	if n := strings.Count(body, `<a class="app" href="`); n != 6 {
		t.Fatalf("一键导入应有 6 个、同一个样式,实际 %d", n)
	}
	if !strings.Contains(body, "Windows · macOS · Android · Linux") || strings.Contains(body, "电视") {
		t.Fatal("佛跳墙那一格的小字应当是 Windows · macOS · Android · Linux")
	}
	if n := strings.Count(body, `<figure><img src="`); n != 3 {
		t.Fatalf("常驻的订阅二维码应有 3 个,实际 %d", n)
	}
	// 压暗全屏的旧小窗不该再有
	for _, bad := range []string{`class="qrpop"`, `class="qrbox"`} {
		if strings.Contains(body, bad) {
			t.Fatalf("落地页里不该再有 %q", bad)
		}
	}
	// 开了临时共享:共享地址一行(复制 + 二维码图标)、取消按钮;共享地址自己的二维码取得到
	if w := doReq(s, "POST", "/sub/alice?share=on", browserUA); w.Code != http.StatusOK {
		t.Fatalf("生成共享地址应当 200,实际 %d", w.Code)
	}
	tok := token(t, db)
	body = doReqZh(s, "/sub/alice").Body.String()
	shareURL := "http://hk.example:2056/sub/" + tok
	for _, want := range []string{`<code id="l4">` + shareURL + `</code>`, `onclick="cp('l4')"`, `data-qs="` + shareURL + `/qr"`, `action="?share=off"`, "取消共享", "已开启"} {
		if !strings.Contains(body, want) {
			t.Fatalf("开了共享的落地页缺少 %q", want)
		}
	}
	if w := doReq(s, "GET", "/sub/"+tok+"/qr", browserUA); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("共享地址的二维码应当出得来,实际 %d", w.Code)
	}
	// 英文界面:状态写成词,不是内部的状态名
	r := doReq(s, "GET", "/sub/alice", browserUA)
	if en := r.Body.String(); !strings.Contains(en, `<span class="badge ok">Active</span>`) || !strings.Contains(en, "Subscription links") {
		t.Fatal("英文落地页的状态应当是 Active")
	}
}

func TestClientsPageKeepsEveryFeature(t *testing.T) {
	s, _ := shareServer(t)
	w := doReqZh(s, "/sub/alice?clients=1")
	if w.Code != 200 {
		t.Fatalf("客户端下载页应当 200,实际 %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"客户端下载", `href="http://hk.example:2056/sub/alice"`, "返回订阅页",
		// 五个系统:上面一排是短名,每块里写全名
		`<span>iOS</span>`, `<span>Android</span>`, `<span>Windows</span>`, `<span>macOS</span>`, `<span>Linux</span>`,
		"iOS / iPadOS / Apple TV", "Android / Android TV",
		`id="p-ios"`, `id="p-android"`, `id="p-windows"`, `id="p-macos"`, `id="p-linux"`,
		// 推荐标、该粘哪个地址、首选包、国内镜像、全部版本
		"推荐", "通用地址", "Clash 地址", `class="dl primary"`, "大陆可用", ghMirrorHost, "全部版本",
		// 没有 JS 时五块全部展开;有 JS 按系统选一块
		`.panel[hidden]{display:block}`, `.js .panel[hidden]{display:none}`, "navigator.userAgent",
		"下载均来自各客户端官方发布页", "prefers-color-scheme:dark",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("客户端下载页缺少 %q", want)
		}
	}
	if n := strings.Count(body, `class="tile"`); n != 5 {
		t.Fatalf("系统应有 5 个,实际 %d", n)
	}
	if n := strings.Count(body, `class="panel"`); n != 5 {
		t.Fatalf("五个系统各一块,实际 %d", n)
	}
	// 每款客户端恰好一个首选包
	apps := strings.Count(body, `<section class="app`)
	if primary := strings.Count(body, `class="dl primary"`); apps < 10 || primary != apps {
		t.Fatalf("每款客户端应恰好一个首选包:%d 款 / %d 个首选包", apps, primary)
	}
	// 这一页可以随手转发:不带用户的任何信息(除了回订阅页的那个地址)
	if strings.Count(body, "alice") != 1+strings.Count(body, "alice/logo") {
		t.Fatalf("客户端下载页里用户名只该出现在「返回订阅页」的地址里")
	}
}
