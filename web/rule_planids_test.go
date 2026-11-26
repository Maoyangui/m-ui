package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 改规则时换的套餐目标要存得进去:5ee96d9 加「按套餐选人」时,PUT 的列清单漏了 plan_ids,
// 新建时存得进去、改的时候换的套餐悄悄丢掉(保存成功,规则还按原来的套餐生效)。
func TestRulePutKeepsPlanIds(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	call := func(method, path, body string) (int, string) {
		req := httptest.NewRequest(method, "http://x/app/api"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		if strings.HasPrefix(path, "/rules/") {
			s.handleRuleItem(w, req)
		} else {
			s.handleRules(w, req)
		}
		return w.Code, w.Body.String()
	}
	stored := func() model.Rule {
		var r model.Rule
		db.First(&r, 1)
		return r
	}
	if code, body := call("POST", "/rules", `{"name":"晚高峰","kind":"schedule","planIds":[1],"start":"19:00","end":"23:00","downMbps":10}`); code != 200 || string(stored().PlanIds) != `[1]` {
		t.Fatalf("新建时套餐目标存得进去: %d %s", code, body)
	}
	if code, body := call("PUT", "/rules/1", `{"name":"晚高峰","kind":"schedule","planIds":[3,4],"start":"19:00","end":"23:00","downMbps":20}`); code != 200 || string(stored().PlanIds) != `[3,4]` {
		t.Fatalf("改规则时换的套餐目标应存下: %d %s / %s", code, body, stored().PlanIds)
	}
	if code, body := call("PUT", "/rules/1", `{"name":"晚高峰","kind":"schedule","allUsers":true,"start":"19:00","end":"23:00","downMbps":20}`); code != 200 || len(stored().PlanIds) != 0 {
		t.Fatalf("改回全部用户,套餐目标清掉: %d %s / %s", code, body, stored().PlanIds)
	}
}
