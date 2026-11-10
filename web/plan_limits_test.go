package web

import (
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
