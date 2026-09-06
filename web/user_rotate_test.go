package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 重置订阅链接:地址换成随机令牌(哪怕主面板按用户名作地址)、凭据全部换新、临时共享收回;
// 主面板路由、代理面板路由(只限自己名下)、外部 API 两种作用域都走同一处。
func TestRotateUser(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})

	// 主面板用户:按用户名作地址(sub_token 空),并开着临时共享
	u := model.User{Name: "alice", Enabled: true, Credentials: generateCredentials("alice"),
		ShareToken: "share-old-token", ShareCreds: generateCredentials("alice"), ShareAt: 1}
	db.Create(&u)
	db.Create(&model.UserLine{UserId: u.Id, LineId: 1})
	if subKey(u) != "alice" {
		t.Fatal("前提:主面板用户按用户名作订阅地址")
	}
	before := string(u.Credentials)
	rotate := func(id uint) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://x/app/api/users/"+strconv.Itoa(int(id))+"/rotate", nil)
		w := httptest.NewRecorder()
		s.handleUserItem(w, req)
		return w
	}
	w := rotate(u.Id)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"link"`) {
		t.Fatalf("重置应返回新的订阅地址: %d %s", w.Code, w.Body.String())
	}
	load := func(id uint) model.User { var x model.User; db.First(&x, id); return x } // 每次用新结构体:gorm 会把已有主键当条件
	var v model.User
	v = load(u.Id)
	if len(v.SubToken) < 20 {
		t.Fatalf("地址应换成随机令牌: %q", v.SubToken)
	}
	if string(v.Credentials) == before {
		t.Fatal("凭据应全部换新")
	}
	if v.ShareToken != "" || len(v.ShareCreds) != 0 || v.ShareAt != 0 {
		t.Fatalf("临时共享应一并收回: %+v", v)
	}
	if subKey(v) != v.SubToken {
		t.Fatal("旧的按用户名的地址不能再用")
	}
	var links map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &links)
	if !strings.HasSuffix(links["link"], v.SubToken) || links["share"] != "" {
		t.Fatalf("返回的地址应带新令牌且不再有共享地址: %v", links)
	}
	// 再重置一次:令牌、凭据再换
	first, firstCreds := v.SubToken, string(v.Credentials)
	rotate(u.Id)
	v = load(u.Id)
	if v.SubToken == first || string(v.Credentials) == firstCreds {
		t.Fatal("每次重置都应换新")
	}
	second := v.SubToken
	// 只接受 POST
	req := httptest.NewRequest("GET", "http://x/app/api/users/"+strconv.Itoa(int(u.Id))+"/rotate", nil)
	w = httptest.NewRecorder()
	s.handleUserItem(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405: %d", w.Code)
	}

	// 代理面板:只能重置自己名下的用户
	a := model.Reseller{Name: "a", Enabled: true, ApiEnabled: true, ApiToken: "tok-a-0123456789"}
	b := model.Reseller{Name: "b", Enabled: true, ApiEnabled: true, ApiToken: "tok-b-0123456789"}
	db.Create(&a)
	db.Create(&b)
	bu := model.User{Name: "b-user", Enabled: true, ResellerId: b.Id, SubToken: "bbbbbbbbbbbbbbbbbbbbbbbb", Credentials: generateCredentials("b-user")}
	db.Create(&bu)
	path := "http://rs/dl/api/users/" + strconv.Itoa(int(bu.Id)) + "/rotate"
	req = httptest.NewRequest("POST", path, nil)
	req = req.WithContext(withScope(req, a.Id))
	w = httptest.NewRecorder()
	s.handleUserItem(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("别的代理的用户应 403: %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest("POST", path, nil)
	req = req.WithContext(withScope(req, b.Id))
	w = httptest.NewRecorder()
	s.handleUserItem(w, req)
	if w.Code != 200 {
		t.Fatalf("自己的用户应能重置: %d %s", w.Code, w.Body.String())
	}
	v = load(bu.Id)
	if v.SubToken == "bbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatal("代理面板重置后令牌应换新")
	}

	// 外部 API:代理令牌只在自己名下找;主面板作用域能重置任何人
	call := func(token, p string) (int, string) {
		req := httptest.NewRequest("POST", "http://rs.example:2054/dl/api/v1/"+p, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.handleResellerPublicAPI(w, req)
		return w.Code, w.Body.String()
	}
	if code, body := call("tok-a-0123456789", "users/b-user/rotate"); code != http.StatusNotFound {
		t.Fatalf("代理 a 看不到 b 的用户: %d %s", code, body)
	}
	// 主面板的用户对代理 API 不可见:按名字、按 id 都不行,令牌与凭据不能被代理换掉
	beforeAlice := load(u.Id)
	for _, key := range []string{"alice", strconv.Itoa(int(u.Id))} {
		if code, body := call("tok-b-0123456789", "users/"+key+"/rotate"); code != http.StatusNotFound {
			t.Fatalf("代理 API 不能碰主面板用户 %s: %d %s", key, code, body)
		}
	}
	if after := load(u.Id); after.SubToken != beforeAlice.SubToken || string(after.Credentials) != string(beforeAlice.Credentials) {
		t.Fatal("主面板用户的令牌与凭据不该被代理 API 改动")
	}
	// 代理面板路由同样只认自己名下:主面板用户 403
	req = httptest.NewRequest("POST", "http://rs/dl/api/users/"+strconv.Itoa(int(u.Id))+"/rotate", nil)
	req = req.WithContext(withScope(req, b.Id))
	w = httptest.NewRecorder()
	s.handleUserItem(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("代理面板不能重置主面板用户: %d %s", w.Code, w.Body.String())
	}
	prev := v.SubToken
	code, body := call("tok-b-0123456789", "users/b-user/rotate")
	v = load(bu.Id)
	if code != 200 || v.SubToken == prev || !strings.Contains(body, v.SubToken) {
		t.Fatalf("代理令牌应能重置自己的用户并返回新地址: %d %s", code, body)
	}
	req = httptest.NewRequest("POST", "http://x/app/api/v1/users/alice/rotate", nil)
	w = httptest.NewRecorder()
	s.publicAPI(w, req, apiScope{actor: "api"})
	var alice model.User
	db.Where("name = ?", "alice").First(&alice)
	if w.Code != 200 || alice.SubToken == second || !strings.Contains(w.Body.String(), alice.SubToken) {
		t.Fatalf("主面板 API 应能重置并返回新地址: %d %s", w.Code, w.Body.String())
	}
}
