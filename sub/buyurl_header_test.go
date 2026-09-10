package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 客户端拿订阅时,「选购 / 续费」地址随 Profile-Web-Page-Url 头一起发。
// 四种地址都要给对同一个人的那一条:主面板用户、代理的用户,以及这两种人各自的临时共享地址
// —— 共享地址是另一个令牌,但底下还是同一个用户,链接得跟着他所属的代理走。
func TestBuyURLHeader(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Setting{Key: "subPageBuyURL", Value: "https://main.example/buy"})
	db.Create(&model.Reseller{Name: "dl1", Enabled: true, PageEnabled: true, ShareOn: true, PageBuyURL: "https://dl1.example/buy"})
	db.Create(&model.User{Name: "bob", Enabled: true, ResellerId: 1, SubToken: "tok123",
		Credentials: []byte(`{"hysteria2":{"password":"p"}}`)})
	db.Exec("INSERT INTO user_lines (user_id, line_id) VALUES (2, 1)")

	// 各自生成一条临时共享地址
	doReq(s, "POST", "/sub/alice?share=on", browserUA)
	var alice, bob model.User
	db.First(&alice, 1)
	db.First(&bob, 2)
	doReq(s, "POST", "/sub/tok123?share=on", browserUA)
	db.First(&bob, 2)

	for _, c := range []struct{ name, path, want string }{
		{"主面板用户", "/sub/alice", "https://main.example/buy"},
		{"代理的用户", "/sub/tok123", "https://dl1.example/buy"},
		{"主面板用户的共享地址", "/sub/" + alice.ShareToken, "https://main.example/buy"},
		{"代理用户的共享地址", "/sub/" + bob.ShareToken, "https://dl1.example/buy"},
	} {
		if c.path == "/sub/" && strings.HasSuffix(c.path, "/sub/") {
			t.Fatalf("%s:共享令牌没生成出来", c.name)
		}
		for _, f := range []string{"", "?format=json", "?format=clash"} {
			w := doReq(s, "GET", c.path+f, "curl/8.4.0")
			if got := w.Header().Get("Profile-Web-Page-Url"); got != c.want {
				t.Fatalf("%s%s:续费地址应是 %q,得 %q", c.name, f, c.want, got)
			}
		}
	}

	// 代理清空自己的,回落主面板的
	db.Model(&model.Reseller{}).Where("id = ?", 1).Update("page_buy_url", "")
	if got := doReq(s, "GET", "/sub/tok123?format=json", "curl/8.4.0").Header().Get("Profile-Web-Page-Url"); got != "https://main.example/buy" {
		t.Fatalf("代理没填就该回落主面板的,得 %q", got)
	}
	// 没配 / 非 http(s) / 带控制字符 → 整条头不发,客户端那边就不画按钮
	for _, bad := range []string{"", "javascript:alert(1)", "https://x.example/buy\r\nX-Injected: 1"} {
		db.Model(&model.Setting{}).Where("key = ?", "subPageBuyURL").Update("value", bad)
		w := doReq(s, "GET", "/sub/alice?format=json", "curl/8.4.0")
		if got := w.Header().Get("Profile-Web-Page-Url"); got != "" {
			t.Fatalf("%q 不该发出去,得 %q", bad, got)
		}
		if got := w.Header().Get("X-Injected"); got != "" {
			t.Fatal("响应头被换行劈开了")
		}
	}
}
