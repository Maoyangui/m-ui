package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 规则的「忽略部分线路」(用户 11-23):不给沿用、新勾的要存在、删掉的悄悄去掉、空数组就是关。
func TestRuleSkipLines(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.User{Name: "a", Enabled: true})
	db.Create(&model.Line{Name: "便宜", Protocol: "anytls", Port: 1001})
	db.Create(&model.Line{Name: "正常", Protocol: "anytls", Port: 1002})
	call := func(method, path, body string) (int, ruleView, string) {
		req := httptest.NewRequest(method, "http://x/app/api"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		if strings.HasPrefix(path, "/rules/") {
			s.handleRuleItem(w, req)
		} else {
			s.handleRules(w, req)
		}
		var v ruleView
		json.Unmarshal(w.Body.Bytes(), &v)
		return w.Code, v, w.Body.String()
	}
	stored := func() model.Rule {
		var r model.Rule
		db.First(&r, 1)
		return r
	}
	// 新建:不给 = 不忽略;视图里是空数组
	code, v, body := call("POST", "/rules", `{"name":"晚高峰","kind":"schedule","allUsers":true,"start":"19:00","end":"23:00","downMbps":10}`)
	if code != 200 || v.SkipLineIds == nil || len(v.SkipLineIds) != 0 || len(stored().SkipLineIds) != 0 {
		t.Fatalf("没给忽略的线路就是不忽略: %d %s", code, body)
	}
	// 勾了不存在的线路:拒绝
	if code, _, body = call("PUT", "/rules/1", `{"name":"晚高峰","kind":"schedule","allUsers":true,"start":"19:00","end":"23:00","downMbps":10,"skipLineIds":[1,5]}`); code != 400 || !strings.Contains(body, "线路 #5 不存在") {
		t.Fatalf("勾了不存在的线路应拒绝: %d %s", code, body)
	}
	// 勾两条:去重、排序后存下
	code, v, body = call("PUT", "/rules/1", `{"name":"晚高峰","kind":"schedule","allUsers":true,"start":"19:00","end":"23:00","downMbps":10,"skipLineIds":[2,1,2,0]}`)
	if code != 200 || string(stored().SkipLineIds) != `[1,2]` || len(v.SkipLineIds) != 2 {
		t.Fatalf("应存成 [1,2]: %d %s / %s", code, body, stored().SkipLineIds)
	}
	// 请求里不带这个字段(老的调用方):沿用原值
	if code, _, body = call("PUT", "/rules/1", `{"name":"晚高峰","kind":"schedule","allUsers":true,"start":"19:00","end":"23:00","downMbps":20}`); code != 200 || string(stored().SkipLineIds) != `[1,2]` {
		t.Fatalf("没带这个字段应沿用原值: %d %s / %s", code, body, stored().SkipLineIds)
	}
	// 勾着的线路被删了:规则页的开关把整条规则原样送回来,不能因此报错,删掉的那条悄悄去掉
	db.Delete(&model.Line{}, 2)
	if code, _, body = call("PUT", "/rules/1", `{"name":"晚高峰","kind":"schedule","allUsers":true,"start":"19:00","end":"23:00","downMbps":20,"enabled":false,"skipLineIds":[1,2]}`); code != 200 || string(stored().SkipLineIds) != `[1]` || stored().Enabled {
		t.Fatalf("勾着的线路删了,开关照样拨得动、那条去掉: %d %s / %s", code, body, stored().SkipLineIds)
	}
	// 给空数组:关掉
	if code, _, body = call("PUT", "/rules/1", `{"name":"晚高峰","kind":"schedule","allUsers":true,"start":"19:00","end":"23:00","downMbps":20,"skipLineIds":[]}`); code != 200 || len(stored().SkipLineIds) != 0 {
		t.Fatalf("给空数组就是不忽略: %d %s / %s", code, body, stored().SkipLineIds)
	}
}
