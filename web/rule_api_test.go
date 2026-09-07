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

func TestRuleCRUDAndValidation(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.User{Name: "a", Enabled: true})
	db.Create(&model.User{Name: "b", Enabled: true, ResellerId: 2})
	db.Create(&model.User{Name: "c"})
	db.Model(&model.User{}).Where("name = ?", "c").Update("enabled", false) // gorm 的 default:true 会把 Create 里的 false 写成 true

	call := func(method, path, body string) (int, string) {
		req := httptest.NewRequest(method, "http://x/app/api"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		if strings.HasPrefix(path, "/rules/") {
			s.handleRuleItem(w, req)
		} else if path == "/limitstates" {
			s.handleLimitStates(w, req)
		} else {
			s.handleRules(w, req)
		}
		return w.Code, w.Body.String()
	}
	// 校验:没目标、没限速、时刻格式、突发参数
	for _, bad := range []string{
		`{"name":"x","kind":"schedule","start":"19:00","end":"23:00","downMbps":10}`,
		`{"name":"x","kind":"schedule","allUsers":true,"start":"19:00","end":"23:00"}`,
		`{"name":"x","kind":"schedule","allUsers":true,"start":"7pm","end":"23:00","downMbps":10}`,
		`{"name":"x","kind":"burst","allUsers":true,"windowMin":0,"thresholdGb":1,"penaltyMin":20,"downMbps":10}`,
		`{"name":"x","kind":"burst","allUsers":true,"windowMin":10,"thresholdGb":0,"penaltyMin":20,"downMbps":10}`,
		`{"name":"","kind":"burst","allUsers":true,"windowMin":10,"thresholdGb":1,"penaltyMin":20,"downMbps":10}`,
	} {
		if code, body := call("POST", "/rules", bad); code != 400 {
			t.Fatalf("应拒绝 %s: %d %s", bad, code, body)
		}
	}
	code, body := call("POST", "/rules", `{"name":"晚高峰","kind":"schedule","userIds":[1,3],"resellerIds":[2],"days":"5,1,1","start":"19:00","end":"23:00","upMbps":30,"downMbps":30}`)
	if code != 200 {
		t.Fatalf("新建失败: %d %s", code, body)
	}
	var v ruleView
	json.Unmarshal([]byte(body), &v)
	if !v.Enabled || !v.TightenOnly || v.Days != "1,5" || v.TargetCount != 2 || len(v.UserIds) != 2 || len(v.ResellerIds) != 1 {
		t.Fatalf("默认启用、默认只升不降、星期去重排序、目标人数(a + 代理 2 的 b,停用的 c 不算): %+v", v)
	}
	code, body = call("POST", "/rules", `{"name":"突发","kind":"burst","allUsers":true,"windowMin":10,"thresholdGb":1.5,"penaltyMin":20,"downMbps":20,"tightenOnly":false,"enabled":false}`)
	if code != 200 {
		t.Fatalf("新建突发失败: %d %s", code, body)
	}
	json.Unmarshal([]byte(body), &v)
	if v.Enabled || v.TightenOnly || v.ThresholdGB != 1.5 || v.ThresholdBytes != 1610612736 || v.TargetCount != 0 {
		t.Fatalf("显式给的开关要生效,停用的规则目标人数记 0: %+v", v)
	}
	code, body = call("GET", "/rules", "")
	var list []ruleView
	json.Unmarshal([]byte(body), &list)
	if code != 200 || len(list) != 2 {
		t.Fatalf("列表: %d %s", code, body)
	}
	// 修改:没给 enabled / tightenOnly 时沿用原值
	code, body = call("PUT", "/rules/2", `{"name":"突发2","kind":"burst","allUsers":true,"windowMin":30,"thresholdGb":2,"penaltyMin":30,"downMbps":10}`)
	if code != 200 {
		t.Fatalf("修改失败: %d %s", code, body)
	}
	json.Unmarshal([]byte(body), &v)
	if v.Name != "突发2" || v.Enabled || v.TightenOnly || v.WindowMin != 30 {
		t.Fatalf("修改应沿用未给的开关: %+v", v)
	}
	// 生效状态接口与删除级联
	db.Create(&model.LimitState{RuleId: 1, UserId: 1, UserName: "a", RuleName: "晚高峰", DownMbps: 30})
	code, body = call("GET", "/rules/1/states", "")
	if code != 200 || !strings.Contains(body, `"userName":"a"`) {
		t.Fatalf("状态列表: %d %s", code, body)
	}
	code, body = call("GET", "/limitstates", "")
	if code != 200 || !strings.Contains(body, `"ruleName":"晚高峰"`) {
		t.Fatalf("全部状态: %d %s", code, body)
	}
	if code, _ := call("DELETE", "/rules/1", ""); code != 200 {
		t.Fatal("删除失败")
	}
	var n int64
	db.Model(&model.LimitState{}).Count(&n)
	if n != 0 {
		t.Fatal("删规则应连带清掉它的生效状态")
	}
	if code, _ := call("GET", "/rules/1", ""); code != 404 {
		t.Fatal("删掉的规则应 404")
	}
}
