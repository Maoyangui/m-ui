package sub

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"

func shareServer(t *testing.T) (*Server, *gorm.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	db.Create(&model.Node{Name: "HK", Domain: "hk.example", IsLocal: true, Enabled: true})
	db.Create(&model.Line{Name: "香港1", Protocol: "hysteria2", Port: 443, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true,
		Credentials: []byte(`{"hysteria2":{"password":"p"}}`)})
	db.Exec("INSERT INTO user_lines (user_id, line_id) VALUES (1, 1)")
	return &Server{db: db}, db
}

func doReq(s *Server, method, target, ua string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://hk.example:2056"+target, nil)
	r.Header.Set("User-Agent", ua)
	w := httptest.NewRecorder()
	s.handle()(w, r)
	return w
}

func token(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var u model.User
	db.First(&u, 1)
	return u.ShareToken
}

// 生成 → 共享地址发同一份订阅但不出页面 → 取消后失效。
func TestShareLink(t *testing.T) {
	s, db := shareServer(t)

	// 订阅页有"生成"按钮
	if body := doReq(s, "GET", "/sub/alice", browserUA).Body.String(); !strings.Contains(body, `action="?share=on"`) {
		t.Fatal("订阅页应有生成按钮")
	}
	// 生成
	if w := doReq(s, "POST", "/sub/alice?share=on", browserUA); w.Code != http.StatusSeeOther {
		t.Fatalf("生成应 303,得 %d", w.Code)
	}
	tok := token(t, db)
	if len(tok) < 20 {
		t.Fatalf("令牌不合理: %q", tok)
	}
	var owner model.User
	db.First(&owner, 1)
	if len(owner.ShareCreds) == 0 || strings.Contains(string(owner.ShareCreds), "\"password\":\"p\"") {
		t.Fatalf("共享应有独立凭据: %s", owner.ShareCreds)
	}

	// 三种格式都能拉到同样的节点,但用的是另一套凭据(本人口令不得出现在共享订阅里)
	for _, f := range []string{"", "?format=clash", "?format=json"} {
		mine := doReq(s, "GET", "/sub/alice"+f, "curl/8.4.0")
		shared := doReq(s, "GET", "/sub/"+tok+f, "curl/8.4.0")
		if shared.Code != 200 || !strings.Contains(shared.Body.String(), "hk.example") {
			t.Fatalf("格式 %q 共享订阅不可用 (code %d)", f, shared.Code)
		}
		if shared.Body.String() == mine.Body.String() || strings.Contains(shared.Body.String(), "//p@") {
			t.Fatalf("格式 %q 共享订阅不该用本人凭据", f)
		}
	}
	// 共享地址:不出订阅页、不出二维码、不能改共享状态
	if w := doReq(s, "GET", "/sub/"+tok, browserUA); strings.Contains(w.Body.String(), "<html") {
		t.Fatal("共享地址不应出订阅页")
	}
	if w := doReq(s, "GET", "/sub/"+tok+"/qr", browserUA); w.Code != 404 {
		t.Fatalf("共享地址不应出二维码,得 %d", w.Code)
	}
	if w := doReq(s, "POST", "/sub/"+tok+"?share=off", browserUA); w.Code != 404 {
		t.Fatalf("共享地址不应能取消,得 %d", w.Code)
	}
	if token(t, db) != tok {
		t.Fatal("令牌被共享地址改动")
	}
	// 访问记在本人名下并标出是共享
	var logs []model.SubLog
	db.Where("format LIKE ?", "%-share").Find(&logs)
	if len(logs) == 0 || logs[0].User != "alice" {
		t.Fatalf("共享访问日志不对: %+v", logs)
	}
	var pages int64
	db.Model(&model.SubLog{}).Where("format = ?", "page-share").Count(&pages)
	if pages != 0 {
		t.Fatal("共享访问发的是订阅,不应记成 page")
	}

	// 再生成一次 = 换新,旧地址失效(始终只有一条)
	doReq(s, "POST", "/sub/alice?share=on", browserUA)
	if tok2 := token(t, db); tok2 == tok {
		t.Fatal("重新生成应换新令牌")
	} else if w := doReq(s, "GET", "/sub/"+tok, "curl/8.4.0"); w.Code != 404 {
		t.Fatalf("旧地址应失效,得 %d", w.Code)
	} else {
		tok = tok2
	}

	// 取消
	if w := doReq(s, "POST", "/sub/alice?share=off", browserUA); w.Code != http.StatusSeeOther {
		t.Fatalf("取消应 303,得 %d", w.Code)
	}
	db.First(&owner, 1)
	if token(t, db) != "" || len(owner.ShareCreds) != 0 {
		t.Fatalf("取消后令牌与共享凭据都要清空: %q %s", owner.ShareToken, owner.ShareCreds)
	}
	if w := doReq(s, "GET", "/sub/"+tok, "curl/8.4.0"); w.Code != 404 {
		t.Fatalf("取消后应 404,得 %d", w.Code)
	}
}

// 设置关掉后:订阅页不出卡片,已生成的地址立即失效,也不能再生成。
func TestShareDisabled(t *testing.T) {
	s, db := shareServer(t)
	doReq(s, "POST", "/sub/alice?share=on", browserUA)
	tok := token(t, db)
	db.Create(&model.Setting{Key: "subShareEnabled", Value: "false"})

	if w := doReq(s, "GET", "/sub/"+tok, "curl/8.4.0"); w.Code != 404 {
		t.Fatalf("关掉后共享地址应 404,得 %d", w.Code)
	}
	body := doReq(s, "GET", "/sub/alice", browserUA).Body.String()
	if strings.Contains(body, "share=on") {
		t.Fatal("关掉后订阅页不应出现共享卡片")
	}
	if w := doReq(s, "POST", "/sub/alice?share=on", browserUA); w.Code != 404 {
		t.Fatalf("关掉后不应能生成,得 %d", w.Code)
	}
}

// 副机:不提供生成/取消(会被主机同步覆盖),但主机建的共享地址照常发订阅。
func TestShareOnNode(t *testing.T) {
	s, db := shareServer(t)
	doReq(s, "POST", "/sub/alice?share=on", browserUA)
	tok := token(t, db)
	db.Create(&model.Setting{Key: "nodeMode", Value: "true"})

	if strings.Contains(doReq(s, "GET", "/sub/alice", browserUA).Body.String(), "share=on") {
		t.Fatal("副机订阅页不应出现共享卡片")
	}
	if w := doReq(s, "POST", "/sub/alice?share=off", browserUA); w.Code != 404 {
		t.Fatalf("副机不应能改共享状态,得 %d", w.Code)
	}
	if w := doReq(s, "GET", "/sub/"+tok, "curl/8.4.0"); w.Code != 200 {
		t.Fatalf("副机应照常按令牌发订阅,得 %d", w.Code)
	}
}

// 代理建的用户:订阅地址是随机令牌,link / clash / json 三种格式都要正常;
// 用户名路径不给(避免枚举),临时共享照常可用。
func TestResellerUserSubscription(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Reseller{Name: "dl", Enabled: true, PageEnabled: true, ShareOn: true})
	db.Model(&model.User{}).Where("id = ?", 1).
		Updates(map[string]interface{}{"reseller_id": 1, "sub_token": "TOKEN1234567890abcdefg"})

	if w := doReq(s, "GET", "/sub/alice", "curl/8.4.0"); w.Code != 404 {
		t.Fatalf("代理用户不应能按用户名订阅,得 %d", w.Code)
	}
	for _, f := range []string{"", "?format=clash", "?format=json"} {
		w := doReq(s, "GET", "/sub/TOKEN1234567890abcdefg"+f, "curl/8.4.0")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "hk.example") {
			t.Fatalf("格式 %q 令牌订阅不可用 (code %d): %s", f, w.Code, w.Body.String())
		}
	}
	// 落地页与二维码也走令牌地址
	page := doReq(s, "GET", "/sub/TOKEN1234567890abcdefg", browserUA).Body.String()
	if !strings.Contains(page, "/sub/TOKEN1234567890abcdefg?format=clash") {
		t.Fatal("落地页里的地址应是令牌地址")
	}
	if w := doReq(s, "GET", "/sub/TOKEN1234567890abcdefg/qr", browserUA); w.Code != 200 {
		t.Fatalf("二维码应可用,得 %d", w.Code)
	}
	// 代理关掉自己的订阅页:客户端照常拿订阅,浏览器不再出页面
	db.Model(&model.Reseller{}).Where("id = ?", 1).Update("page_enabled", false)
	if w := doReq(s, "GET", "/sub/TOKEN1234567890abcdefg", browserUA); w.Code != 200 || strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("关掉订阅页后应直接给订阅,得 %d", w.Code)
	}
}

// 老库升级上来的行,新列是 NULL:SQL 里 NULL = 0 不成立,订阅必须照样按用户名找得到人。
func TestLegacyNullResellerIdStillResolves(t *testing.T) {
	s, db := shareServer(t)
	db.Exec("UPDATE users SET reseller_id = NULL, sub_token = NULL WHERE id = 1")
	if w := doReq(s, "GET", "/sub/alice", "curl/8.4.0"); w.Code != 200 {
		t.Fatalf("历史用户的订阅不能 404,得 %d", w.Code)
	}
}

// 订阅标题带中文时必须编码:HTTP 头只能放 ASCII,原样发客户端会丢掉,
// 于是 Shadowrocket 显示域名、Clash Verge 显示用户名、nextin 显示首个节点名。
func TestProfileTitleEncoding(t *testing.T) {
	u := model.User{Name: "alice", Volume: 100, Up: 1, Down: 2}

	h := headers(u, Options{ProfileTitle: "冒央会社", UpdateHours: 12}, "text/plain")
	if h["Profile-Title"] != "base64:5YaS5aSu5Lya56S+" {
		t.Fatalf("中文标题应 base64 编码: %q", h["Profile-Title"])
	}
	if !strings.HasPrefix(h["Content-Disposition"], "attachment;filename*=UTF-8''") { // 分号后不能有空格
		t.Fatalf("Content-Disposition 格式不对: %q", h["Content-Disposition"])
	}
	if !strings.Contains(h["Content-Disposition"], "filename*=UTF-8''") {
		t.Fatalf("应带 RFC 5987 文件名: %q", h["Content-Disposition"])
	}
	// 中文标题时不能再给 ASCII 回退名:有的客户端只认第一个 filename=,
	// 会拿它当订阅名(Clash Verge 就会显示成用户名)
	if strings.Contains(h["Content-Disposition"], "filename=") {
		t.Fatalf("中文标题不应带 ASCII filename=: %q", h["Content-Disposition"])
	}
	for _, v := range h { // 头里不能出现非 ASCII
		for i := 0; i < len(v); i++ {
			if v[i] > 127 {
				t.Fatalf("响应头出现非 ASCII: %q", v)
			}
		}
	}
	if got := headers(u, Options{ProfileTitle: "Maoyang Node"}, "text/plain")["Profile-Title"]; got != "Maoyang Node" {
		t.Fatalf("纯 ASCII 标题应原样发: %q", got)
	}
	// 没配标题时回落到备注 → 用户名
	if got := headers(model.User{Name: "bob"}, Options{}, "text/plain")["Profile-Title"]; got != "bob" {
		t.Fatalf("应回落到用户名: %q", got)
	}
}

// 代理填了标题,他名下用户的订阅头就用代理的。
func TestResellerProfileTitle(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Setting{Key: "subProfileTitle", Value: "主站"})
	db.Create(&model.Reseller{Name: "dl", Enabled: true, PageEnabled: true, ShareOn: true, PageTitle: "代理站"})
	db.Model(&model.User{}).Where("id = ?", 1).
		Updates(map[string]interface{}{"reseller_id": 1, "sub_token": "TOKENabcdefghijklmnop"})

	w := doReq(s, "GET", "/sub/TOKENabcdefghijklmnop", "curl/8.4.0")
	want := "base64:" + base64.StdEncoding.EncodeToString([]byte("代理站"))
	if got := w.Header().Get("Profile-Title"); got != want {
		t.Fatalf("没填订阅标题时应回落到页面标题: %q", got)
	}
	// 代理单独设了订阅标题:优先于页面标题
	db.Model(&model.Reseller{}).Where("id = ?", 1).Update("profile_title", "代理机场")
	w = doReq(s, "GET", "/sub/TOKENabcdefghijklmnop", "curl/8.4.0")
	want = "base64:" + base64.StdEncoding.EncodeToString([]byte("代理机场"))
	if got := w.Header().Get("Profile-Title"); got != want {
		t.Fatalf("应使用代理的订阅标题: %q", got)
	}
}

// Shadowrocket 不读响应头,流量与到期看正文首行的 STATUS=;别的客户端不能收到这一行。
func TestShadowrocketStatusLine(t *testing.T) {
	s, _ := shareServer(t)
	rocket := doReq(s, "GET", "/sub/alice", "Shadowrocket/2288 CFNetwork/1494 Darwin/23.4.0").Body.String()
	if !strings.HasPrefix(rocket, "STATUS=") {
		t.Fatalf("小火箭应拿到 STATUS 行: %.60q", rocket)
	}
	if !strings.Contains(rocket, "hysteria2://") {
		t.Fatalf("STATUS 行之后仍要有节点: %.80q", rocket)
	}
	other := doReq(s, "GET", "/sub/alice", "clash-verge/1.5").Body.String()
	if strings.Contains(other, "STATUS=") {
		t.Fatalf("其它客户端不应收到 STATUS 行: %.60q", other)
	}
}
