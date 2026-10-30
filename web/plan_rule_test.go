package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/rules"

	"gorm.io/gorm"
)

func planRuleServer(t *testing.T) (*Server, *gorm.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	return &Server{db: db}, db
}

func planReq(t *testing.T, h http.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func mkRule(t *testing.T, db *gorm.DB, name string, planIDs ...uint) model.Rule {
	t.Helper()
	b, _ := json.Marshal(planIDs)
	r := model.Rule{Name: name, Kind: "schedule", Enabled: true, Start: "00:00", End: "00:00", DownMbps: 10, PlanIds: b}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	return r
}

// 规则按套餐选人:现有的、以后才套用的都算;换了套餐、移出套餐就不算。
func TestRuleTargetsFollowPlan(t *testing.T) {
	_, db := planRuleServer(t)
	gold := model.Plan{Name: "金", VolumeGB: 10}
	silver := model.Plan{Name: "银", VolumeGB: 5}
	db.Create(&gold)
	db.Create(&silver)
	alice := model.User{Name: "alice", Enabled: true}
	bob := model.User{Name: "bob", Enabled: true}
	db.Create(&alice)
	db.Create(&bob)

	apply := func(id uint, p model.Plan) {
		var u model.User
		db.First(&u, id)
		applyPlan(&u, p, "renew", 1000)
		db.Model(&model.User{}).Where("id = ?", u.Id).Update("plan_id", u.PlanId)
	}
	apply(alice.Id, gold)

	rule := mkRule(t, db, "晚高峰", gold.Id)
	targets := func() []string {
		var users []model.User
		db.Find(&users)
		var out []string
		for _, x := range rules.Targets(rule, users) {
			out = append(out, x.Name)
		}
		return out
	}
	if got := fmt.Sprint(targets()); got != "[alice]" {
		t.Fatalf("现有套餐用户要被选中: %s", got)
	}
	// 以后才套用的也算
	apply(bob.Id, gold)
	if got := fmt.Sprint(targets()); got != "[alice bob]" {
		t.Fatalf("后续套用的也要被选中: %s", got)
	}
	// 换套餐就不算了
	apply(alice.Id, silver)
	if got := fmt.Sprint(targets()); got != "[bob]" {
		t.Fatalf("换了套餐不该再被选中: %s", got)
	}
	// 停用的用户不参与(规则判定的老约定)
	db.Model(&model.User{}).Where("id = ?", bob.Id).Update("enabled", false)
	if got := fmt.Sprint(targets()); got != "[]" {
		t.Fatalf("停用的用户不参与: %s", got)
	}
}

// 套餐表单选规则:改的是规则里的套餐列表,两头看到的是同一份。没带 ruleIds 不动,删套餐一并清掉。
func TestPlanRuleIdsSync(t *testing.T) {
	s, db := planRuleServer(t)
	a := mkRule(t, db, "A")
	b := mkRule(t, db, "B")

	w := planReq(t, s.handlePlans, http.MethodPost, "/plans", fmt.Sprintf(`{"name":"金","volumeGb":0.5,"days":30,"ruleIds":[%d]}`, a.Id))
	if w.Code != 200 {
		t.Fatalf("新增套餐: %d %s", w.Code, w.Body)
	}
	var created model.Plan
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.VolumeGB != 0.5 {
		t.Fatalf("小数额度没存住: %v", created.VolumeGB)
	}
	plansOf := func(id uint) string {
		var r model.Rule
		db.First(&r, id)
		return fmt.Sprint(rules.ParseIDs(r.PlanIds))
	}
	if plansOf(a.Id) != fmt.Sprint([]uint{created.Id}) || plansOf(b.Id) != "[]" {
		t.Fatalf("规则里的套餐不对: A=%s B=%s", plansOf(a.Id), plansOf(b.Id))
	}

	// 列表带上规则与用户数
	db.Create(&model.User{Name: "alice", Enabled: true, PlanId: created.Id})
	w = planReq(t, s.handlePlans, http.MethodGet, "/plans", "")
	var views []planView
	if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || fmt.Sprint(views[0].RuleIds) != fmt.Sprint([]uint{a.Id}) || views[0].UserCount != 1 {
		t.Fatalf("套餐列表不对: %+v", views)
	}

	put := func(body string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/plans/%d", created.Id), strings.NewReader(body))
		rr := httptest.NewRecorder()
		s.handlePlanItem(rr, r)
		if rr.Code != 200 {
			t.Fatalf("修改套餐: %d %s", rr.Code, rr.Body)
		}
	}
	// 换成只跟 B
	put(fmt.Sprintf(`{"name":"金","ruleIds":[%d]}`, b.Id))
	if plansOf(a.Id) != "[]" || plansOf(b.Id) != fmt.Sprint([]uint{created.Id}) {
		t.Fatalf("改规则后不对: A=%s B=%s", plansOf(a.Id), plansOf(b.Id))
	}
	// 没带 ruleIds(外部程序按老格式改套餐)不动
	put(`{"name":"金币"}`)
	if plansOf(b.Id) != fmt.Sprint([]uint{created.Id}) {
		t.Fatalf("没带 ruleIds 不该动规则: B=%s", plansOf(b.Id))
	}

	// 删套餐:用户移出、规则清掉
	r := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/plans/%d", created.Id), nil)
	rr := httptest.NewRecorder()
	s.handlePlanItem(rr, r)
	if rr.Code != 200 {
		t.Fatalf("删套餐: %d %s", rr.Code, rr.Body)
	}
	if plansOf(b.Id) != "[]" {
		t.Fatalf("删套餐后规则里还留着: %s", plansOf(b.Id))
	}
	var u model.User
	db.Where("name = ?", "alice").First(&u)
	if u.PlanId != 0 {
		t.Fatalf("删套餐后用户还指着它: %d", u.PlanId)
	}
}

// 首次连接开始计时:新号不算到期;正在计时的老用户续费照常顺延;没开始计时的再续就累加。
func TestApplyPlanFirstUse(t *testing.T) {
	now := int64(1_000_000)
	p := model.Plan{Id: 3, Days: 30, FirstUse: true}
	var u model.User
	applyPlan(&u, p, "new", now)
	if u.Expiry != 0 || u.ActivateDays != 30 || u.PlanId != 3 {
		t.Fatalf("新号要等首次连接: %+v", u)
	}
	applyPlan(&u, p, "renew", now)
	if u.Expiry != 0 || u.ActivateDays != 60 {
		t.Fatalf("没开始计时的再续应累加: %+v", u)
	}
	running := model.User{Expiry: now + 86400}
	applyPlan(&running, p, "renew", now)
	if running.Expiry != now+31*86400 || running.ActivateDays != 0 {
		t.Fatalf("正在计时的要接着顺延: %+v", running)
	}
	expired := model.User{Expiry: now - 86400}
	applyPlan(&expired, p, "renew", now)
	if expired.Expiry != 0 || expired.ActivateDays != 30 {
		t.Fatalf("已过期的续费后重新等首次连接: %+v", expired)
	}
	// 不限时长的套餐把等待也清掉
	waiting := model.User{ActivateDays: 7}
	applyPlan(&waiting, model.Plan{Days: 0}, "renew", now)
	if waiting.ActivateDays != 0 || waiting.Expiry != 0 {
		t.Fatalf("不限时长的套餐要清掉等待: %+v", waiting)
	}
}

// 用户归属:归入 / 移出不动额度;跨归属的套餐不收。
func TestBindUserPlan(t *testing.T) {
	s, db := planRuleServer(t)
	own := model.Plan{Name: "主面板的", VolumeGB: 10}
	other := model.Plan{Name: "代理的", ResellerId: 5, VolumeGB: 10}
	db.Create(&own)
	db.Create(&other)
	u := model.User{Name: "alice", Enabled: true, Volume: 123, Expiry: 456}
	db.Create(&u)
	if err := s.bindUserPlan([]model.User{u}, own.Id); err != nil {
		t.Fatal(err)
	}
	var got model.User
	db.First(&got, u.Id)
	if got.PlanId != own.Id || got.Volume != 123 || got.Expiry != 456 {
		t.Fatalf("只归入不该动额度: %+v", got)
	}
	if err := s.bindUserPlan([]model.User{u}, other.Id); err == nil {
		t.Fatal("跨归属的套餐应该被拒绝")
	}
	if err := s.bindUserPlan([]model.User{u}, 0); err != nil {
		t.Fatal(err)
	}
	db.First(&got, u.Id)
	if got.PlanId != 0 {
		t.Fatalf("移出后应为 0: %d", got.PlanId)
	}
}

// 模板写坏了不让存;好的能存。
func TestValidateSubTemplates(t *testing.T) {
	for _, c := range []struct {
		in map[string]string
		ok bool
	}{
		{map[string]string{"subClashExt": "mixed-port: 7890\nrules: [MATCH,Proxy]\n"}, true},
		{map[string]string{"subClashExt": ""}, true},
		{map[string]string{"subClashExt": "a: [坏"}, false},
		{map[string]string{"subClashExt": "- 只是一个列表"}, false},
		{map[string]string{"subSingboxExt": `{"route":{}}`}, true},
		{map[string]string{"subSingboxExt": `{坏`}, false},
		{map[string]string{"subSingboxExt": `[1]`}, false},
		{map[string]string{"subSingboxExt": strings.Repeat("x", maxSubTemplate+1)}, false},
	} {
		if err := validateSubTemplates(c.in); (err == nil) != c.ok {
			t.Errorf("%v: 结果 %v,应该 ok=%v", c.in, err, c.ok)
		}
	}
}
