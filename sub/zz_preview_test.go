package sub

import (
	"bytes"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 把两张页面渲染成文件,用浏览器看效果:M_UI_PREVIEW=<目录> go test -run Preview ./sub/
func TestPreviewPages(t *testing.T) {
	dir := os.Getenv("M_UI_PREVIEW")
	if dir == "" {
		t.Skip("设 M_UI_PREVIEW=<输出目录> 才生成预览")
	}
	r := httptest.NewRequest("GET", "http://sub.example:2056/sub/one", nil)
	r.Header.Set("Accept-Language", "zh-CN")
	r.Header.Set("X-Forwarded-Proto", "https")
	u := model.User{Name: "one", Enabled: true, Volume: 100 << 30, Up: 12 << 30, Down: 30 << 30, Expiry: 4102444800}
	lines := []model.Line{{Name: "香港1-高带宽", Protocol: "hysteria2"}, {Name: "台湾2", Protocol: "anytls"}}
	d := buildPageData(r, "/sub/", "one", u, lines, Options{UpdateHours: 12, ProfileTitle: "冒央会社"}, "冒央会社", "欢迎使用", "tg: @support")
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/landing.html", buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	cd := clientsData{Lang: "zh", Title: "冒央会社", Back: "https://sub.example:2056/sub/one", Tiles: clientTiles("zh"), Year: 2026,
		MirrorHint: "经 ghfast.top 中转"}
	if err := clientsTmpl.Execute(&buf, cd); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/clients.html", buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("已生成 %s/landing.html 与 clients.html", dir)
}
