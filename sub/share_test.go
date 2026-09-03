package sub

import (
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
