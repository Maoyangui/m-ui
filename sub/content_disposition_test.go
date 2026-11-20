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

// 三种订阅的响应头里文件名都带对应后缀。
func TestSubscriptionHeadersHaveExtension(t *testing.T) {
	u := model.User{Name: "ces"}
	if got := headers(u, Options{}, "text/plain; charset=utf-8", ".txt")["Content-Disposition"]; got != `attachment;filename="ces.txt"` {
		t.Errorf("link: %q", got)
	}
	if got := headers(u, Options{}, "text/yaml; charset=utf-8", ".yaml")["Content-Disposition"]; got != `attachment;filename="ces.yaml"` {
		t.Errorf("clash: %q", got)
	}
	if got := headers(u, Options{}, "application/json; charset=utf-8", ".json")["Content-Disposition"]; got != `attachment;filename="ces.json"` {
		t.Errorf("sing-box: %q", got)
	}
}
