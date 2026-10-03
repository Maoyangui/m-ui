package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 共享地址拉到的那一份里,不能有任何看得出本人是谁的东西:m-ui 默认拿用户名当订阅地址,借用者知道用户名就能拿到
// 本人的永久订阅和凭据(审计 MB23)。标题、下载文件名、用量头、提示节点、一键导入名都不能回落到本人用户名;
// socks / http / mixed 的链接里写着用户名,这几条线路不给。本人地址照旧。
func TestSharedSubscriptionHidesOwner(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Line{Name: "s5", Protocol: "socks", Port: 1080, Enabled: true})
	db.Exec("INSERT INTO user_lines (user_id, line_id) VALUES (1, 2)")
	db.Model(&model.User{}).Where("id = 1").Updates(map[string]interface{}{"up": 5 << 30, "volume": 100 << 30, "remark": "alice 的号",
		"credentials": []byte(`{"hysteria2":{"password":"p"},"socks":{"username":"alice","password":"sp"}}`)})
	db.Create(&model.Setting{Key: "subShowNotice", Value: "true"})
	if w := doReq(s, "POST", "/sub/alice?share=on", browserUA); w.Code != 200 {
		t.Fatalf("生成共享失败: %d", w.Code)
	}
	tok := token(t, db)

	for _, f := range []string{"", "?format=clash", "?format=json"} {
		w := doReq(s, "GET", "/sub/"+tok+f, "curl/8.4.0")
		if w.Code != 200 {
			t.Fatalf("格式 %q 共享订阅拉不到: %d", f, w.Code)
		}
		for _, h := range []string{"Profile-Title", "Content-Disposition"} {
			if v := w.Header().Get(h); strings.Contains(v, "alice") || strings.Contains(v, "YWxpY2U") {
				t.Fatalf("格式 %q 的 %s 带着本人用户名: %s", f, h, v)
			}
		}
		if ui := w.Header().Get("Subscription-Userinfo"); strings.Contains(ui, "upload") || strings.Contains(ui, "total") {
			t.Fatalf("格式 %q 共享订阅带着本人用量: %s", f, ui)
		}
		if body := w.Body.String(); strings.Contains(body, "alice") || strings.Contains(body, "socks") {
			t.Fatalf("格式 %q 共享订阅正文里有本人用户名或 socks 线路:\n%s", f, body)
		}
	}
	page := doReq(s, "GET", "/sub/"+tok, browserUA).Body.String()
	if strings.Contains(page, "alice") {
		t.Fatal("共享地址的落地页(含一键导入名)带着本人用户名")
	}
	mine := doReq(s, "GET", "/sub/alice", "curl/8.4.0")
	if !strings.Contains(mine.Header().Get("Subscription-Userinfo"), "upload=5368709120") || !strings.Contains(mine.Body.String(), "socks5://") {
		t.Fatalf("本人地址应照旧带用量和 socks 线路: %s", mine.Header().Get("Subscription-Userinfo"))
	}
}

// socks / http / mixed 的链接用户名要和数据面一致(当前用户名):凭据里存的是建号时的名字,改名后链接就认证不过(审计 MB26)。
func TestSocksLinkUsesCurrentName(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Line{Name: "s5", Protocol: "socks", Port: 1080, Enabled: true})
	db.Exec("INSERT INTO user_lines (user_id, line_id) VALUES (1, 2)")
	db.Model(&model.User{}).Where("id = 1").Updates(map[string]interface{}{"name": "alice2",
		"credentials": []byte(`{"hysteria2":{"password":"p"},"socks":{"username":"alice","password":"sp"}}`)})
	for _, f := range []string{"", "?format=clash"} {
		body := doReq(s, "GET", "/sub/alice2"+f, "curl/8.4.0").Body.String()
		if f == "" && !strings.Contains(body, "socks5://alice2:sp@") {
			t.Fatalf("改名后链接里的用户名应是新名字:\n%s", body)
		}
		if f != "" && !strings.Contains(body, "username: alice2") {
			t.Fatalf("改名后 clash 里的用户名应是新名字:\n%s", body)
		}
	}
}
