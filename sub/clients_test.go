package sub

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 五块系统各自要有图标、系统名、说明和至少一个下载入口,而且链接必须是 https。
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
			if tile.OS == "" || tile.App == "" || tile.Desc == "" || !strings.Contains(string(tile.Icon), "<svg") {
				t.Fatalf("%s/%s 内容不全: %+v", lang, tile.Key, tile)
			}
			if len(tile.Links) == 0 {
				t.Fatalf("%s/%s 没有下载入口", lang, tile.Key)
			}
			primary := 0
			for _, l := range tile.Links {
				if l.Text == "" || !strings.HasPrefix(string(l.Href), "https://") {
					t.Fatalf("%s/%s 链接不对: %+v", lang, tile.Key, l)
				}
				if l.Primary {
					primary++
				}
			}
			if primary != 1 {
				t.Fatalf("%s/%s 推荐项应恰好一个,实际 %d", lang, tile.Key, primary)
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
