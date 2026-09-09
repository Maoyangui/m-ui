package sub

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 五块系统各自要有图标、系统名,每块至少两款客户端(第一款是推荐项,且只有一款推荐),
// 每款都要有说明、该粘贴哪种地址、至少一个 https 下载入口且首选包恰好一个。
func TestClientTiles(t *testing.T) {
	for _, lang := range []string{"zh", "en"} {
		tiles := clientTiles(lang)
		if len(tiles) != 5 {
			t.Fatalf("%s: 应有 5 块,实际 %d", lang, len(tiles))
		}
		want := map[string]bool{"ios": true, "android": true, "windows": true, "macos": true, "linux": true}
		for _, tile := range tiles {
			if !want[tile.Key] {
				t.Fatalf("多出的系统: %s", tile.Key)
			}
			delete(want, tile.Key)
			if tile.OS == "" || !strings.Contains(string(tile.Icon), "<svg") {
				t.Fatalf("%s/%s 内容不全: %+v", lang, tile.Key, tile)
			}
			if len(tile.Apps) < 2 {
				t.Fatalf("%s/%s 应列出不止一款客户端,实际 %d", lang, tile.Key, len(tile.Apps))
			}
			rec := 0
			for i, app := range tile.Apps {
				if app.Recommended {
					rec++
					if i != 0 {
						t.Fatalf("%s/%s 推荐项应排第一,实际是第 %d 款 %s", lang, tile.Key, i+1, app.Name)
					}
				}
				if app.Name == "" || app.Desc == "" || app.Format == "" {
					t.Fatalf("%s/%s/%s 内容不全: %+v", lang, tile.Key, app.Name, app)
				}
				if len(app.Links) == 0 {
					t.Fatalf("%s/%s/%s 没有下载入口", lang, tile.Key, app.Name)
				}
				primary := 0
				for _, l := range app.Links {
					if l.Text == "" || !strings.HasPrefix(string(l.Href), "https://") {
						t.Fatalf("%s/%s/%s 链接不对: %+v", lang, tile.Key, app.Name, l)
					}
					if l.Primary {
						primary++
					}
				}
				if primary != 1 {
					t.Fatalf("%s/%s/%s 首选包应恰好一个,实际 %d", lang, tile.Key, app.Name, primary)
				}
				// GitHub 上的客户端都要有且只有一个国内镜像(同一文件经镜像站),App Store 的没有
				mirrors, ghPrimary := 0, false
				for _, l := range app.Links {
					if l.Primary && strings.HasPrefix(string(l.Href), "https://github.com/") {
						ghPrimary = true
					}
					if l.Mirror {
						mirrors++
						if !strings.HasPrefix(string(l.Href), ghMirror+"https://github.com/") || !l.Muted {
							t.Fatalf("%s/%s/%s 镜像链接不对: %+v", lang, tile.Key, app.Name, l)
						}
					}
				}
				if ghPrimary && mirrors != 1 {
					t.Fatalf("%s/%s/%s 应恰好一个国内镜像,实际 %d", lang, tile.Key, app.Name, mirrors)
				}
				if !ghPrimary && mirrors != 0 {
					t.Fatalf("%s/%s/%s 不是 GitHub 发布的,不该有镜像", lang, tile.Key, app.Name)
				}
			}
			if rec != 1 {
				t.Fatalf("%s/%s 推荐项应恰好一款,实际 %d", lang, tile.Key, rec)
			}
			anyMirror := false
			for _, app := range tile.Apps {
				for _, l := range app.Links {
					if l.Mirror {
						anyMirror = true
					}
				}
			}
			if anyMirror != (tile.MirrorNote != "") {
				t.Fatalf("%s/%s 镜像说明与镜像链接不一致(有链接=%v,有说明=%v)", lang, tile.Key, anyMirror, tile.MirrorNote != "")
			}
		}
	}
}

// 模板要能渲染出来,而且这一页不带任何用户信息(它可以被随手转发)。
func TestClientsPageRender(t *testing.T) {
	var buf bytes.Buffer
	d := clientsData{Lang: "zh", Title: "maoyang", Back: "https://sub.example/sub/alice", Tiles: clientTiles("zh"), Year: 2026}
	if err := clientsTmpl.Execute(&buf, d); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{"客户端下载", "Nextin", "Clash Verge Rev", "Clash Meta for Android", "ghfast.top", "id6754002454"} {
		if !strings.Contains(html, want) {
			t.Fatalf("页面里缺少 %q", want)
		}
	}
	if strings.Count(html, `class="tile"`) != 5 {
		t.Fatalf("图标块数量不对")
	}
	// 下载按钮一律新标签打开,且带 noopener
	if strings.Count(html, `rel="noopener noreferrer"`) < 10 {
		t.Fatalf("下载链接没有全部带 rel=noopener")
	}
}

// 订阅页上的"客户端下载"入口指向同一地址加 ?clients=1(代理用户同理,走的是同一段代码)。
func TestLandingLinksToClients(t *testing.T) {
	r := httptest.NewRequest("GET", "http://sub.example:2056/sub/alice", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	d := buildPageData(r, "/sub/", "alice", model.User{Name: "alice", Enabled: true}, nil, Options{}, "t", "", "")
	if string(d.ClientsURL) != "https://sub.example:2056/sub/alice?clients=1" {
		t.Fatalf("入口地址不对: %s", d.ClientsURL)
	}
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `class="getapp"`) || !strings.Contains(buf.String(), "?clients=1") {
		t.Fatal("订阅页里没有客户端下载入口")
	}
}

// 自家客户端:安卓 / Windows / Linux 三块里佛跳墙必须排第一并带推荐标,链接指向最新版本的发布资产,
// 并且能自动拿到国内镜像(和别家一样经 addMirrors 补上)。
func TestGodusevpnFirstAndMirrored(t *testing.T) {
	for _, lang := range []string{"zh", "en"} {
		name := "佛跳墙"
		if lang != "zh" {
			name = "Fotiaoqiang"
		}
		ver := godusevpnVersion()
		for _, key := range []string{"android", "windows", "macos", "linux"} {
			var tile clientTile
			for _, x := range clientTiles(lang) {
				if x.Key == key {
					tile = x
				}
			}
			if len(tile.Apps) == 0 {
				t.Fatalf("%s: 找不到 %s 块", lang, key)
			}
			app := tile.Apps[0]
			if app.Name != name || !app.Recommended {
				t.Fatalf("%s/%s: 第一款应是带推荐标的 %s,实际 %q recommended=%v", lang, key, name, app.Name, app.Recommended)
			}
			var primary, mirror string
			for _, l := range app.Links {
				if l.Primary {
					primary = string(l.Href)
				}
				if l.Mirror {
					mirror = string(l.Href)
				}
			}
			if !strings.Contains(primary, "Maoyangui/godusevpn/releases/download/v"+ver+"/godusevpn-"+ver+"-") {
				t.Fatalf("%s/%s: 首选包应指向本仓库最新版本的资产: %s", lang, key, primary)
			}
			if !strings.HasPrefix(mirror, ghMirror) {
				t.Fatalf("%s/%s: 应有国内镜像入口: %q", lang, key, mirror)
			}
		}
	}
}

// 版本号取自后台刷新的最新发布;拿不到网络时用兜底常量,格式必须是 x.y.z[-后缀]。
func TestGodusevpnVersionFallbackAndParse(t *testing.T) {
	if got := godusevpnVersion(); !godTagRe.MatchString(got) {
		t.Fatalf("兜底版本格式不对: %q", got)
	}
	before := godusevpnVersion()
	setGodusevpnVersion("不是版本号")
	if godusevpnVersion() != before {
		t.Fatal("非法版本号不该被采用")
	}
	setGodusevpnVersion("v9.9.9-z1")
	if godusevpnVersion() != "9.9.9-z1" {
		t.Fatalf("应去掉 v 前缀并采用: %q", godusevpnVersion())
	}
	setGodusevpnVersion(before)
}

// 真的去 GitHub 读一次发布订阅源(默认跳过,平时不联网跑测试):M_UI_NET_TEST=1 go test -run Atom ./sub/
func TestGodusevpnAtomLive(t *testing.T) {
	if os.Getenv("M_UI_NET_TEST") != "1" {
		t.Skip("联网用例,设 M_UI_NET_TEST=1 再跑")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, err := fetchGodusevpnVersion(ctx, "https://github.com")
	if err != nil {
		t.Fatal(err)
	}
	if !godTagRe.MatchString(v) {
		t.Fatalf("拿到的版本号不对: %q", v)
	}
	t.Logf("最新发布: %s", v)
}
