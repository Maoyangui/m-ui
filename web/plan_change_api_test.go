package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// 套餐操作的接口:外部 API(主令牌 / 代理令牌)与面板内部路由,预览不落库、幂等、事务内重读、作用域与护栏。

const pcMaster = "master-token-0123456789"

type pcEnv struct {
	t     *testing.T
	s     *Server
	db    *gorm.DB
	rules int64
}

func newPCEnv(t *testing.T) *pcEnv {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	e := &pcEnv{t: t, db: db, s: &Server{db: db}}
	e.s.onRulesNow = func() { atomic.AddInt64(&e.rules, 1) }
	db.Create(&model.Setting{Key: "apiEnabled", Value: "true"})
	db.Create(&model.Setting{Key: "apiToken", Value: pcMaster})
	db.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})
	db.Create(&model.Line{Name: "jp", Protocol: "anytls", Port: 30444, Enabled: true})
	return e
}

func (e *pcEnv) do(h http.HandlerFunc, url, token, method, body string, hdr []string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// api 主面板令牌调外部 API。
func (e *pcEnv) api(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	return e.do(e.s.handlePublicAPI, "http://panel.example:2053/app/api/v1/"+path, pcMaster, method, body, hdr)
}

// rapi 代理令牌调代理面板上的外部 API。
func (e *pcEnv) rapi(token, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	return e.do(e.s.handleResellerPublicAPI, "http://rs.example:2054/dl/api/v1/"+path, token, method, body, hdr)
}

// panel 面板内部的用户接口;rid > 0 是代理面板的会话。
func (e *pcEnv) panel(rid uint, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://panel.example:2053/app/api/"+path, strings.NewReader(body))
	if rid > 0 {
		r = r.WithContext(withScope(r, rid))
	}
	w := httptest.NewRecorder()
	e.s.handleUserItem(w, r)
	return w
}

func (e *pcEnv) user(name string) model.User {
	e.t.Helper()
	var u model.User
	if err := e.db.Where("name = ?", name).First(&u).Error; err != nil {
		e.t.Fatalf("找不到用户 %s: %v", name, err)
	}
	return u
}

func (e *pcEnv) count(tbl interface{}, where ...interface{}) int64 {
	var n int64
	q := e.db.Model(tbl)
	if len(where) > 0 {
		q = q.Where(where[0], where[1:]...)
	}
	q.Count(&n)
	return n
}

func (e *pcEnv) mkPlan(p model.Plan) model.Plan {
	e.t.Helper()
	if err := e.db.Create(&p).Error; err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *pcEnv) mkUser(u model.User, lines ...uint) model.User {
	e.t.Helper()
	u.Enabled = true
	if err := e.db.Create(&u).Error; err != nil {
		e.t.Fatal(err)
	}
	for _, l := range lines {
		e.db.Create(&model.UserLine{UserId: u.Id, LineId: l})
	}
	return u
}

func pcResult(t *testing.T, w *httptest.ResponseRecorder) planResult {
	t.Helper()
	var r planResult
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("返回不是结果对象: %v %s", err, w.Body.String())
	}
	return r
}

func pcCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != status || body.Code != code || body.Error == "" {
		t.Fatalf("应返回 %d %s,实际 %d %s", status, code, w.Code, w.Body.String())
	}
}

func pcUserLines(db *gorm.DB, id uint) []uint {
	var ids []uint
	db.Model(&model.UserLine{}).Where("user_id = ?", id).Order("line_id asc").Pluck("line_id", &ids)
	return ids
}

// 主令牌换套餐:预览一个字节都不写;正式执行改用户与线路,记一条套餐记录和一条老格式的审计,并立刻重判规则。
func TestPlanChangeDryRunThenExecute(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	monthly := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30, DeviceLimit: 3, LineIds: []byte(`[1]`)})
	premium := e.mkPlan(model.Plan{Name: "高级月付", VolumeGB: 300, Days: 30, AutoReset: true, ResetDays: 30, DeviceLimit: 5, LineIds: []byte(`[1,2]`)})
	alice := e.mkUser(model.User{Name: "alice", PlanId: monthly.Id, Volume: 100 << 30, Up: 10 << 30, Down: 20 << 30, Expiry: now + 20*86400,
		AutoReset: true, ResetDays: 30, NextReset: now + 20*86400, DeviceLimit: 3, CreatedAt: now}, 1)

	before := e.user("alice")
	ops, changes, links := e.count(&model.PlanOp{}), e.count(&model.Change{}), e.count(&model.UserLine{})
	for _, path := range []string{"users/alice/change", "users/alice/change?dryRun=true"} {
		body := `{"plan":"高级月付","mode":"stack","dryRun":true}`
		if strings.Contains(path, "?") {
			body = `{"plan":"高级月付","mode":"stack"}`
		}
		w := e.api("POST", path, body)
		if w.Code != 200 {
			t.Fatalf("预览失败: %d %s", w.Code, w.Body.String())
		}
		r := pcResult(t, w)
		if !r.DryRun || r.OpId != 0 || r.User != nil || r.To.Volume != 300<<30 || r.To.Expiry != alice.Expiry+30*86400 || r.Direction != "upgrade" ||
			r.From.Plan != "月付" || r.To.Plan != "高级月付" || len(r.To.LineRefs) != 2 {
			t.Fatalf("预览内容: %s", w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "per_period_not_stacked") {
			t.Fatalf("叠加到周期重置套餐应提示每期额度不叠加: %s", w.Body.String())
		}
	}
	if after := e.user("alice"); !reflect.DeepEqual(after, before) || e.count(&model.PlanOp{}) != ops || e.count(&model.Change{}) != changes || e.count(&model.UserLine{}) != links {
		t.Fatal("dryRun 不能写库")
	}
	if e.rules != 0 {
		t.Fatal("预览不该触发规则重判")
	}

	w := e.api("POST", "users/alice/change", `{"plan":"高级月付","mode":"stack","remark":"订单 1001"}`)
	if w.Code != 200 {
		t.Fatalf("执行失败: %d %s", w.Code, w.Body.String())
	}
	r := pcResult(t, w)
	if r.DryRun || r.Replayed || r.OpId == 0 || r.User == nil || r.User.PlanId != premium.Id {
		t.Fatalf("执行结果应带 opId 与最新的用户对象: %s", w.Body.String())
	}
	u := e.user("alice")
	if u.PlanId != premium.Id || u.Volume != 300<<30 || u.Expiry != alice.Expiry+30*86400 || u.DeviceLimit != 5 || u.Up+u.Down != 30<<30 || u.NextReset != alice.NextReset {
		t.Fatalf("落库的用户: %+v", u)
	}
	if got := pcUserLines(e.db, u.Id); !reflect.DeepEqual(got, []uint{1, 2}) {
		t.Fatalf("套餐带线路就替换: %v", got)
	}
	var op model.PlanOp
	e.db.Last(&op)
	if e.count(&model.PlanOp{}) != ops+1 || op.ResellerId != 0 || op.Op != "stack" || op.FromPlan != "月付" || op.ToPlan != "高级月付" ||
		op.Actor != "api" || op.Remark != "订单 1001" || op.RequestId != nil || op.UserId != u.Id || op.Direction != "upgrade" {
		t.Fatalf("套餐记录: %+v", op)
	}
	var ch model.Change
	e.db.Last(&ch)
	if e.count(&model.Change{}) != changes+1 || ch.Action != "plan:stack:高级月付" || string(ch.Obj) != `"alice"` || ch.Actor != "api" || ch.Key != "user" {
		t.Fatalf("审计沿用 plan:<动作>:<套餐名>,obj 是用户名: %+v %s", ch, ch.Obj)
	}
	if e.rules != 1 {
		t.Fatalf("换了套餐要立刻重判规则一次,实际 %d", e.rules)
	}
}

// 幂等:同键重放、Idempotency-Key 请求头、同键不同内容 409、不同作用域互不干扰、预览与 409 不占用键。
func TestPlanOpIdempotency(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	monthly := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30})
	small := e.mkPlan(model.Plan{Name: "轻量", VolumeGB: 10, Days: 30, AutoReset: true, ResetDays: 30})
	e.mkUser(model.User{Name: "alice", PlanId: monthly.Id, Volume: 100 << 30, Up: 20 << 30, Expiry: now + 10*86400, AutoReset: true, ResetDays: 30, NextReset: now + 10*86400})
	exp0 := e.user("alice").Expiry

	w := e.api("POST", "users/alice/renew", `{"requestId":"order-1"}`)
	if w.Code != 200 || w.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("首次续费: %d %s", w.Code, w.Body.String())
	}
	if got := e.user("alice").Expiry; got != exp0+30*86400 {
		t.Fatalf("续当前套餐一期: %+d 天", (got-exp0)/86400)
	}
	w = e.api("POST", "users/alice/renew", `{"requestId":"order-1"}`)
	if w.Code != 200 || w.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("同键重试应重放: %d %v", w.Code, w.Header())
	}
	var view apiUserView
	json.Unmarshal(w.Body.Bytes(), &view)
	if got := e.user("alice").Expiry; got != exp0+30*86400 || view.Expiry != got || e.count(&model.PlanOp{}) != 1 {
		t.Fatalf("重放不能再加时: 到期 %+d 天,记录 %d 条", (got-exp0)/86400, e.count(&model.PlanOp{}))
	}
	// Idempotency-Key 请求头,效果一样
	for i, want := range []string{"", "true"} {
		w = e.api("POST", "users/alice/renew", `{"periods":2}`, "Idempotency-Key", "order-2")
		if w.Code != 200 || w.Header().Get("Idempotent-Replayed") != want {
			t.Fatalf("请求头第 %d 次: %d %q", i+1, w.Code, w.Header().Get("Idempotent-Replayed"))
		}
	}
	if got := e.user("alice").Expiry; got != exp0+90*86400 {
		t.Fatalf("两期只加一次: %+d 天", (got-exp0)/86400)
	}
	// 同键不同内容
	pcCode(t, e.api("POST", "users/alice/renew", `{"requestId":"order-1","periods":2}`), 409, "idempotency_conflict")
	pcCode(t, e.api("POST", "users/alice/change", `{"plan":"轻量","mode":"replace","requestId":"order-1"}`), 409, "idempotency_conflict")
	pcCode(t, e.api("POST", "users/alice/renew", `{"requestId":"含空格 不行"}`), 400, "bad_request_id")
	pcCode(t, e.api("POST", "users/alice/renew", `{"requestId":"`+strings.Repeat("a", 65)+`"}`), 400, "bad_request_id")

	// 预览不占用键:同一个键之后照样能正式执行
	if w := e.api("POST", "users/alice/renew", `{"requestId":"order-3","dryRun":true}`); w.Code != 200 || !pcResult(t, w).DryRun {
		t.Fatalf("预览: %d %s", w.Code, w.Body.String())
	}
	n := e.count(&model.PlanOp{})
	if w := e.api("POST", "users/alice/renew", `{"requestId":"order-3"}`); w.Code != 200 || w.Header().Get("Idempotent-Replayed") != "" || e.count(&model.PlanOp{}) != n+1 {
		t.Fatalf("预览过的键要能正式执行: %d %v", w.Code, w.Header())
	}

	// 降级立即超量:409 needs_force 带 warnings 与 preview,不占用键;带 force 重试才执行,再重试是重放
	e.db.Model(&model.User{}).Where("name = ?", "alice").Update("up", int64(20<<30))
	w = e.api("POST", "users/alice/change", `{"plan":"轻量","mode":"replace","usage":"keep","requestId":"order-4"}`)
	pcCode(t, w, 409, "needs_force")
	var nf struct {
		Warnings []planWarning `json:"warnings"`
		Preview  planResult    `json:"preview"`
	}
	json.Unmarshal(w.Body.Bytes(), &nf)
	if len(nf.Warnings) == 0 || !nf.Preview.NeedsForce || nf.Preview.To.Volume != 10<<30 || e.user("alice").PlanId != monthly.Id {
		t.Fatalf("409 要带告警与预览,而且没有落库: %s", w.Body.String())
	}
	for i, want := range []string{"", "true"} {
		w = e.api("POST", "users/alice/change", `{"plan":"轻量","mode":"replace","usage":"keep","requestId":"order-4","force":true}`)
		if w.Code != 200 || w.Header().Get("Idempotent-Replayed") != want {
			t.Fatalf("force 第 %d 次: %d %s", i+1, w.Code, w.Body.String())
		}
		if r := pcResult(t, w); r.Replayed != (want == "true") || r.User == nil || r.To.PlanId != small.Id {
			t.Fatalf("force 第 %d 次的结果: %s", i+1, w.Body.String())
		}
	}
	if u := e.user("alice"); u.PlanId != small.Id || u.Up != 20<<30 {
		t.Fatalf("force 后执行,用量保留: %+v", u)
	}

	// 不同作用域(主面板与代理)同一个键各自成功
	a := model.Reseller{Name: "a", Enabled: true, ApiEnabled: true, ApiToken: "tok-a-0123456789"}
	e.db.Create(&a)
	e.s.setResellerLines(a.Id, []uint{1})
	ap := e.mkPlan(model.Plan{Name: "a-月付", ResellerId: a.Id, VolumeGB: 10, Days: 30})
	e.mkUser(model.User{Name: "a-u", ResellerId: a.Id, PlanId: ap.Id, SubToken: "uuuuuuuuuuuuuuuuuuuuuuuu"}, 1)
	if w := e.rapi("tok-a-0123456789", "POST", "users/a-u/renew", `{"requestId":"order-1"}`); w.Code != 200 || w.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("代理作用域里同一个键是另一笔: %d %s", w.Code, w.Body.String())
	}
	if n := e.count(&model.PlanOp{}, "request_id = ?", "order-1"); n != 2 {
		t.Fatalf("两个作用域各一条: %d", n)
	}
}

// 并发:8 个请求同时用同一个键,只有一个真正执行,其余是重放。
func TestPlanOpIdempotencyConcurrent(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	p := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30})
	e.mkUser(model.User{Name: "bob", PlanId: p.Id, Expiry: now + 86400})
	exp0 := e.user("bob").Expiry
	var wg sync.WaitGroup
	var replays, fails int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := e.api("POST", "users/bob/renew", `{"plan":"月付"}`, "Idempotency-Key", "same-key")
			switch {
			case w.Code != 200:
				atomic.AddInt32(&fails, 1)
			case w.Header().Get("Idempotent-Replayed") == "true":
				atomic.AddInt32(&replays, 1)
			}
		}()
	}
	wg.Wait()
	if fails != 0 || replays != 7 || e.count(&model.PlanOp{}) != 1 {
		t.Fatalf("应 1 次执行 7 次重放: 失败 %d 重放 %d 记录 %d", fails, replays, e.count(&model.PlanOp{}))
	}
	if got := e.user("bob").Expiry; got != exp0+30*86400 {
		t.Fatalf("只加一次 30 天: %+d 天", (got-exp0)/86400)
	}
}

// 事务里重新读用户:请求开始时读到的用量已经过时(统计任务又记了流量),结转按库里最新的算。
func TestPlanOpRereadsInTx(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	small := e.mkPlan(model.Plan{Name: "小包", VolumeGB: 100, Days: 30})
	e.mkPlan(model.Plan{Name: "大包", VolumeGB: 200, Days: 30})
	e.mkUser(model.User{Name: "carol", PlanId: small.Id, Volume: 100 << 30, Expiry: now + 5*86400})
	stale := e.user("carol")
	e.db.Model(&model.User{}).Where("id = ?", stale.Id).Updates(map[string]interface{}{"up": int64(40 << 30), "down": int64(10 << 30)})
	name := "大包"
	r := httptest.NewRequest("POST", "/", nil)
	res, err := e.s.changeUserPlan(r, planScope{actor: "t"}, stale, planOpReq{Plan: &name, Mode: "stack", Usage: "reset"})
	if err != nil {
		t.Fatal(err)
	}
	u := e.user("carol")
	if res.From.Used != 50<<30 || u.Volume != 250<<30 || res.To.Volume != u.Volume || u.Up+u.Down != 0 || u.TotalUp+u.TotalDown != 50<<30 {
		t.Fatalf("应按库里的 50 GB 结转:max(0, 100−50) + 200 = 250 GB,实际 %d GB(预览 %d GB)", u.Volume>>30, res.To.Volume>>30)
	}
}

// 代理令牌:只能换成自己的套餐、只能动自己名下的用户;授权、额度照常生效;记录与预览里的代理信息。
func TestPlanOpResellerScope(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	a := model.Reseller{Name: "a", Enabled: true, ApiEnabled: true, ApiToken: "tok-a-0123456789", Volume: 10 << 30}
	b := model.Reseller{Name: "b", Enabled: true, ApiEnabled: true, ApiToken: "tok-b-0123456789"}
	e.db.Create(&a)
	e.db.Create(&b)
	e.s.setResellerLines(a.Id, []uint{1})
	e.s.setResellerLines(b.Id, []uint{2})
	e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30}) // 主面板的
	am := e.mkPlan(model.Plan{Name: "a-月付", ResellerId: a.Id, VolumeGB: 5, Days: 30, AutoReset: true, ResetDays: 30, LineIds: []byte(`[1]`)})
	big := e.mkPlan(model.Plan{Name: "a-大", ResellerId: a.Id, VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30})
	e.mkPlan(model.Plan{Name: "a-越权", ResellerId: a.Id, VolumeGB: 5, Days: 30, LineIds: []byte(`[2]`)}) // 直接写库:模拟授权后来被收回
	bp := e.mkPlan(model.Plan{Name: "b-月付", ResellerId: b.Id, VolumeGB: 5, Days: 30})
	au := e.mkUser(model.User{Name: "a-u", ResellerId: a.Id, PlanId: am.Id, Volume: 5 << 30, Up: 1 << 30, TotalUp: 2 << 30, Expiry: now + 10*86400,
		AutoReset: true, ResetDays: 30, NextReset: now + 10*86400, SubToken: "aaaaaaaaaaaaaaaaaaaaaaaa"}, 1)
	bu := e.mkUser(model.User{Name: "b-u", ResellerId: b.Id, PlanId: bp.Id, SubToken: "bbbbbbbbbbbbbbbbbbbbbbbb"}, 2)
	const tok = "tok-a-0123456789"

	pcCode(t, e.rapi(tok, "POST", "users/a-u/change", `{"plan":"月付","mode":"replace"}`), 400, "plan_not_found")
	pcCode(t, e.rapi(tok, "POST", "users/a-u/change", `{"planId":`+strconv.Itoa(int(bp.Id))+`,"mode":"replace"}`), 400, "plan_not_found")
	if w := e.rapi(tok, "POST", "users/b-u/change", `{"plan":"a-大","mode":"replace"}`); w.Code != 404 {
		t.Fatalf("别家的用户 404: %d", w.Code)
	}
	if w := e.rapi(tok, "POST", "users/"+strconv.Itoa(int(bu.Id))+"/renew", `{}`); w.Code != 404 {
		t.Fatalf("按 id 也摸不到别家的用户: %d", w.Code)
	}
	w := e.rapi(tok, "POST", "users/a-u/change", `{"plan":"a-越权","mode":"replace"}`)
	pcCode(t, w, 400, "reseller_denied")
	if !strings.Contains(w.Body.String(), "未授权") {
		t.Fatalf("线路超出授权: %s", w.Body.String())
	}

	// 预览:带代理池,用户换完剩的配额(不限期的每期 100 GB)多于代理剩的 7 GB
	w = e.rapi(tok, "POST", "users/a-u/change", `{"plan":"a-大","mode":"stack","dryRun":true}`)
	r := pcResult(t, w)
	if w.Code != 200 || r.Pool == nil || r.Pool.Volume != 10<<30 || r.Pool.Used != 3<<30 || r.Pool.Remaining != 7<<30 || !strings.Contains(w.Body.String(), "reseller_pool_low") {
		t.Fatalf("代理用户的预览要带 pool 与提示: %s", w.Body.String())
	}
	if w := e.rapi(tok, "POST", "users/a-u/change", `{"plan":"a-大","mode":"stack"}`); w.Code != 200 {
		t.Fatalf("自己的套餐应通过: %d %s", w.Code, w.Body.String())
	}
	var op model.PlanOp
	e.db.Last(&op)
	if op.ResellerId != a.Id || op.Actor != "api:a" || op.ToPlanId != big.Id {
		t.Fatalf("代理的操作记在代理作用域: %+v", op)
	}

	// 主令牌改代理用户:按该代理的套餐找(降级到期提前,要 force);主面板的套餐名找不到
	if w := e.api("POST", "users/a-u/change", `{"plan":"a-月付","mode":"replace","force":true}`); w.Code != 200 {
		t.Fatalf("主令牌按用户归属找套餐: %d %s", w.Code, w.Body.String())
	}
	pcCode(t, e.api("POST", "users/a-u/change", `{"plan":"月付","mode":"replace"}`), 400, "plan_not_found")

	// 套餐记录:代理看到主面板的操作人一律是「主面板」,主面板看得到原样
	var mine []model.PlanOp
	w = e.rapi(tok, "GET", "users/a-u/plan-ops", "")
	json.Unmarshal(w.Body.Bytes(), &mine)
	if w.Code != 200 || len(mine) != 2 || mine[0].Actor != planOpMasterTag || mine[1].Actor != "api:a" {
		t.Fatalf("代理视角的套餐记录: %s", w.Body.String())
	}
	if w := e.api("GET", "users/a-u/plan-ops?limit=1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"actor":"api"`) || strings.Count(w.Body.String(), `"id":`) != 1 {
		t.Fatalf("主面板视角 + limit: %s", w.Body.String())
	}
	if w := e.rapi(tok, "POST", "users/a-u/plan-ops", ""); w.Code != 405 {
		t.Fatalf("plan-ops 只读: %d", w.Code)
	}

	// 代理流量用尽:换套餐与续费都拒
	e.db.Model(&model.User{}).Where("id = ?", au.Id).Update("total_down", int64(20<<30))
	pcCode(t, e.rapi(tok, "POST", "users/a-u/change", `{"plan":"a-大","mode":"stack","dryRun":true}`), 400, "reseller_denied")
	pcCode(t, e.rapi(tok, "POST", "users/a-u/renew", `{}`), 400, "reseller_denied")
	e.db.Model(&model.User{}).Where("id = ?", au.Id).Update("total_down", 0)

	// 代理到期:令牌直接 401;代理面板会话里的换套餐被拒
	e.db.Model(&model.Reseller{}).Where("id = ?", a.Id).Update("expiry", now-60)
	if w := e.rapi(tok, "POST", "users/a-u/renew", `{}`); w.Code != 401 {
		t.Fatalf("到期代理的令牌失效: %d", w.Code)
	}
	w = e.panel(a.Id, "POST", "users/"+strconv.Itoa(int(au.Id))+"/change", `{"plan":"a-大","mode":"stack"}`)
	pcCode(t, w, 400, "reseller_denied")
	if !strings.Contains(w.Body.String(), "到期") {
		t.Fatalf("代理到期: %s", w.Body.String())
	}
}

// 副机上的写操作一律 403;套餐记录只读照常。
func TestPlanOpOnNode(t *testing.T) {
	e := newPCEnv(t)
	p := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30})
	e.mkUser(model.User{Name: "alice", PlanId: p.Id})
	e.db.Create(&model.Setting{Key: "nodeMode", Value: "true"})
	for _, path := range []string{"users/alice/change", "users/alice/renew", "users/alice/plan"} {
		if w := e.api("POST", path, `{"plan":"月付","mode":"replace","dryRun":true}`); w.Code != 403 {
			t.Fatalf("副机上 %s 应 403: %d", path, w.Code)
		}
	}
	if w := e.api("GET", "users/alice/plan-ops", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("副机上套餐记录只读: %d %s", w.Code, w.Body.String())
	}
}

// /renew 与 /plan:不给套餐续当前套餐、期数校验、未知 mode 按 renew、换了套餐立刻重判规则、预览返回结果对象。
func TestPlanOpRenewEndpoint(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	monthly := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30})
	e.mkPlan(model.Plan{Name: "年付", VolumeGB: 100, Days: 365})
	e.mkUser(model.User{Name: "alice", PlanId: monthly.Id, Volume: 10 << 30, Up: 1 << 30, Expiry: now + 86400})
	e.mkUser(model.User{Name: "nobody"})
	exp0 := e.user("alice").Expiry

	if w := e.api("POST", "users/alice/renew", ""); w.Code != 200 { // 空请求体 = 续当前套餐
		t.Fatalf("空请求体续当前套餐: %d %s", w.Code, w.Body.String())
	}
	if u := e.user("alice"); u.Expiry != exp0+30*86400 || u.Up != 0 {
		t.Fatalf("续了一期月付: %+v", u)
	}
	pcCode(t, e.api("POST", "users/nobody/renew", `{}`), 400, "plan_required")
	pcCode(t, e.api("POST", "users/alice/renew", `{"periods":0}`), 400, "bad_periods")
	pcCode(t, e.api("POST", "users/alice/renew", `{"periods":121}`), 400, "bad_periods")
	pcCode(t, e.api("POST", "users/alice/plan", `{"plan":"年付","periods":101}`), 400, "bad_periods")
	pcCode(t, e.api("POST", "users/alice/plan", `{"plan":"季付"}`), 400, "plan_not_found")
	pcCode(t, e.api("POST", "users/alice/change", `{"plan":"年付"}`), 400, "bad_mode")
	pcCode(t, e.api("POST", "users/alice/change", `{"plan":"年付","mode":"renew"}`), 400, "bad_mode")
	pcCode(t, e.api("POST", "users/alice/change", `{"mode":"replace"}`), 400, "plan_required")
	pcCode(t, e.api("POST", "users/alice/change", `{"plan":"年付","mode":"stack","usage":"clear"}`), 400, "bad_usage")
	pcCode(t, e.api("POST", "users/alice/change", `{"plan":"年付","mode":"stack","fromPlanId":999}`), 409, "plan_changed")
	if w := e.api("POST", "users/alice/change", `not json`); w.Code != 400 {
		t.Fatalf("坏 JSON: %d", w.Code)
	}

	e.db.Model(&model.User{}).Where("name = ?", "alice").Update("up", int64(3<<30))
	if w := e.api("POST", "users/alice/plan", `{"plan":"月付","mode":"weird"}`); w.Code != 200 || e.user("alice").Up != 0 {
		t.Fatalf("未知 mode 仍按 renew(清零): %d", w.Code)
	}
	if e.rules != 0 {
		t.Fatalf("续同一个套餐不用重判规则: %d", e.rules)
	}
	w := e.api("POST", "users/alice/plan", `{"plan":"年付","dryRun":true}`)
	if r := pcResult(t, w); w.Code != 200 || r.Op != "renew" || !r.DryRun || r.To.Plan != "年付" || e.user("alice").PlanId != monthly.Id {
		t.Fatalf("/plan 预览返回结果对象且不落库: %s", w.Body.String())
	}
	exp1 := e.user("alice").Expiry
	w = e.api("POST", "users/alice/plan", `{"plan":"年付","periods":2,"mode":"extend"}`)
	var view apiUserView
	json.Unmarshal(w.Body.Bytes(), &view)
	if w.Code != 200 || view.Name != "alice" || view.Expiry != exp1+730*86400 || e.rules != 1 {
		t.Fatalf("/plan 正式执行仍返回用户对象,换了套餐重判一次规则: %d %s rules=%d", w.Code, w.Body.String(), e.rules)
	}
}

// 面板内部路由:换套餐、套餐记录、续费 N 期与预览、批量续 N 期;代理会话碰不到别人的用户。
func TestPlanOpPanelRoutes(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	monthly := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30})
	yearly := e.mkPlan(model.Plan{Name: "年付", VolumeGB: 100, Days: 365})
	alice := e.mkUser(model.User{Name: "alice", PlanId: monthly.Id, Volume: 10 << 30, Expiry: now + 86400})
	bob := e.mkUser(model.User{Name: "bob", PlanId: monthly.Id, Volume: 10 << 30, Expiry: now + 86400})
	a := model.Reseller{Name: "a", Enabled: true}
	e.db.Create(&a)
	e.s.setResellerLines(a.Id, []uint{1})
	ap := e.mkPlan(model.Plan{Name: "a-月付", ResellerId: a.Id, VolumeGB: 5, Days: 30})
	au := e.mkUser(model.User{Name: "a-u", ResellerId: a.Id, PlanId: ap.Id, SubToken: "aaaaaaaaaaaaaaaaaaaaaaaa"}, 1)
	id := func(u model.User) string { return strconv.Itoa(int(u.Id)) }

	w := e.panel(0, "POST", "users/"+id(alice)+"/change", `{"planId":`+strconv.Itoa(int(yearly.Id))+`,"mode":"replace"}`)
	if r := pcResult(t, w); w.Code != 200 || r.OpId == 0 || r.User != nil || e.user("alice").PlanId != yearly.Id {
		t.Fatalf("面板换套餐: %d %s", w.Code, w.Body.String())
	}
	if w := e.panel(0, "GET", "users/"+id(alice)+"/plan-ops", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"op":"replace"`) {
		t.Fatalf("面板套餐记录: %s", w.Body.String())
	}
	if w := e.panel(0, "GET", "users/"+id(alice)+"/change", ""); w.Code != 405 {
		t.Fatalf("change 只收 POST: %d", w.Code)
	}
	// 代理会话:别人的用户 403,自己的照常
	if w := e.panel(a.Id, "POST", "users/"+id(alice)+"/change", `{"planId":`+strconv.Itoa(int(ap.Id))+`,"mode":"replace"}`); w.Code != 403 {
		t.Fatalf("代理会话改别人的用户应 403: %d", w.Code)
	}
	if w := e.panel(a.Id, "GET", "users/"+id(alice)+"/plan-ops", ""); w.Code != 403 {
		t.Fatalf("代理会话看别人的套餐记录应 403: %d", w.Code)
	}
	if w := e.panel(a.Id, "POST", "users/"+id(au)+"/change", `{"planId":`+strconv.Itoa(int(monthly.Id))+`,"mode":"replace"}`); w.Code != 400 {
		t.Fatalf("代理会话换成主面板的套餐应 400: %d", w.Code)
	}
	if w := e.panel(a.Id, "POST", "users/"+id(au)+"/plan", `{"planId":`+strconv.Itoa(int(monthly.Id))+`}`); w.Code != 400 {
		t.Fatalf("代理会话续主面板的套餐应 400: %d", w.Code)
	}
	if w := e.panel(a.Id, "POST", "users/"+id(au)+"/plan", `{"periods":2}`); w.Code != 200 {
		t.Fatalf("代理会话续自己的当前套餐: %d %s", w.Code, w.Body.String())
	}

	// 续费:预览返回结果对象;同一个 requestId 第二次是重放
	w = e.panel(0, "POST", "users/"+id(bob)+"/plan", `{"planId":`+strconv.Itoa(int(monthly.Id))+`,"periods":3,"dryRun":true}`)
	if r := pcResult(t, w); w.Code != 200 || !r.DryRun || r.To.Expiry != bob.Expiry+90*86400 {
		t.Fatalf("续费预览: %s", w.Body.String())
	}
	for i, replayed := range []bool{false, true} {
		w = e.panel(0, "POST", "users/"+id(bob)+"/plan", `{"planId":`+strconv.Itoa(int(monthly.Id))+`,"periods":3,"requestId":"ui-1"}`)
		var out map[string]interface{}
		json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != 200 || out["ok"] != "1" || (out["replayed"] == true) != replayed {
			t.Fatalf("第 %d 次续费: %s", i+1, w.Body.String())
		}
	}
	if got := e.user("bob").Expiry; got != bob.Expiry+90*86400 {
		t.Fatalf("续 3 期只执行一次: %+d 天", (got-bob.Expiry)/86400)
	}

	// 批量续 N 期:每人一条套餐记录,审计照旧整批一条
	ops, changes := e.count(&model.PlanOp{}), e.count(&model.Change{})
	w = e.panel(0, "POST", "users/batch", `{"ids":[`+id(alice)+`,`+id(bob)+`],"action":"plan","planId":`+strconv.Itoa(int(monthly.Id))+`,"periods":2,"mode":"extend"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"affected":2`) {
		t.Fatalf("批量续费: %d %s", w.Code, w.Body.String())
	}
	if e.count(&model.PlanOp{}) != ops+2 || e.count(&model.Change{}) != changes+1 {
		t.Fatalf("批量:记录 +2、审计 +1,实际 %d %d", e.count(&model.PlanOp{})-ops, e.count(&model.Change{})-changes)
	}
	if got := e.user("bob").Expiry; got != bob.Expiry+150*86400 {
		t.Fatalf("批量 2 期: %+d 天", (got-bob.Expiry)/86400)
	}
	if w := e.panel(0, "POST", "users/batch", `{"ids":[`+id(alice)+`],"action":"plan","planId":`+strconv.Itoa(int(monthly.Id))+`,"periods":0}`); w.Code != 400 {
		t.Fatalf("批量期数为 0 应拒: %d", w.Code)
	}
}

// 套餐的参考价:能存能改,负数拒绝,面板与外部 API 的套餐列表都带上。
func TestPlanPrice(t *testing.T) {
	e := newPCEnv(t)
	call := func(h http.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://panel.example:2053/app/api/"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		h(w, r)
		return w
	}
	w := call(e.s.handlePlans, "POST", "plans", `{"name":"月付","volumeGb":100,"days":30,"price":12.5}`)
	if w.Code != 200 {
		t.Fatalf("新建: %d %s", w.Code, w.Body.String())
	}
	var p model.Plan
	e.db.Where("name = ?", "月付").First(&p)
	if p.Price != 12.5 {
		t.Fatalf("参考价没存上: %v", p.Price)
	}
	if w := call(e.s.handlePlanItem, "PUT", "plans/"+strconv.Itoa(int(p.Id)), `{"name":"月付","volumeGb":100,"days":30,"price":20}`); w.Code != 200 {
		t.Fatalf("修改: %d %s", w.Code, w.Body.String())
	}
	e.db.First(&p, p.Id)
	if p.Price != 20 {
		t.Fatalf("PUT 要写参考价: %v", p.Price)
	}
	if w := call(e.s.handlePlanItem, "PUT", "plans/"+strconv.Itoa(int(p.Id)), `{"name":"月付","price":-1}`); w.Code != 400 {
		t.Fatalf("负数应拒: %d", w.Code)
	}
	if w := call(e.s.handlePlans, "GET", "plans", ""); !strings.Contains(w.Body.String(), `"price":20`) {
		t.Fatalf("面板套餐列表带参考价: %s", w.Body.String())
	}
	if w := e.api("GET", "plans", ""); !strings.Contains(w.Body.String(), `"price":20`) {
		t.Fatalf("外部 API 套餐列表带参考价: %s", w.Body.String())
	}
}
