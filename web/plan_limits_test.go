package web

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 数值上限:配额、天数、参考价。SQLite 的整数加法溢出时结果会变成 REAL,存进整数列以后整表读用户都会报错,
// 所以算出来的值一律封顶;请求里给的离谱数值直接拒绝。

const pcVolCap = int64(1) << 62 // 配额上限 2^62 字节(约 4.6 EB)

func colType(t *testing.T, e *pcEnv, col, name string) string {
	t.Helper()
	var typ string
	e.db.Raw("SELECT typeof("+col+") FROM users WHERE name = ?", name).Scan(&typ)
	return typ
}

// 叠加的一次性额度结转在 SQL 里算:配额已经在 int64 顶上时反复叠加(保留 / 清零用量各来几次),
// 结果封在 2^62,列仍是 INTEGER,整表读用户照常。以前 volume + ? 溢出,代理就能让全站读不出用户。
func TestPlanStackVolumeSaturates(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	a := model.Reseller{Name: "a", Enabled: true, ApiEnabled: true, ApiToken: "tok-a-0123456789"}
	e.db.Create(&a)
	e.s.setResellerLines(a.Id, []uint{1})
	huge := e.mkPlan(model.Plan{Name: "巨包", ResellerId: a.Id, VolumeGB: 1 << 32, Days: 30}) // 每期 2^62 字节
	e.mkUser(model.User{Name: "a-u", ResellerId: a.Id, PlanId: huge.Id, Volume: 1 << 30, Up: 1 << 20, Expiry: now + 86400,
		SubToken: "uuuuuuuuuuuuuuuuuuuuuuuu"}, 1)
	const tok = "tok-a-0123456789"
	// 外部 API 只要求配额非负:代理先把配额改到 int64 的顶
	if w := e.rapi(tok, "PATCH", "users/a-u", `{"volume":9223372036854775807}`); w.Code != 200 {
		t.Fatalf("改配额: %d %s", w.Code, w.Body.String())
	}
	for i, usage := range []string{"keep", "reset", "keep", "reset"} {
		w := e.rapi(tok, "POST", "users/a-u/change", `{"plan":"巨包","mode":"stack","periods":120,"usage":"`+usage+`"}`)
		if w.Code != 200 {
			t.Fatalf("第 %d 次叠加(%s): %d %s", i+1, usage, w.Code, w.Body.String())
		}
		if typ := colType(t, e, "volume", "a-u"); typ != "integer" {
			t.Fatalf("第 %d 次叠加后 volume 存成了 %s", i+1, typ)
		}
		r := pcResult(t, w)
		if u := e.user("a-u"); u.Volume != pcVolCap || r.To.Volume != u.Volume {
			t.Fatalf("第 %d 次叠加:配额应封在 2^62,库里 %d,预览 %d", i+1, u.Volume, r.To.Volume)
		}
	}
	var all []model.User
	if err := e.db.Find(&all).Error; err != nil {
		t.Fatalf("整表读用户失败: %v", err)
	}
	if w := e.api("GET", "users", ""); w.Code != 200 {
		t.Fatalf("用户列表: %d %s", w.Code, w.Body.String())
	}
}

// 套餐的 GB 数大到超出 int64:换算按上限饱和,不再随 CPU 变成负数(amd64 上浮点转整数越界是最小负数)。
func TestPlanHugeVolumeGBSaturates(t *testing.T) {
	e := newPCEnv(t)
	e.mkPlan(model.Plan{Name: "离谱", VolumeGB: 1e30, Days: 30})
	e.mkUser(model.User{Name: "alice", Volume: 10 << 30, Expiry: time.Now().Unix() + 86400})
	if w := e.api("POST", "users/alice/plan", `{"plan":"离谱"}`); w.Code != 200 {
		t.Fatalf("续费: %d %s", w.Code, w.Body.String())
	}
	if u := e.user("alice"); u.Volume != pcVolCap {
		t.Fatalf("配额应饱和到 2^62,实际 %d", u.Volume)
	}
	if w := e.api("POST", "users/alice/change", `{"plan":"离谱","mode":"stack","periods":3}`); w.Code != 200 {
		t.Fatalf("叠加: %d %s", w.Code, w.Body.String())
	}
	if u := e.user("alice"); u.Volume != pcVolCap || colType(t, e, "volume", "alice") != "integer" {
		t.Fatalf("叠加后仍封顶: %d", u.Volume)
	}
}

// 请求里给的天数(首次连接后天数、重置周期、days、套餐天数、批量延期)超过上限直接 400,不再落库后在算到期时溢出。
func TestDaysOverLimitRejected(t *testing.T) {
	e := newPCEnv(t)
	e.mkUser(model.User{Name: "alice", Expiry: time.Now().Unix() + 86400})
	huge := strconv.Itoa(1 << 50)
	for _, body := range []string{`{"activateDays":` + huge + `}`, `{"resetDays":` + huge + `,"autoReset":true}`, `{"days":` + huge + `}`} {
		if w := e.api("PATCH", "users/alice", body); w.Code != 400 {
			t.Errorf("PATCH %s 应 400: %d %s", body, w.Code, w.Body.String())
		}
		if w := e.api("POST", "users", strings.Replace(body, "{", `{"name":"bob",`, 1)); w.Code != 400 {
			t.Errorf("POST %s 应 400: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := e.api("PATCH", "users/alice", `{"days":3650000}`); w.Code != 200 {
		t.Fatalf("上限以内照常: %d %s", w.Code, w.Body.String())
	}
	plans := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://panel.example:2053/app/api/"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		if strings.HasPrefix(path, "plans/") {
			e.s.handlePlanItem(w, r)
		} else {
			e.s.handlePlans(w, r)
		}
		return w
	}
	for _, body := range []string{`{"name":"长","days":` + huge + `}`, `{"name":"长","autoReset":true,"resetDays":` + huge + `}`} {
		if w := plans("POST", "plans", body); w.Code != 400 {
			t.Errorf("套餐 %s 应 400: %d %s", body, w.Code, w.Body.String())
		}
	}
	id := strconv.Itoa(int(e.user("alice").Id))
	if w := e.panel(0, "PUT", "users/"+id, `{"name":"alice","enabled":true,"activateDays":`+huge+`}`); w.Code != 400 {
		t.Errorf("面板改用户的首次连接天数应 400: %d %s", w.Code, w.Body.String())
	}
	if w := e.panel(0, "POST", "users/batch", `{"ids":[`+id+`],"action":"extend","days":`+huge+`}`); w.Code != 400 {
		t.Errorf("批量延期应 400: %d %s", w.Code, w.Body.String())
	}
}

// 参考价:有限的非负数,0 = 不填,填了就在 0.01 到 1e9 之间;极端值会让折算结果出现 ±Inf,JSON 编不出来。
func TestPlanPriceBounds(t *testing.T) {
	e := newPCEnv(t)
	post := func(body string) int {
		r := httptest.NewRequest("POST", "http://panel.example:2053/app/api/plans", strings.NewReader(body))
		w := httptest.NewRecorder()
		e.s.handlePlans(w, r)
		return w.Code
	}
	for _, price := range []string{"1e300", "1000000000.5", "0.001", "-1"} {
		if code := post(`{"name":"p` + price + `","days":30,"price":` + price + `}`); code != 400 {
			t.Errorf("参考价 %s 应 400: %d", price, code)
		}
	}
	for _, price := range []string{"0", "0.01", "1000000000", "12.5"} {
		if code := post(`{"name":"p` + price + `","days":30,"price":` + price + `}`); code != 200 {
			t.Errorf("参考价 %s 应通过: %d", price, code)
		}
	}
	// 上下限的两头一起折算:结果里每个数都是有限的,能编成 JSON
	from := model.Plan{Id: 1, Name: "贵", VolumeGB: 1, Days: 1, Price: 1e9}
	to := model.Plan{Id: 2, Name: "便宜", VolumeGB: 1, Price: 0.01} // 不限期的流量包:只折流量
	u := model.User{Id: 1, Name: "u", Enabled: true, PlanId: 1, Volume: math.MaxInt64, Expiry: math.MaxInt64 / 2}
	c, err := calcPlanOp(u, &from, to, planOpOpt{Op: "prorate", Periods: 0, Usage: "reset"}, nil, pcNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(struct {
		C *planCredit
		R planRemaining
	}{c.credit, c.remaining}); err != nil {
		t.Fatalf("折算结果编不成 JSON: %v %+v", err, c.credit)
	}
}

// 生成响应失败(结果里混进了 ±Inf 之类)不能吞掉:以前先写了 200 再编码失败,调用方拿到 200 空响应,
// 正式执行却已经落库了。
func TestWriteJSONEncodeFailure(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, 200, map[string]float64{"x": math.Inf(1)})
	var body map[string]string
	if w.Code != 500 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["error"] == "" {
		t.Fatalf("应返回 500 与错误说明: %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	writeJSON(w, 201, map[string]string{"a": "<b>"})
	if w.Code != 201 || w.Body.String() != "{\"a\":\"\\u003cb\\u003e\"}\n" {
		t.Fatalf("正常输出与以前逐字节相同: %d %q", w.Code, w.Body.String())
	}
}

// 套餐 PUT 的白名单里有 price:按老格式保存(请求里没有 price)要保留原来的参考价,不能抹成 0。
func TestPlanPutKeepsPriceWhenAbsent(t *testing.T) {
	e := newPCEnv(t)
	p := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 100, Days: 30, Price: 20})
	put := func(body string) int {
		r := httptest.NewRequest("PUT", "http://panel.example:2053/app/api/plans/"+strconv.Itoa(int(p.Id)), strings.NewReader(body))
		w := httptest.NewRecorder()
		e.s.handlePlanItem(w, r)
		return w.Code
	}
	if code := put(`{"name":"月付","volumeGb":200,"days":30}`); code != 200 {
		t.Fatalf("老格式保存: %d", code)
	}
	var got model.Plan
	e.db.First(&got, p.Id)
	if got.Price != 20 || got.VolumeGB != 200 {
		t.Fatalf("没带 price 要保留原值: %+v", got)
	}
	if code := put(`{"name":"月付","volumeGb":200,"days":30,"price":0}`); code != 200 {
		t.Fatalf("显式清空: %d", code)
	}
	e.db.First(&got, p.Id)
	if got.Price != 0 {
		t.Fatalf("显式给 0 就清空: %v", got.Price)
	}
}
