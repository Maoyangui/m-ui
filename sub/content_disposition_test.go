package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 订阅下载文件名必须按格式带后缀:不带时浏览器存下来的文件没有扩展名,
// 有的下载器还会自己编一个像 3.bin 这样的名字。
func TestContentDispositionExtension(t *testing.T) {
	cases := []struct {
		name     string
		title    string
		fallback string
		ext      string
		want     string
	}{
		{"ASCII clash", "ces", "ces", ".yaml", `attachment;filename="ces.yaml"`},
		{"ASCII sing-box", "ces", "ces", ".json", `attachment;filename="ces.json"`},
		{"ASCII link", "ces", "ces", ".txt", `attachment;filename="ces.txt"`},
		{"空标题回落", "", "bob", ".yaml", `attachment;filename="bob.yaml"`},
		{"全空回落 subscription", "", "", ".json", `attachment;filename="subscription.json"`},
		{"已带后缀不重复", "ces.yaml", "ces", ".yaml", `attachment;filename="ces.yaml"`},
		{"无后缀参数", "ces", "ces", "", `attachment;filename="ces"`},
	}
	for _, c := range cases {
		if got := contentDisposition(c.title, c.fallback, c.ext); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// 中文标题:只发 filename*,后缀追加在转义后的名字后面。
	got := contentDisposition("冒央会社", "alice", ".yaml")
	if !strings.HasPrefix(got, "attachment;filename*=UTF-8''") {
		t.Fatalf("中文标题格式不对: %q", got)
	}
	if !strings.HasSuffix(got, ".yaml") {
		t.Fatalf("中文标题也应带后缀: %q", got)
	}
	if strings.Contains(got, "filename=") {
		t.Fatalf("中文标题不应带 ASCII filename=: %q", got)
	}
}

// 浏览器下载时三种订阅的文件名都带对应后缀;客户端拉订阅时不带(Clash 系客户端拿这个文件名当配置名,带了就显示成「ces.yaml」)。
func TestSubscriptionHeadersHaveExtension(t *testing.T) {
	u := model.User{Name: "ces"}
	dl := Options{Download: true}
	if got := headers(u, dl, "text/plain; charset=utf-8", ".txt")["Content-Disposition"]; got != `attachment;filename="ces.txt"` {
		t.Errorf("link: %q", got)
	}
	if got := headers(u, dl, "text/yaml; charset=utf-8", ".yaml")["Content-Disposition"]; got != `attachment;filename="ces.yaml"` {
		t.Errorf("clash: %q", got)
	}
	if got := headers(u, dl, "application/json; charset=utf-8", ".json")["Content-Disposition"]; got != `attachment;filename="ces.json"` {
		t.Errorf("sing-box: %q", got)
	}
	for _, ext := range []string{".txt", ".yaml", ".json"} {
		if got := headers(u, Options{}, "text/plain", ext)["Content-Disposition"]; got != `attachment;filename="ces"` {
			t.Errorf("客户端拉订阅不该带后缀 %s: %q", ext, got)
		}
	}
}

// 谁算浏览器:常见浏览器和 App 内置浏览器算;代理客户端(UA 里也常带 Mozilla)、命令行工具不算。
func TestIsBrowserUA(t *testing.T) {
	for ua, want := range map[string]bool{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36":                      true,
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 MicroMessenger/8.0": true,
		"Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0":                                                           true,
		"clash-verge/v2.0.3":            false,
		"mihomo/1.19.0":                 false,
		"Shadowrocket/2070 CFNetwork":   false,
		"SFA (sing-box 1.14.0; sdk 34)": false,
		"Mozilla/5.0 (Linux; Android 14) ClashMetaForAndroid/2.11 AppleWebKit Chrome": false,
		"curl/8.5.0": false,
		"":           false,
	} {
		if got := isBrowserUA(ua); got != want {
			t.Errorf("isBrowserUA(%q) = %v, want %v", ua, got, want)
		}
	}
}
