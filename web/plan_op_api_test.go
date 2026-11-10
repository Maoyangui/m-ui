package web

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 套餐操作接口的边界:续当前套餐在事务里解析、套餐字段的有无、幂等重放排在最前、代理探不到别人的套餐、
// bind 的预览不落库、重放与首次同一个结构、PATCH 带套餐走同一个执行器。

// 「续当前套餐」的目标在事务里按重新读到的用户解析:请求开始时读到的还是月付,并发的换套餐已经提交成季付,
// 续的就是季付;以前按请求开始时的月付续,把用户静默改回了旧套餐。
func TestPlanRenewCurrentResolvedInTx(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	monthly := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30})
	quarterly := e.mkPlan(model.Plan{Name: "季付", VolumeGB: 30, Days: 90})
	e.mkUser(model.User{Name: "alice", PlanId: monthly.Id, Volume: 10 << 30, Expiry: now + 10*86400})
	stale := e.user("alice")
	e.db.Model(&model.User{}).Where("id = ?", stale.Id).Updates(map[string]interface{}{"plan_id": quarterly.Id, "volume": int64(30 << 30)})

	for _, dry := range []bool{true, false} {
		r := httptest.NewRequest("POST", "/", nil)
		res, err := e.s.renewUserPlan(r, planScope{actor: "t"}, stale, planOpReq{DryRun: dry}, true)
		if err != nil {
			t.Fatalf("dryRun=%v: %v", dry, err)
		}
		if res.To.PlanId != quarterly.Id || res.To.Plan != "季付" || res.To.Expiry != stale.Expiry+90*86400 {
			t.Fatalf("dryRun=%v:应续事务里那一刻的当前套餐(季付): %+v", dry, res.To)
		}
	}
	if u := e.user("alice"); u.PlanId != quarterly.Id || u.Expiry != stale.Expiry+90*86400 || u.Volume != 30<<30 {
		t.Fatalf("落库:仍是季付、加 90 天: %+v", u)
	}
	var op model.PlanOp
	e.db.Last(&op)
	if op.ToPlanId != quarterly.Id || op.FromPlanId != quarterly.Id {
		t.Fatalf("套餐记录: %+v", op)
	}
}

// 只有请求里完全没有套餐字段(含空请求体)才续当前套餐;带了套餐字段但为 0、空串或 null,和以前一样 400。
func TestPlanRenewPlanFieldPresence(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	monthly := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30})
	alice := e.mkUser(model.User{Name: "alice", PlanId: monthly.Id, Volume: 10 << 30, Expiry: now + 86400})
	exp := alice.Expiry
	for _, body := range []string{`{"planId":0}`, `{"plan":""}`, `{"plan":"  "}`, `{"planId":null}`, `{"plan":null}`, `{"PLANID":0}`, `{"planId":0,"plan":null}`} {
		for _, ep := range []string{"plan", "renew"} {
			pcCode(t, e.api("POST", "users/alice/"+ep, body), 400, "plan_required")
		}
		if w := e.panel(0, "POST", "users/"+strconv.Itoa(int(alice.Id))+"/plan", body); w.Code != 400 {
			t.Errorf("面板 %s 应 400: %d %s", body, w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`null`, `[]`, `"x"`} {
		if w := e.api("POST", "users/alice/renew", body); w.Code != 400 {
			t.Errorf("请求体 %s 不是对象应 400: %d %s", body, w.Code, w.Body.String())
		}
	}
	if u := e.user("alice"); u.Expiry != exp || e.count(&model.PlanOp{}) != 0 {
		t.Fatalf("400 的请求不能落库: %+v", u)
	}
	for i, body := range []string{"", "  ", `{}`, `{"periods":1}`, `{"mode":"extend"}`} {
		if w := e.api("POST", "users/alice/renew", body); w.Code != 200 {
			t.Fatalf("没带套餐字段 %q 续当前套餐: %d %s", body, w.Code, w.Body.String())
		}
		if got := e.user("alice").Expiry; got != exp+int64(i+1)*30*86400 {
			t.Fatalf("%q 续了一期: %+d 天", body, (got-exp)/86400)
		}
	}
}

// 幂等重放排在找套餐、校验期数、代理校验之前:已经成功的请求用同键同内容重试,套餐后来改名 / 删了、天数改了、
// 代理额度用完了,照样 200 重放;同键不同内容仍是 409。
func TestPlanOpReplayBeforeChecks(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	monthly := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30})
	yearly := e.mkPlan(model.Plan{Name: "年付", VolumeGB: 100, Days: 365})
	e.mkUser(model.User{Name: "alice", PlanId: monthly.Id, Volume: 10 << 30, Expiry: now + 86400})

	if w := e.api("POST", "users/alice/renew", `{"plan":"月付","requestId":"k1"}`); w.Code != 200 {
		t.Fatalf("首次: %d %s", w.Code, w.Body.String())
	}
	exp := e.user("alice").Expiry
	e.db.Model(&model.Plan{}).Where("id = ?", monthly.Id).Update("name", "月付-新")
	w := e.api("POST", "users/alice/renew", `{"plan":"月付","requestId":"k1"}`)
	if w.Code != 200 || w.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("套餐改名后同键重试应重放: %d %s", w.Code, w.Body.String())
	}
	if w := e.api("POST", "users/alice/plan", `{"plan":"年付","periods":2,"requestId":"k2"}`); w.Code != 200 {
		t.Fatalf("年付两期: %d %s", w.Code, w.Body.String())
	}
	e.db.Model(&model.Plan{}).Where("id = ?", yearly.Id).Update("days", 20000)
	if w := e.api("POST", "users/alice/plan", `{"plan":"年付","periods":2,"requestId":"k2"}`); w.Code != 200 || w.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("天数改了以后同键重试应重放(不是 bad_periods): %d %s", w.Code, w.Body.String())
	}
	if w := e.api("POST", "users/alice/change", `{"plan":"年付","mode":"stack","requestId":"k3"}`); w.Code != 200 {
		t.Fatalf("叠加: %d %s", w.Code, w.Body.String())
	}
	e.db.Delete(&model.Plan{}, yearly.Id)
	w = e.api("POST", "users/alice/change", `{"plan":"年付","mode":"stack","requestId":"k3"}`)
	if w.Code != 200 || w.Header().Get("Idempotent-Replayed") != "true" || pcResult(t, w).User == nil {
		t.Fatalf("套餐删了以后同键重试应重放: %d %s", w.Code, w.Body.String())
	}
	pcCode(t, e.api("POST", "users/alice/renew", `{"plan":"月付","requestId":"k1","periods":2}`), 409, "idempotency_conflict")
	pcCode(t, e.api("POST", "users/alice/change", `{"plan":"不存在","mode":"stack","requestId":"k3"}`), 409, "idempotency_conflict")
	if got := e.user("alice").Expiry; got <= exp {
		t.Fatalf("到期: %d", got)
	}
	ops := e.count(&model.PlanOp{})

	// 代理:成功之后额度用完,同键重试照样重放;令牌下代理停用 / 到期是 401(鉴权那一步就拒)
	a := model.Reseller{Name: "a", Enabled: true, ApiEnabled: true, ApiToken: "tok-a-0123456789", Volume: 10 << 30}
	e.db.Create(&a)
	e.s.setResellerLines(a.Id, []uint{1})
	ap := e.mkPlan(model.Plan{Name: "a-月付", ResellerId: a.Id, VolumeGB: 5, Days: 30})
	au := e.mkUser(model.User{Name: "a-u", ResellerId: a.Id, PlanId: ap.Id, SubToken: "aaaaaaaaaaaaaaaaaaaaaaaa"}, 1)
	const tok = "tok-a-0123456789"
	if w := e.rapi(tok, "POST", "users/a-u/renew", `{"requestId":"r1"}`); w.Code != 200 {
		t.Fatalf("代理续费: %d %s", w.Code, w.Body.String())
	}
	e.db.Model(&model.User{}).Where("id = ?", au.Id).Update("total_down", int64(20<<30))
	if w := e.rapi(tok, "POST", "users/a-u/renew", `{"requestId":"r1"}`); w.Code != 200 || w.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("额度用完后同键重试应重放(不是 reseller_denied): %d %s", w.Code, w.Body.String())
	}
	pcCode(t, e.rapi(tok, "POST", "users/a-u/renew", `{"requestId":"r2"}`), 400, "reseller_denied")
	if n := e.count(&model.PlanOp{}); n != ops+1 {
		t.Fatalf("重放不能再记: %d", n-ops)
	}
	e.db.Model(&model.Reseller{}).Where("id = ?", a.Id).Update("enabled", false)
	if w := e.rapi(tok, "POST", "users/a-u/renew", `{"requestId":"r1"}`); w.Code != 401 {
		t.Fatalf("代理停用后令牌 401: %d", w.Code)
	}
}

// 面板 mode=bind 的 dryRun 只校验:不改归属、不写审计、不触发规则重判。
func TestPlanBindDryRun(t *testing.T) {
	e := newPCEnv(t)
	p := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30})
	u := e.mkUser(model.User{Name: "alice"})
	id := strconv.Itoa(int(u.Id))
	changes := e.count(&model.Change{})
	w := e.panel(0, "POST", "users/"+id+"/plan", `{"planId":`+strconv.Itoa(int(p.Id))+`,"mode":"bind","dryRun":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"dryRun":true`) {
		t.Fatalf("bind 预览: %d %s", w.Code, w.Body.String())
	}
	if e.user("alice").PlanId != 0 || e.count(&model.Change{}) != changes || e.rules != 0 {
		t.Fatalf("预览不能落库: plan=%d 审计 +%d 规则 %d", e.user("alice").PlanId, e.count(&model.Change{})-changes, e.rules)
	}
	if w := e.panel(0, "POST", "users/"+id+"/plan?dryRun=true", `{"planId":999,"mode":"bind"}`); w.Code != 400 {
		t.Fatalf("预览照样校验套餐: %d", w.Code)
	}
	if w := e.panel(0, "POST", "users/"+id+"/plan", `{"planId":`+strconv.Itoa(int(p.Id))+`,"mode":"bind"}`); w.Code != 200 || e.user("alice").PlanId != p.Id || e.rules != 1 {
		t.Fatalf("正式执行照常: %d %s", w.Code, w.Body.String())
	}
}

// 代理面板:别人的套餐和不存在的套餐得到同一个错误,期数校验排在归属核对之后(探不出别人的套餐和它的天数);
// 预览与套餐记录里不出现别人套餐的名字。
func TestPlanOpResellerCannotProbePlans(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	a := model.Reseller{Name: "a", Enabled: true}
	b := model.Reseller{Name: "b", Enabled: true}
	e.db.Create(&a)
	e.db.Create(&b)
	e.s.setResellerLines(a.Id, []uint{1})
	master := e.mkPlan(model.Plan{Name: "主面板机密套餐", VolumeGB: 500, Days: 30, DeviceLimit: 9})
	long := e.mkPlan(model.Plan{Name: "b-长", ResellerId: b.Id, VolumeGB: 5, Days: 20000})
	mine := e.mkPlan(model.Plan{Name: "a-月付", ResellerId: a.Id, VolumeGB: 5, Days: 30})
	au := e.mkUser(model.User{Name: "a-u", ResellerId: a.Id, PlanId: mine.Id, Expiry: now + 86400, SubToken: "aaaaaaaaaaaaaaaaaaaaaaaa"}, 1)
	id := strconv.Itoa(int(au.Id))
	body := func(pid uint) string { return `{"planId":` + strconv.Itoa(int(pid)) + `,"periods":2,"dryRun":true}` }

	other, missing := e.panel(a.Id, "POST", "users/"+id+"/plan", body(long.Id)), e.panel(a.Id, "POST", "users/"+id+"/plan", body(9999))
	pcCode(t, other, 400, "plan_not_found")
	if other.Body.String() != missing.Body.String() {
		t.Fatalf("别人的套餐与不存在的套餐要同一个错误:\n%s\n%s", other.Body.String(), missing.Body.String())
	}
	batch := func(pid uint) string {
		return e.panel(a.Id, "POST", "users/batch", `{"ids":[`+id+`],"action":"plan","planId":`+strconv.Itoa(int(pid))+`,"periods":2}`).Body.String()
	}
	if x, y := batch(long.Id), batch(9999); x != y || !strings.Contains(x, "套餐不存在") {
		t.Fatalf("批量也一样:\n%s\n%s", x, y)
	}
	// 主面板把主面板的套餐套到了代理的用户上:代理预览时看不到那个套餐的名字
	e.db.Model(&model.User{}).Where("id = ?", au.Id).Update("plan_id", master.Id)
	w := e.panel(a.Id, "POST", "users/"+id+"/change", `{"planId":`+strconv.Itoa(int(mine.Id))+`,"mode":"replace","dryRun":true}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "主面板机密套餐") {
		t.Fatalf("代理的预览里不能出现别人套餐的名字: %d %s", w.Code, w.Body.String())
	}
	if r := pcResult(t, w); r.From.PlanId != master.Id || r.From.Plan != "" {
		t.Fatalf("from: %+v", r.From)
	}
	// 续当前套餐:当前套餐不是自己的,同样是「套餐不存在」
	pcCode(t, e.panel(a.Id, "POST", "users/"+id+"/plan", `{}`), 400, "plan_not_found")
	if w := e.panel(a.Id, "POST", "users/"+id+"/change", `{"planId":`+strconv.Itoa(int(mine.Id))+`,"mode":"replace","force":true}`); w.Code != 200 {
		t.Fatalf("执行: %d %s", w.Code, w.Body.String())
	}
	// 主面板又把主面板的套餐续给了这个用户:代理看这条记录时也不显示那个套餐的名字(id 照留)
	if w := e.panel(0, "POST", "users/"+id+"/plan", `{"planId":`+strconv.Itoa(int(master.Id))+`}`); w.Code != 200 {
		t.Fatalf("主面板续费: %d %s", w.Code, w.Body.String())
	}
	if w := e.panel(0, "GET", "users/"+id+"/plan-ops", ""); !strings.Contains(w.Body.String(), "主面板机密套餐") {
		t.Fatalf("主面板自己看得到: %s", w.Body.String())
	}
	w = e.panel(a.Id, "GET", "users/"+id+"/plan-ops", "")
	var ops []model.PlanOp
	if json.Unmarshal(w.Body.Bytes(), &ops) != nil || len(ops) != 2 || strings.Contains(w.Body.String(), "主面板机密套餐") {
		t.Fatalf("套餐记录里也不能有别人套餐的名字: %s", w.Body.String())
	}
	if ops[0].Actor != planOpMasterTag || ops[0].ToPlanId != master.Id || ops[0].ToPlan != "" || ops[0].FromPlan != "a-月付" || ops[1].ToPlan != "a-月付" {
		t.Fatalf("自己的套餐名照常显示,别人的只留 id: %+v", ops)
	}
}

// 幂等重放与第一次的响应是同一个结构:/plan、/renew 是用户对象,/change 是结果对象加用户对象,面板 /plan 是 {ok, expiry}。
func TestPlanOpReplaySameShape(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	p := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30})
	e.mkPlan(model.Plan{Name: "季付", VolumeGB: 30, Days: 90})
	u := e.mkUser(model.User{Name: "alice", PlanId: p.Id, Volume: 10 << 30, Expiry: now + 86400})
	keys := func(w *httptest.ResponseRecorder) []string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("不是对象: %s", w.Body.String())
		}
		var out []string
		for k := range m {
			if k != "replayed" {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return out
	}
	for _, c := range []struct{ path, body string }{
		{"users/alice/plan", `{"requestId":"s1"}`},
		{"users/alice/renew", `{"requestId":"s2","periods":2}`},
		{"users/alice/change", `{"plan":"季付","mode":"stack","requestId":"s3"}`},
	} {
		first := e.api("POST", c.path, c.body)
		again := e.api("POST", c.path, c.body)
		if first.Code != 200 || again.Code != 200 || again.Header().Get("Idempotent-Replayed") != "true" {
			t.Fatalf("%s: %d %d", c.path, first.Code, again.Code)
		}
		if !reflect.DeepEqual(keys(first), keys(again)) {
			t.Fatalf("%s 重放的结构与第一次不同:\n%v\n%v", c.path, keys(first), keys(again))
		}
	}
	path := "users/" + strconv.Itoa(int(u.Id)) + "/plan"
	first, again := e.panel(0, "POST", path, `{"requestId":"s4"}`), e.panel(0, "POST", path, `{"requestId":"s4"}`)
	if !reflect.DeepEqual(keys(first), keys(again)) || !strings.Contains(again.Body.String(), `"replayed":true`) {
		t.Fatalf("面板 /plan: %s / %s", first.Body.String(), again.Body.String())
	}
}

// PATCH / PUT /users 带套餐等同一次续费:走同一个执行器,记套餐记录,认 requestId / Idempotency-Key;
// 请求与响应的格式不变(显式字段优先于套餐,返回用户对象,审计仍是 update:plan:<套餐名>)。
func TestPatchUserWithPlanUsesExecutor(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	monthly := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30, LineIds: []byte(`[1]`)})
	e.mkUser(model.User{Name: "alice", Volume: 1 << 30, Up: 1 << 20, Expiry: now + 10*86400}, 2)
	exp := e.user("alice").Expiry

	w := e.api("PATCH", "users/alice", `{"plan":"月付","remark":"VIP"}`)
	var view apiUserView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.Name != "alice" || view.PlanId != monthly.Id || view.Remark != "VIP" {
		t.Fatalf("返回用户对象: %d %s", w.Code, w.Body.String())
	}
	u := e.user("alice")
	if u.Expiry != exp+30*86400 || u.Volume != 10<<30 || u.Up != 0 || u.TotalUp != 1<<20 || !reflect.DeepEqual(pcUserLines(e.db, u.Id), []uint{1}) {
		t.Fatalf("按套餐续了一期、线路换成套餐的: %+v %v", u, pcUserLines(e.db, u.Id))
	}
	var op model.PlanOp
	e.db.Last(&op)
	if e.count(&model.PlanOp{}) != 1 || op.Op != "renew" || op.ToPlanId != monthly.Id || op.Actor != "api" || op.UserId != u.Id {
		t.Fatalf("要记一条套餐记录: %+v", op)
	}
	var ch model.Change
	e.db.Last(&ch)
	if ch.Action != "update:plan:月付" || string(ch.Obj) != `"alice"` {
		t.Fatalf("审计格式不变: %+v %s", ch, ch.Obj)
	}

	// 幂等:同键同内容重放,不再加时;同键不同内容 409(改了名,重试按 id 找人)
	exp = e.user("alice").Expiry
	byID := "users/" + strconv.Itoa(int(u.Id))
	for i, want := range []string{"", "true"} {
		w := e.api("PATCH", byID, `{"plan":"月付","mode":"extend","days":7,"name":"alice2"}`, "Idempotency-Key", "p-1")
		if w.Code != 200 || w.Header().Get("Idempotent-Replayed") != want || !strings.Contains(w.Body.String(), `"name":"alice2"`) {
			t.Fatalf("第 %d 次: %d %q %s", i+1, w.Code, w.Header().Get("Idempotent-Replayed"), w.Body.String())
		}
	}
	if u := e.user("alice2"); u.Expiry != exp+7*86400 {
		t.Fatalf("days 替代套餐天数、只加一次: %+d 天", (u.Expiry-exp)/86400)
	}
	pcCode(t, e.api("PATCH", byID, `{"plan":"月付","days":8,"requestId":"p-1"}`), 409, "idempotency_conflict")
	if n := e.count(&model.PlanOp{}); n != 2 {
		t.Fatalf("记录 %d 条", n)
	}
	// 不带套餐的 PATCH 不变:不记套餐记录
	if w := e.api("PATCH", "users/alice2", `{"remark":"x"}`); w.Code != 200 || e.count(&model.PlanOp{}) != 2 {
		t.Fatalf("普通修改: %d", w.Code)
	}
	// 显式字段的校验照旧:不合法时 400,不落库、不记录
	if w := e.api("PATCH", "users/alice2", `{"plan":"月付","volume":-1}`); w.Code != 400 || e.count(&model.PlanOp{}) != 2 {
		t.Fatalf("负数配额应 400: %d %s", w.Code, w.Body.String())
	}
}
