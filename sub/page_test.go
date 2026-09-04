package sub

import (
	"bytes"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

func TestWantsPage(t *testing.T) {
	cases := []struct {
		ua, query string
		want      bool
	}{
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1", "", true},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36", "", true},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0", "", true},
		{"Shadowrocket/2288 CFNetwork/1494 Darwin/23.4.0", "", false},
		{"clash-verge/v1.5.1", "", false},
		{"ClashMetaForAndroid/2.10.1.Meta", "", false},
		{"Hiddify/2.0.5 (Android)", "", false},
		{"v2rayN/6.45", "", false},
		{"SFI/1.9.0 (iOS 17)", "", false},
		{"curl/8.4.0", "", false},
		{"Mozilla/5.0 Chrome/120", "format=clash", false},
		{"curl/8.4.0", "format=page", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/sub/u?"+c.query, nil)
		r.Header.Set("User-Agent", c.ua)
		if got := WantsPage(r); got != c.want {
			t.Errorf("UA %q query %q: want %v got %v", c.ua, c.query, c.want, got)
		}
	}
}

func TestBuildPage(t *testing.T) {
	r := httptest.NewRequest("GET", "http://sub.example:2056/sub/alice", nil)
	r.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	r.Header.Set("X-Forwarded-Proto", "https")
	u := model.User{Name: "alice", Enabled: true, Volume: 100 << 30, Up: 10 << 30, Down: 20 << 30, Expiry: 4102444800}
	lines := []model.Line{{Name: "香港1", Protocol: "hysteria2"}, {Name: "美国-reality", Protocol: "vless", Tls: raw(map[string]interface{}{"mode": "reality"})}}
	d := buildPageData(r, "/sub/", u.Name, u, lines, Options{UpdateHours: 12}, "maoyang", "欢迎使用", "tg: @support")

	if d.SubClash != "https://sub.example:2056/sub/alice?format=clash" || d.SubLink != "https://sub.example:2056/sub/alice" {
		t.Fatalf("订阅地址不符: %s %s", d.SubClash, d.SubLink)
	}
	if d.Percent != 30 || d.UsedText != "30.0 GB" || d.TotalText != "100.0 GB" {
		t.Fatalf("用量不符: %d %s %s", d.Percent, d.UsedText, d.TotalText)
	}
	if !d.HasExpiry || d.Expired || d.StatusText != "active" || d.Lang != "zh" {
		t.Fatalf("状态不符: %+v", d)
	}
	if len(d.Imports) != 5 || !strings.HasPrefix(string(d.Imports[0].Href), "clash://install-config?url=https%3A%2F%2Fsub.example") || !strings.HasPrefix(string(d.Imports[2].Href), "sing-box://import-remote-profile?url=") || d.SubJSON != "https://sub.example:2056/sub/alice?format=json" {
		t.Fatalf("导入链接不符: %+v", d.Imports)
	}
	if len(d.Lines) != 2 || d.Lines[1].TLS != "reality" {
		t.Fatalf("节点列表不符: %+v", d.Lines)
	}

	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, d); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{"alice", "maoyang", "欢迎使用", "clash://install-config", "shadowrocket://add/sub://", "hiddify://import/", "/sub/alice/qr?format=clash", "美国-reality", "剩 "} {
		if !strings.Contains(html, want) {
			t.Errorf("页面缺少 %q", want)
		}
	}
	if strings.Contains(html, "ZgotmplZ") {
		t.Fatal("自定义 scheme 链接被 html/template 拦截,需用 template.URL")
	}
}

func TestPageStatusExhausted(t *testing.T) {
	r := httptest.NewRequest("GET", "http://x/sub/b", nil)
	u := model.User{Name: "b", Enabled: true, Volume: 1 << 30, Up: 1 << 30}
	d := buildPageData(r, "/sub/", u.Name, u, nil, Options{}, "t", "", "")
	if d.StatusText != "exhausted" || d.Percent != 100 || d.Lang != "en" {
		t.Fatalf("超量状态不符: %+v", d)
	}
}

// 一键导入的名字必须用"订阅标题",而不是订阅页标题:
// 两者常常不一样(页面叫 Maoyang Node,订阅标题叫 冒央会社),客户端里应显示后者。
func TestImportLinksUseProfileTitle(t *testing.T) {
	r := httptest.NewRequest("GET", "http://sub.example:2056/sub/alice", nil)
	u := model.User{Name: "alice", Enabled: true}
	opt := Options{UpdateHours: 12, ProfileTitle: "冒央会社"}
	d := buildPageData(r, "/sub/", u.Name, u, nil, opt, "Maoyang Node", "", "")

	if d.Title != "Maoyang Node" {
		t.Fatalf("页面标题应保持不变: %q", d.Title)
	}
	want := url.QueryEscape("冒央会社")
	for _, im := range d.Imports {
		href := string(im.Href)
		if im.Name == "Hiddify" { // 它的协议不带名字
			continue
		}
		if !strings.Contains(href, want) {
			t.Fatalf("%s 的导入链接应带订阅标题: %s", im.Name, href)
		}
		if strings.Contains(href, url.QueryEscape("Maoyang Node")) {
			t.Fatalf("%s 的导入链接不该用订阅页标题: %s", im.Name, href)
		}
	}
}
