package web

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 外部 API 给商城、用户机器人、商城机器人补的几样(0.8.0):
//   - POST /users 可以不给名字(自动起名)、认幂等键(重试不重复建号);
//   - POST /users/batch 一次建一批;
//   - POST /users/lookup 按订阅地址找用户(机器人绑定账号);
//   - GET /users 的状态筛选、分页与总数。

func usersOf(t *testing.T, w *httptest.ResponseRecorder) []apiUserView {
	t.Helper()
	var out []apiUserView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("用户列表: %d %s", w.Code, w.Body.String())
	}
	return out
}

func userOf(t *testing.T, w *httptest.ResponseRecorder) apiUserView {
	t.Helper()
	var out apiUserView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Id == 0 {
		t.Fatalf("用户对象: %d %s", w.Code, w.Body.String())
	}
	return out
}

func TestAPICreateAutoNameAndIdempotent(t *testing.T) {
	e := newPCEnv(t)
	// 不给名字:自动起成「前缀-8 位」,默认前缀 u
	a := userOf(t, e.api("POST", "users", `{"volumeGb":10,"days":30}`))
	if !regexp.MustCompile(`^u-[a-z2-9]{8}$`).MatchString(a.Name) || a.Volume != 10<<30 || a.SubLink == "" {
		t.Fatalf("自动起名的用户不对: %+v", a)
	}
	b := userOf(t, e.api("POST", "users", `{"prefix":"shop"}`))
	if !regexp.MustCompile(`^shop-[a-z2-9]{8}$`).MatchString(b.Name) || b.Name == a.Name {
		t.Fatalf("带前缀的自动起名不对: %s", b.Name)
	}
	if w := e.api("POST", "users", `{"prefix":"bad prefix"}`); w.Code != 400 {
		t.Fatalf("不合适的前缀应当 400,实际 %d", w.Code)
	}
	// 给了名字照旧;重名照旧 400
	if c := userOf(t, e.api("POST", "users", `{"name":"carol"}`)); c.Name != "carol" {
		t.Fatalf("给了名字应当用它: %s", c.Name)
	}
	if w := e.api("POST", "users", `{"name":"carol"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "用户名已存在") {
		t.Fatalf("重名应当 400 用户名已存在: %d %s", w.Code, w.Body.String())
	}

	// 幂等:同一个键、同样的内容再发一次,不再建号,返回当时那个用户
	before := e.count(&model.User{})
	first := e.api("POST", "users", `{"prefix":"order","volumeGb":5,"requestId":"order-1001"}`)
	u1 := userOf(t, first)
	if first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatal("第一次不该标重放")
	}
	again := e.api("POST", "users", `{"prefix":"order","volumeGb":5,"requestId":"order-1001"}`)
	u2 := userOf(t, again)
	if u2.Id != u1.Id || u2.Name != u1.Name || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("重试应当返回同一个用户并标重放: %+v / %+v", u1, u2)
	}
	if e.count(&model.User{}) != before+1 {
		t.Fatalf("重试不该多建号:%d → %d", before, e.count(&model.User{}))
	}
	// 幂等键写在请求头里一样
	h1 := userOf(t, e.api("POST", "users", `{"prefix":"hdr"}`, "Idempotency-Key", "order-1002"))
	h2 := e.api("POST", "users", `{"prefix":"hdr"}`, "Idempotency-Key", "order-1002")
	if userOf(t, h2).Id != h1.Id || h2.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("请求头里的幂等键也要认")
	}
	// 同一个键、内容不同:409
	if w := e.api("POST", "users", `{"prefix":"order","volumeGb":50,"requestId":"order-1001"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "idempotency_conflict") {
		t.Fatalf("同键不同内容应当 409 idempotency_conflict: %d %s", w.Code, w.Body.String())
	}
	// 键的格式不对:400
	if w := e.api("POST", "users", `{"name":"x1","requestId":"有 空格"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "bad_request_id") {
		t.Fatalf("幂等键格式不对应当 400 bad_request_id: %d %s", w.Code, w.Body.String())
	}
	// 那个用户删了以后再用同一个键:409,说清要换键(不悄悄再建一个,也不返回一个不存在的人)
	if err := e.db.Delete(&model.User{}, u1.Id).Error; err != nil { // 这个测试环境没有数据面,不走接口删(删用户要踢线)
		t.Fatal(err)
	}
	if w := e.api("POST", "users", `{"prefix":"order","volumeGb":5,"requestId":"order-1001"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "idempotency_gone") {
		t.Fatalf("用户删了以后重放应当 409 idempotency_gone: %d %s", w.Code, w.Body.String())
	}
	// 建号失败(重名)不占用幂等键:改对了用同一个键能建成
	if w := e.api("POST", "users", `{"name":"carol","requestId":"order-1003"}`); w.Code != 400 {
		t.Fatalf("重名应当 400: %d", w.Code)
	}
	if e.count(&model.ApiCreate{}, "request_id = ?", "order-1003") != 0 {
		t.Fatal("没建成的请求不该留下幂等记录")
	}
	// 代理和主面板的键互不相干(各自的作用域)
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-default-token-0123456789"}
	e.db.Create(&rs)
	e.db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 1})
	ru := userOf(t, e.rapi(rs.ApiToken, "POST", "users", `{"prefix":"r","requestId":"order-1002"}`))
	if ru.Id == h1.Id || !strings.HasPrefix(ru.Name, "r-") {
		t.Fatalf("代理用同一个键建的是它自己的用户: %+v", ru)
	}
}

func TestAPIBatchCreate(t *testing.T) {
	e := newPCEnv(t)
	plan := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 100, Days: 30})
	w := e.api("POST", "users/batch", `{"count":5,"prefix":"card","plan":"月付","remark":"卡密批次 7"}`)
	var out struct {
		Users     []apiUserView `json:"users"`
		Requested int           `json:"requested"`
		Created   int           `json:"created"`
		Error     string        `json:"error"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("批量建号: %d %s", w.Code, w.Body.String())
	}
	if out.Requested != 5 || out.Created != 5 || len(out.Users) != 5 || out.Error != "" {
		t.Fatalf("应当建成 5 个: %+v", out)
	}
	seen := map[string]bool{}
	for _, u := range out.Users {
		if !regexp.MustCompile(`^card-[a-z2-9]{8}$`).MatchString(u.Name) || seen[u.Name] || u.PlanId != plan.Id || u.Volume != 100<<30 || u.Remark != "卡密批次 7" || u.SubLink == "" || !u.Enabled {
			t.Fatalf("批量建的用户不对: %+v", u)
		}
		seen[u.Name] = true
	}
	if e.count(&model.User{}) != 5 {
		t.Fatalf("库里应当有 5 个用户,实际 %d", e.count(&model.User{}))
	}
	for _, c := range []struct{ body, want string }{
		{`{"count":0}`, "count 填 1–100"},
		{`{"count":101}`, "count 填 1–100"},
		{`{"count":2,"name":"x"}`, "不要给 name"},
		{`{"count":2,"requestId":"k1"}`, "不认幂等键"},
		{`{"count":2,"plan":"没有这个套餐"}`, ""},
	} {
		w := e.api("POST", "users/batch", c.body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), c.want) {
			t.Fatalf("%s 应当 400(%s): %d %s", c.body, c.want, w.Code, w.Body.String())
		}
	}
	if e.count(&model.User{}) != 5 {
		t.Fatal("没建成的批次不该留下用户")
	}
	// 代理:用户数上限卡在中间 —— 建到上限为止,已经建的照样返回,并说明为什么停
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-default-token-0123456789", UserLimit: 2}
	e.db.Create(&rs)
	e.db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 1})
	w = e.rapi(rs.ApiToken, "POST", "users/batch", `{"count":4,"prefix":"r"}`)
	out.Users, out.Error = nil, ""
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Created != 2 || out.Requested != 4 || out.Error == "" {
		t.Fatalf("代理上限 2:应当建成 2 个并说明原因: %d %s", w.Code, w.Body.String())
	}
	// 权限:只读的密钥不能批量建;叫 batch / lookup 的用户照样能按名字读到
	ro := e.mkKey(0, "只读", permUsersRead)
	if w := e.keyCall(false, "203.0.113.10:4000", ro.Token, "POST", "users/batch", `{"count":1}`); w.Code != 403 {
		t.Fatalf("只读密钥批量建号应当 403,实际 %d", w.Code)
	}
	e.mkUser(model.User{Name: "batch"}, 1)
	if u := userOf(t, e.api("GET", "users/batch", "")); u.Name != "batch" {
		t.Fatal("叫 batch 的用户应当照样读得到")
	}
}

// 代理的密钥在 /overview 里看得到自己的额度,canCreate 和真去建号的结果一致;主面板的密钥没有这一段。
func TestAPIOverviewResellerQuota(t *testing.T) {
	e := newPCEnv(t)
	rs := model.Reseller{Name: "shop", Enabled: true, ApiEnabled: true, ApiToken: "rs-default-token-0123456789", UserLimit: 2, Volume: 10 << 30, DeviceLimit: 5}
	e.db.Create(&rs)
	e.db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 1})
	type quota struct {
		Name       string `json:"name"`
		Enabled    bool   `json:"enabled"`
		Expiry     int64  `json:"expiry"`
		UserLimit  int    `json:"userLimit"`
		Users      int64  `json:"users"`
		UsersLeft  *int64 `json:"usersLeft"`
		Volume     int64  `json:"volume"`
		Used       int64  `json:"used"`
		VolumeLeft *int64 `json:"volumeLeft"`
		DeviceCap  int    `json:"deviceLimit"`
		CanCreate  bool   `json:"canCreate"`
	}
	get := func() *quota {
		t.Helper()
		w := e.rapi(rs.ApiToken, "GET", "overview", "")
		var out struct {
			Reseller *quota `json:"reseller"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Reseller == nil {
			t.Fatalf("代理的概况应当带 reseller: %d %s", w.Code, w.Body.String())
		}
		return out.Reseller
	}
	create := func() int { return e.rapi(rs.ApiToken, "POST", "users", `{"prefix":"s"}`).Code }
	q := get()
	if q.Name != "shop" || !q.Enabled || q.UserLimit != 2 || q.Users != 0 || q.UsersLeft == nil || *q.UsersLeft != 2 ||
		q.Volume != 10<<30 || q.Used != 0 || q.VolumeLeft == nil || *q.VolumeLeft != 10<<30 || q.DeviceCap != 5 || !q.CanCreate {
		t.Fatalf("初始额度不对: %+v", q)
	}
	if create() != 200 || create() != 200 {
		t.Fatal("上限 2:前两个应当建得成")
	}
	if q = get(); q.Users != 2 || *q.UsersLeft != 0 || q.CanCreate {
		t.Fatalf("建满之后应当 usersLeft 0、canCreate false: %+v", q)
	}
	if create() == 200 {
		t.Fatal("canCreate false 时真去建也应当建不成")
	}
	// 用户数放开、流量用尽:名下用户用掉 10 GB
	e.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("user_limit", 0)
	var one model.User
	e.db.Where("reseller_id = ?", rs.Id).First(&one)
	e.db.Model(&model.User{}).Where("id = ?", one.Id).Update("down", int64(10<<30))
	if q = get(); q.UsersLeft != nil || q.Used < 10<<30 || *q.VolumeLeft != 0 || q.CanCreate {
		t.Fatalf("流量用尽应当 volumeLeft 0、canCreate false,且不限用户数时没有 usersLeft: %+v", q)
	}
	if create() == 200 {
		t.Fatal("流量用尽时真去建也应当建不成")
	}
	// 流量放开、已到期:到期的代理密钥整个不认(401),读不到概况也建不了号
	e.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Updates(map[string]interface{}{"volume": 0, "expiry": time.Now().Add(-time.Hour).Unix()})
	if w := e.rapi(rs.ApiToken, "GET", "overview", ""); w.Code != 401 {
		t.Fatalf("到期的代理密钥应当 401,实际 %d", w.Code)
	}
	if create() == 200 {
		t.Fatal("到期时真去建也应当建不成")
	}
	// 还没到期:带着到期时间,能建;不限流量时没有 volumeLeft
	soon := time.Now().Add(48 * time.Hour).Unix()
	e.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("expiry", soon)
	if q = get(); !q.CanCreate || q.Expiry != soon || q.VolumeLeft != nil {
		t.Fatalf("没到期、都放开应当能建: %+v", q)
	}
	if create() != 200 {
		t.Fatal("canCreate true 时应当建得成")
	}
	// 主面板的密钥没有这一段
	w := e.api("GET", "overview", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"reseller"`) {
		t.Fatalf("主面板的概况不该带 reseller: %d %s", w.Code, w.Body.String())
	}
}

func TestAPILookupBySubscription(t *testing.T) {
	e := newPCEnv(t)
	alice := e.mkUser(model.User{Name: "alice"}, 1)
	tok := e.mkUser(model.User{Name: "bob", SubToken: "TOKEN1234567890abcdefg"}, 1)
	e.db.Model(&model.User{}).Where("id = ?", alice.Id).Update("share_token", "shr-alice-abcdefghijklmnop")
	find := func(sub string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"sub": sub})
		return e.api("POST", "users/lookup", string(b))
	}
	for _, sub := range []string{
		"alice", "https://sub.example:2056/sub/alice", "https://sub.example:2056/sub/alice?format=clash", "http://1.2.3.4:2056/sub/alice/",
		"https://sub.example:2056/sub/alice/qr?format=json", " https://sub.example:2056/sub/alice#x ", "/sub/alice",
	} {
		if u := userOf(t, find(sub)); u.Name != "alice" {
			t.Fatalf("%q 应当找到 alice,实际 %s", sub, u.Name)
		}
	}
	for _, sub := range []string{"TOKEN1234567890abcdefg", "https://sub.example:2056/sub/TOKEN1234567890abcdefg?format=json"} {
		if u := userOf(t, find(sub)); u.Name != "bob" {
			t.Fatalf("%q 应当按订阅令牌找到 bob,实际 %s", sub, u.Name)
		}
	}
	// 有订阅令牌的用户:用户名不是他的订阅地址,不能按名字绑
	if w := find("https://sub.example:2056/sub/bob"); w.Code != 404 {
		t.Fatalf("有订阅令牌的用户按名字找应当 404,实际 %d", w.Code)
	}
	// 临时共享的地址不认(拿着别人分享的地址不能把账号绑成自己的)
	if w := find("https://sub.example:2056/sub/shr-alice-abcdefghijklmnop"); w.Code != 404 {
		t.Fatalf("共享地址应当 404,实际 %d", w.Code)
	}
	for _, sub := range []string{"", "   ", "https://sub.example:2056", "nobody"} {
		w := find(sub)
		want := 404
		if strings.TrimSpace(sub) == "" || sub == "https://sub.example:2056" {
			want = 400
		}
		if w.Code != want {
			t.Fatalf("%q 应当 %d,实际 %d %s", sub, want, w.Code, w.Body.String())
		}
	}
	_ = tok
	// 代理的密钥只找得到自己名下的
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-default-token-0123456789"}
	e.db.Create(&rs)
	mine := e.mkUser(model.User{Name: "r-user", ResellerId: rs.Id, SubToken: "RSTOKEN0123456789abcdef"}, 1)
	b, _ := json.Marshal(map[string]string{"sub": "https://rs.example:2056/sub/" + mine.SubToken})
	if u := userOf(t, e.rapi(rs.ApiToken, "POST", "users/lookup", string(b))); u.Name != "r-user" {
		t.Fatal("代理应当找得到自己名下的用户")
	}
	b, _ = json.Marshal(map[string]string{"sub": "https://sub.example:2056/sub/alice"})
	if w := e.rapi(rs.ApiToken, "POST", "users/lookup", string(b)); w.Code != 404 {
		t.Fatalf("代理找主面板的用户应当 404,实际 %d", w.Code)
	}
	// 权限:要读用户的权限;只有建号权限的密钥不行
	mk := e.mkKey(0, "只建", permUsersCreate)
	if w := e.keyCall(false, "203.0.113.10:4000", mk.Token, "POST", "users/lookup", `{"sub":"alice"}`); w.Code != 403 {
		t.Fatalf("没有读权限应当 403,实际 %d", w.Code)
	}
	ro := e.mkKey(0, "只读", permUsersRead)
	if w := e.keyCall(false, "203.0.113.10:4000", ro.Token, "POST", "users/lookup", `{"sub":"alice"}`); w.Code != 200 {
		t.Fatalf("只读密钥应当能找: %d %s", w.Code, w.Body.String())
	}
	// 订阅地址只在请求体里:GET 带在网址上的不认(那是在读一个叫 lookup 的用户)
	if w := e.api("GET", "users/lookup?sub=alice", ""); w.Code != 404 {
		t.Fatalf("GET users/lookup 应当是 404(没有叫 lookup 的用户),实际 %d", w.Code)
	}
}

func TestAPIListFiltersAndPaging(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	plan := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 100, Days: 30})
	e.mkUser(model.User{Name: "a-active", Expiry: now + 40*86400, Volume: 100, Up: 10}, 1)
	e.mkUser(model.User{Name: "b-soon", Expiry: now + 3*86400, PlanId: plan.Id}, 1)
	e.mkUser(model.User{Name: "c-expired", Expiry: now - 3600}, 1)
	e.mkUser(model.User{Name: "d-used", Volume: 100, Up: 60, Down: 60}, 1)
	off := e.mkUser(model.User{Name: "e-off"}, 1)
	e.db.Model(&model.User{}).Where("id = ?", off.Id).Update("enabled", false)
	e.mkUser(model.User{Name: "f-plain", PlanId: plan.Id}, 1)
	names := func(path string) (string, string) {
		w := e.api("GET", path, "")
		var list []string
		for _, u := range usersOf(t, w) {
			list = append(list, u.Name)
		}
		return strings.Join(list, ","), w.Header().Get("X-Total-Count")
	}
	for _, c := range []struct{ path, want, total string }{
		{"users", "a-active,b-soon,c-expired,d-used,e-off,f-plain", "6"},
		{"users?state=active", "a-active,b-soon,f-plain", "3"},
		{"users?state=expiring", "b-soon", "1"},
		{"users?state=expiring&days=60", "a-active,b-soon", "2"},
		{"users?state=expired", "c-expired", "1"},
		{"users?state=exhausted", "d-used", "1"},
		{"users?state=disabled", "e-off", "1"},
		{"users?planId=" + keyID(plan.Id), "b-soon,f-plain", "2"},
		{"users?planId=0&enabled=true", "a-active,c-expired,d-used", "3"},
		{"users?limit=2", "a-active,b-soon", "6"},
		{"users?limit=2&offset=2", "c-expired,d-used", "6"},
		{"users?limit=2&offset=5", "f-plain", "6"},
		{"users?offset=99", "", "6"},
		{"users?state=active&limit=1&offset=1", "b-soon", "3"},
		{"users?online=true", "", "0"},
		{"users?online=false&q=soon", "b-soon", "1"},
	} {
		got, total := names(c.path)
		if got != c.want || total != c.total {
			t.Fatalf("%s: 得到 [%s] 总数 %s,应为 [%s] 总数 %s", c.path, got, total, c.want, c.total)
		}
	}
	for _, bad := range []string{"users?state=what", "users?limit=0", "users?limit=1001", "users?limit=x", "users?offset=-1", "users?planId=x", "users?state=expiring&days=0"} {
		if w := e.api("GET", bad, ""); w.Code != 400 {
			t.Fatalf("%s 应当 400,实际 %d", bad, w.Code)
		}
	}
	// 分页后的用户对象和详情一样完整(订阅地址在)
	if list := usersOf(t, e.api("GET", "users?limit=1", "")); len(list) != 1 || list[0].SubLink == "" {
		t.Fatal("分页后的用户对象应当完整")
	}
}
