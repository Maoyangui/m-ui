package web

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 请求体:订阅地址(/users/lookup 的 sub,就是用户的凭据)、令牌之类的值不进日志,任意层级都打码;不是 JSON 的只记字节数;太长截断。
func TestAPILogBody(t *testing.T) {
	got := apiLogBody([]byte(`{"sub":"https://panel.example/sub/abc","name":"alice","nested":{"token":"t1","days":30},"list":[{"password":"p"}]}`))
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("记下的请求体应还是 JSON: %q", got)
	}
	if m["sub"] != "***" || m["name"] != "alice" || m["nested"].(map[string]interface{})["token"] != "***" || m["list"].([]interface{})[0].(map[string]interface{})["password"] != "***" {
		t.Fatalf("敏感的值没打码: %s", got)
	}
	if strings.Contains(got, "abc") || strings.Contains(got, "t1") {
		t.Fatalf("订阅地址 / 令牌进了日志: %s", got)
	}
	if got := apiLogBody([]byte("not json at all")); got != "(15 bytes)" {
		t.Fatalf("不是 JSON 只记字节数: %q", got)
	}
	if got := apiLogBody([]byte(`{"remark":"` + strings.Repeat("长", 900) + `"}`)); len([]rune(got)) != apiLogBodyMax+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("太长的请求体应截到 %d 字: %d", apiLogBodyMax, len([]rune(got)))
	}
	if apiLogBody(nil) != "" || apiLogBody([]byte("  ")) != "" {
		t.Fatal("没有请求体就空着")
	}
}

// 每个接口都认得出做了什么、对谁(界面按它拼大白话);路径从 /api/v1/ 起,前面面板的路径(本身是秘密)不进日志。
func TestAPILogAction(t *testing.T) {
	cases := []struct {
		method, path, body, action, target string
	}{
		{"GET", "/app/api/v1/ping", "", "ping", ""},
		{"GET", "/app/api/v1/overview", "", "overview", ""},
		{"GET", "/app/api/v1/servers", "", "servers", ""},
		{"GET", "/app/api/v1/lines", "", "lines", ""},
		{"GET", "/app/api/v1/onlines", "", "onlines", ""},
		{"GET", "/app/api/v1/plans", "", "plans", ""},
		{"GET", "/app/api/v1/users", "", "users.list", ""},
		{"POST", "/app/api/v1/users", `{"name":"alice","days":30}`, "user.create", "alice"},
		{"POST", "/app/api/v1/users/lookup", `{"sub":"x"}`, "user.lookup", ""},
		{"POST", "/app/api/v1/users/batch", `{"prefix":"t","count":5}`, "users.batch", "5"},
		{"GET", "/app/api/v1/users/bob", "", "user.get", "bob"},
		{"PATCH", "/app/api/v1/users/bob", `{}`, "user.update", "bob"},
		{"PUT", "/app/api/v1/users/bob", `{}`, "user.update", "bob"},
		{"DELETE", "/app/api/v1/users/bob", "", "user.delete", "bob"},
		{"GET", "/app/api/v1/users/bob/traffic", "", "user.traffic", "bob"},
		{"GET", "/app/api/v1/users/bob/sub", "", "user.sub", "bob"},
		{"GET", "/app/api/v1/users/bob/plan-ops", "", "user.planOps", "bob"},
		{"POST", "/app/api/v1/users/bob/enable", "", "user.enable", "bob"},
		{"POST", "/app/api/v1/users/bob/disable", "", "user.disable", "bob"},
		{"POST", "/app/api/v1/users/bob/reset", "", "user.reset", "bob"},
		{"POST", "/app/api/v1/users/bob/kick", "", "user.kick", "bob"},
		{"POST", "/app/api/v1/users/bob/rotate", "", "user.rotate", "bob"},
		{"POST", "/app/api/v1/users/bob/renew", `{"periods":1}`, "user.renew", "bob"},
		{"POST", "/app/api/v1/users/bob/plan", `{}`, "user.renew", "bob"},
		{"POST", "/app/api/v1/users/bob/change", `{}`, "user.change", "bob"},
		{"GET", "/app/api/v1/what", "", "unknown", ""},
	}
	for _, c := range cases {
		path, parts := apiLogPath(c.path)
		if !strings.HasPrefix(path, "/api/v1/") || strings.Contains(path, "/app/") {
			t.Fatalf("%s:记下的路径应从 /api/v1/ 起: %s", c.path, path)
		}
		a, tg := apiLogAction(c.method, parts, []byte(c.body))
		if a != c.action || tg != c.target {
			t.Fatalf("%s %s:得 %s / %s,应 %s / %s", c.method, c.path, a, tg, c.action, c.target)
		}
	}
}

// waitLogs 写日志是另起协程写的:等它落库
func waitLogs(t *testing.T, e *pcEnv, keyID uint, n int) []model.ApiKeyLog {
	t.Helper()
	var rows []model.ApiKeyLog
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		rows = nil
		e.db.Where("key_id = ?", keyID).Order("id asc").Find(&rows)
		if len(rows) >= n || time.Now().After(end) {
			return rows
		}
	}
}

// 调用日志:成功、没权限、来源不允许的都记;大白话要的动作和对象、原始请求(请求体打码)、状态、失败原因都在;
// 关掉以后不再记;保留天数只能选那几档;一键清空;删密钥连日志一起删;代理的密钥只记在代理自己那把下面,代理看不到主面板的日志。
func TestAPIKeyLogs(t *testing.T) {
	e := newPCEnv(t)
	e.mkUser(model.User{Name: "bob"}, 1)
	k := e.mkKey(0, "商城", permUsersRead, permUsersCreate)
	src := "203.0.113.9:4000"
	if w := e.keyCall(false, src, k.Token, "GET", "users/bob", ""); w.Code != 200 {
		t.Fatalf("查用户: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(false, src, k.Token, "POST", "users/lookup", `{"sub":"https://panel.example/sub/bob"}`); w.Code != 200 && w.Code != 404 {
		t.Fatalf("按订阅地址查: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(false, src, k.Token, "DELETE", "users/bob", ""); w.Code != 403 {
		t.Fatalf("没有删除权限应 403: %d", w.Code)
	}
	rows := waitLogs(t, e, k.Id, 3)
	if len(rows) != 3 {
		t.Fatalf("应记 3 条,得 %d", len(rows))
	}
	if r := rows[0]; r.Action != "user.get" || r.Target != "bob" || r.Status != 200 || r.Method != "GET" || r.Path != "/api/v1/users/bob" || r.IP != "203.0.113.9" || r.Err != "" {
		t.Fatalf("查用户那条不对: %+v", r)
	}
	if r := rows[1]; r.Action != "user.lookup" || strings.Contains(r.Body, "sub/bob") || !strings.Contains(r.Body, `"sub":"***"`) {
		t.Fatalf("按订阅地址查的那条:订阅地址不该进日志: %+v", r)
	}
	if r := rows[2]; r.Action != "user.delete" || r.Status != 403 || !strings.Contains(r.Err, "权限") {
		t.Fatalf("没权限的那条应记 403 和原因: %+v", r)
	}
	// 来源不允许:也记一笔
	if w := e.setKey(0, k.Id, `{"ipAllow":true,"ipList":"198.51.100.1"}`); w.Code != 200 {
		t.Fatalf("限来源: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(false, src, k.Token, "GET", "users", ""); w.Code != 403 {
		t.Fatalf("来源不在名单应 403: %d", w.Code)
	}
	if rows = waitLogs(t, e, k.Id, 4); len(rows) != 4 || rows[3].Status != 403 || !strings.Contains(rows[3].Err, "来源 IP") || rows[3].Action != "users.list" {
		t.Fatalf("来源不允许的那条: %+v", rows)
	}
	e.setKey(0, k.Id, `{"ipAllow":false}`)

	// 看日志:新的在前,分页
	var page struct {
		Items      []model.ApiKeyLog `json:"items"`
		More       bool              `json:"more"`
		Total      int64             `json:"total"`
		LogOff     bool              `json:"logOff"`
		LogDays    int               `json:"logDays"`
		DayChoices []int             `json:"dayChoices"`
	}
	get := func(rid uint, q string) int {
		t.Helper()
		sub := "keys/" + keyID(k.Id) + "/logs"
		r := httptest.NewRequest("GET", "http://panel.example:2053/app/api/admin/api/"+sub+q, nil) // 参数在网址里,sub 只有路径(和面板的路由一样)
		w := httptest.NewRecorder()
		e.s.handleAPIKeys(w, r, rid, sub, func(string, interface{}) {})
		page.Items = nil
		if w.Code == 200 {
			_ = json.Unmarshal(w.Body.Bytes(), &page)
		}
		return w.Code
	}
	if code := get(0, "?limit=3"); code != 200 || len(page.Items) != 3 || !page.More || page.Total != 4 || page.Items[0].Id <= page.Items[1].Id || page.LogOff || page.LogDays != 7 || !reflect.DeepEqual(page.DayChoices, []int{1, 7, 30, 90}) {
		t.Fatalf("第一页: %d %+v", code, page)
	}
	if code := get(0, "?limit=3&before="+keyID(page.Items[2].Id)); code != 200 || len(page.Items) != 1 || page.More {
		t.Fatalf("第二页: %d %+v", code, page)
	}
	// 设置:保留天数只认那几档;关掉以后不再记
	if w := e.manage(0, "PUT", "keys/"+keyID(k.Id)+"/logs", `{"days":5}`); w.Code != 400 {
		t.Fatalf("5 天不在可选的几档里应 400: %d", w.Code)
	}
	if w := e.manage(0, "PUT", "keys/"+keyID(k.Id)+"/logs", `{"days":30,"off":true}`); w.Code != 200 {
		t.Fatalf("改日志设置: %d %s", w.Code, w.Body.String())
	}
	if get(0, ""); !page.LogOff || page.LogDays != 30 {
		t.Fatalf("设置没存上: %+v", page)
	}
	e.keyCall(false, src, k.Token, "GET", "users/bob", "")
	time.Sleep(300 * time.Millisecond)
	if rows = waitLogs(t, e, k.Id, 5); len(rows) != 4 {
		t.Fatalf("关了日志不该再记: %d 条", len(rows))
	}
	e.manage(0, "PUT", "keys/"+keyID(k.Id)+"/logs", `{"off":false}`)
	e.keyCall(false, src, k.Token, "GET", "users/bob", "")
	if rows = waitLogs(t, e, k.Id, 5); len(rows) != 5 {
		t.Fatalf("重新打开后照常记: %d 条", len(rows))
	}

	// 代理:它的密钥记在自己名下;看不到主面板密钥的日志
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-default-token-0123456789"}
	e.db.Create(&rs)
	e.db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 1})
	rk := e.mkKey(rs.Id, "代理的机器人", permUsersRead)
	if w := e.keyCall(true, src, rk.Token, "GET", "users", ""); w.Code != 200 {
		t.Fatalf("代理的密钥查用户: %d", w.Code)
	}
	if rr := waitLogs(t, e, rk.Id, 1); len(rr) != 1 || rr[0].Action != "users.list" {
		t.Fatalf("代理那把的日志: %+v", rr)
	}
	if code := get(rs.Id, ""); code != 404 {
		t.Fatalf("代理看主面板那把的日志应 404: %d", code)
	}

	// 一键清空;删密钥连日志一起删
	if w := e.manage(0, "DELETE", "keys/"+keyID(k.Id)+"/logs", ""); w.Code != 200 {
		t.Fatalf("清空日志: %d", w.Code)
	}
	if rows = waitLogs(t, e, k.Id, 1); len(rows) != 0 {
		t.Fatalf("清空后应一条不剩: %d", len(rows))
	}
	e.keyCall(true, src, rk.Token, "GET", "users", "")
	waitLogs(t, e, rk.Id, 2)
	if w := e.manage(rs.Id, "DELETE", "keys/"+keyID(rk.Id), ""); w.Code != 200 {
		t.Fatalf("删代理的密钥: %d", w.Code)
	}
	var n int64
	e.db.Model(&model.ApiKeyLog{}).Where("key_id = ?", rk.Id).Count(&n)
	if n != 0 {
		t.Fatalf("删密钥时日志应一起删: 剩 %d 条", n)
	}
}

// 网址里的参数:和请求体同一份敏感键打码(现在的接口不从网址收这些,调用方放错了地方也不记下来);其余原样、顺序不变。
func TestAPILogQuery(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "",
		"q=alice&state=expired": "q=alice&state=expired",
		"q=alice&sub=https%3A%2F%2Fp.example%2Fs": "q=alice&sub=***",
		"Token=t1&limit=5&PASSWORD=x":             "Token=***&limit=5&PASSWORD=***",
		"su%62=abc&enabled=true":                  "su%62=***&enabled=true", // 键转义过也认得
		"a%zz=1&q=x":                              "a%zz=***&q=x",           // 键解不开:值也打码
		"flag&key":                                "flag&key",               // 没有值的照原样
	} {
		if got := apiLogQuery(in); got != want {
			t.Errorf("apiLogQuery(%q) = %q,应为 %q", in, got, want)
		}
	}
	if got := apiLogQuery("q=" + strings.Repeat("长", 400)); len([]rune(got)) != apiLogQueryMax+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("太长的参数应截到 %d 字: %d", apiLogQueryMax, len([]rune(got)))
	}
}
