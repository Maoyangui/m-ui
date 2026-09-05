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

// 代理的外部 API:令牌对应哪个代理就只看得到 / 改得动那个代理名下的用户、套餐与授权线路;
// 主面板的令牌不能用在代理入口上,反之亦然;停用的代理令牌立即失效。
func TestResellerPublicAPIScope(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}

	db.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})
	db.Create(&model.Line{Name: "jp", Protocol: "anytls", Port: 30444, Enabled: true})
	a := model.Reseller{Name: "a", Enabled: true, ApiEnabled: true, ApiToken: "tok-a-0123456789"}
	b := model.Reseller{Name: "b", Enabled: true, ApiEnabled: true, ApiToken: "tok-b-0123456789"}
	off := model.Reseller{Name: "off", Enabled: false, ApiEnabled: true, ApiToken: "tok-off-123456789"}
	db.Create(&a)
	db.Create(&b)
	db.Create(&off)
	db.Model(&model.Reseller{}).Where("id = ?", off.Id).Update("enabled", false) // gorm 的 default:true 会把 false 写成 true
	s.setResellerLines(a.Id, []uint{1})                                          // a 只有 hk
	s.setResellerLines(b.Id, []uint{2})
	db.Create(&model.Plan{Name: "a-plan", ResellerId: a.Id, VolumeGB: 10, Days: 30, LineIds: []byte(`[1]`)})
	db.Create(&model.Plan{Name: "b-plan", ResellerId: b.Id, VolumeGB: 10, Days: 30, LineIds: []byte(`[2]`)})
	db.Create(&model.Plan{Name: "master-plan", VolumeGB: 10, Days: 30, LineIds: []byte(`[1,2]`)})
	bu := model.User{Name: "b-user", Enabled: true, ResellerId: b.Id, SubToken: "bbbbbbbbbbbbbbbbbbbbbbbb"}
	db.Create(&bu)
	db.Create(&model.UserLine{UserId: bu.Id, LineId: 2})

	call := func(token, method, path, body string) (int, string) {
		req := httptest.NewRequest(method, "http://rs.example:2054/dl/api/v1/"+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.handleResellerPublicAPI(w, req)
		return w.Code, w.Body.String()
	}

	// 鉴权
	for _, tok := range []string{"", "wrong", "tok-off-123456789"} {
		if code, _ := call(tok, "GET", "ping", ""); code != http.StatusUnauthorized {
			t.Fatalf("令牌 %q 应 401,实际 %d", tok, code)
		}
	}
	if code, body := call("tok-a-0123456789", "GET", "ping", ""); code != 200 || !strings.Contains(body, `"role":"reseller"`) {
		t.Fatalf("ping 应通过并标明代理身份: %d %s", code, body)
	}
	// 主面板入口不认代理令牌(主面板 API 没开)
	req := httptest.NewRequest("GET", "http://x/app/api/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer tok-a-0123456789")
	w := httptest.NewRecorder()
	s.handlePublicAPI(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("代理令牌不该能用主面板入口: %d", w.Code)
	}

	// 套餐只看自己的
	if code, body := call("tok-a-0123456789", "GET", "plans", ""); code != 200 || !strings.Contains(body, "a-plan") || strings.Contains(body, "b-plan") || strings.Contains(body, "master-plan") {
		t.Fatalf("套餐列表应只含自己的: %d %s", code, body)
	}
	// 建用户:未授权线路被拒;授权线路通过,归到自己名下并发随机订阅令牌
	if code, body := call("tok-a-0123456789", "POST", "users", `{"name":"a-user","lineIds":[2]}`); code != 400 || !strings.Contains(body, "未授权") {
		t.Fatalf("未授权线路应被拒: %d %s", code, body)
	}
	code, body := call("tok-a-0123456789", "POST", "users", `{"name":"a-user","plan":"a-plan"}`)
	if code != 200 {
		t.Fatalf("建用户应通过: %d %s", code, body)
	}
	var created apiUserView
	json.Unmarshal([]byte(body), &created)
	var au model.User
	db.First(&au, created.Id)
	if au.ResellerId != a.Id || len(au.SubToken) < 20 || !strings.Contains(created.SubLink, au.SubToken) {
		t.Fatalf("应归到代理 a 名下并用随机令牌做订阅地址: %+v %s", au, created.SubLink)
	}
	if len(created.LineIds) != 1 || created.LineIds[0] != 1 {
		t.Fatalf("套餐线路应为 hk: %v", created.LineIds)
	}
	// 用别家的套餐 / 主面板套餐:都是"不存在"
	if code, _ := call("tok-a-0123456789", "POST", "users/a-user/plan", `{"plan":"b-plan"}`); code != 400 {
		t.Fatalf("别家的套餐应被拒: %d", code)
	}
	if code, _ := call("tok-a-0123456789", "POST", "users/a-user/plan", `{"plan":"master-plan"}`); code != 400 {
		t.Fatalf("主面板套餐应被拒: %d", code)
	}
	if code, _ := call("tok-a-0123456789", "POST", "users/a-user/plan", `{"plan":"a-plan","mode":"extend"}`); code != 200 {
		t.Fatalf("自己的套餐应通过: %d", code)
	}
	// 列表与定位只在自己名下
	if code, body := call("tok-a-0123456789", "GET", "users", ""); code != 200 || !strings.Contains(body, "a-user") || strings.Contains(body, "b-user") {
		t.Fatalf("用户列表应只含自己的: %d %s", code, body)
	}
	if code, _ := call("tok-a-0123456789", "GET", "users/b-user", ""); code != http.StatusNotFound {
		t.Fatalf("别家的用户应 404: %d", code)
	}
	if code, _ := call("tok-a-0123456789", "GET", "users/"+strconv.Itoa(int(bu.Id)), ""); code != http.StatusNotFound {
		t.Fatalf("按 id 也不能摸到别家的用户: %d", code)
	}
	// 修改:改到未授权线路被拒;停用通过
	if code, _ := call("tok-a-0123456789", "PATCH", "users/a-user", `{"lineIds":[2]}`); code != 400 {
		t.Fatalf("改到未授权线路应被拒: %d", code)
	}
	if code, body := call("tok-a-0123456789", "POST", "users/a-user/disable", ""); code != 200 || !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("停用应通过: %d %s", code, body)
	}
	// 越界操作:删别家的用户 404;外部节点分配被忽略;别家的用户不能被启停 / 重置 / 踢线
	if code, _ := call("tok-a-0123456789", "DELETE", "users/b-user", ""); code != http.StatusNotFound {
		t.Fatalf("删别家的用户应 404: %d", code)
	}
	for _, act := range []string{"enable", "disable", "reset", "kick", "plan"} {
		if code, _ := call("tok-a-0123456789", "POST", "users/b-user/"+act, `{"plan":"a-plan"}`); code != http.StatusNotFound {
			t.Fatalf("对别家用户 %s 应 404: %d", act, code)
		}
	}
	db.Create(&model.ExtNode{Name: "ext", Type: "link", Value: "vless://x", Enabled: true})
	if code, _ := call("tok-a-0123456789", "PATCH", "users/a-user", `{"extIds":[1]}`); code != 200 {
		t.Fatalf("带 extIds 的修改应通过但忽略: %d", code)
	}
	var ne int64
	db.Model(&model.UserExt{}).Where("user_id = ?", created.Id).Count(&ne)
	if ne != 0 {
		t.Fatal("代理不能分配外部节点")
	}
	// 用户数上限也管外部 API
	db.Model(&model.Reseller{}).Where("id = ?", a.Id).Update("user_limit", 1)
	if code, body := call("tok-a-0123456789", "POST", "users", `{"name":"a-user-2"}`); code != 400 || !strings.Contains(body, "用户数上限") {
		t.Fatalf("达到用户数上限应被拒: %d %s", code, body)
	}
	// 代理关掉 API 后令牌失效
	db.Model(&model.Reseller{}).Where("id = ?", a.Id).Update("api_enabled", false)
	if code, _ := call("tok-a-0123456789", "GET", "ping", ""); code != http.StatusUnauthorized {
		t.Fatalf("关掉后应 401: %d", code)
	}
}
