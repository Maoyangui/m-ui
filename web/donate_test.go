package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func TestCleanDonorName(t *testing.T) {
	for in, want := range map[string]string{
		"Larry Wang":   "Larry Wang",
		"  张  三 ":      "张 三",
		"O'Brien-Jr.":  "O'Brien-Jr.",
		"佛跳墙·用户_1":     "佛跳墙·用户_1",
		"":             "",
		"   ":          "",
		"http://a.com": "",
		"加我 t.me":      "",
		"example.com":  "",
		"<b>x</b>":     "",
		"a\nb<script>": "",
		"名字太长太长太长太长太长太长太长太长太长太长太长太长太长": "",
	} {
		got, bad := cleanDonorName(in)
		if got != want {
			t.Errorf("%q → %q(%s),应为 %q", in, got, bad, want)
		}
		if want == "" && bad == "" {
			t.Errorf("%q 应该报错", in)
		}
	}
}

func TestPayableAmount(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p := payableAmount(10)
		if !strings.HasPrefix(p, "10.0") || len(p) != 6 {
			t.Fatalf("实际 %s", p)
		}
		seen[p] = true
	}
	if len(seen) < 20 {
		t.Fatalf("零头应该分散开: %d", len(seen))
	}
	if v, ok := usdtMicros("10.037"); !ok || v != 10037000 {
		t.Fatalf("实际 %d %v", v, ok)
	}
	if v, ok := usdtMicros("32.83885"); !ok || v != 32838850 {
		t.Fatalf("实际 %d %v", v, ok)
	}
	for _, bad := range []string{"", "abc", "1.2345678", "-1"} {
		if _, ok := usdtMicros(bad); ok {
			t.Errorf("%q 应该不认", bad)
		}
	}
}

// 外面取来的名单再过一遍:坏名字、负数都不要。
func TestCleanBoard(t *testing.T) {
	b := cleanBoard(donorBoard{Devices: -3, Donors: []donorRow{
		{"Larry Wang", 10}, {"<script>alert(1)</script>", 99}, {"加群 t.me", 99}, {"负数", -1}, {"", 5},
	}})
	if len(b.Donors) != 1 || b.Donors[0].Name != "Larry Wang" || b.Devices != 0 {
		t.Fatalf("实际 %+v", b)
	}
}

func TestDonorBoardFallbackAndCache(t *testing.T) {
	hits := 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		fmt.Fprintf(w, `{"updatedAt":1,"devices":12,"devicesAt":%d,"donors":[{"name":"Larry Wang","total":10}]}`, time.Now().Unix())
	}))
	defer good.Close()
	old := donorsURLs
	donorsURLs = []string{bad.URL, good.URL}
	defer func() { donorsURLs = old }()
	s, _ := newTestServerForSupport(t, "")
	w := httptest.NewRecorder()
	s.handleSupportDonors(w, httptest.NewRequest("GET", "/app/support/donors", nil))
	var b donorBoard
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Devices != 12 || len(b.Donors) != 1 || b.Donors[0].Name != "Larry Wang" {
		t.Fatalf("第一个地址坏了应该用第二个: %s", w.Body.String())
	}
	for i := 0; i < 5; i++ {
		s.handleSupportDonors(httptest.NewRecorder(), httptest.NewRequest("GET", "/app/support/donors", nil))
	}
	time.Sleep(50 * time.Millisecond)
	if hits != 1 {
		t.Fatalf("一小时内只该取一次,实际 %d", hits)
	}
}

func TestSupportPledge(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	old := pledgeURL
	pledgeURL = srv.URL
	defer func() { pledgeURL = old }()
	s, _ := newTestServerForSupport(t, "")
	call := func(body string, mut ...func(*http.Request)) (int, map[string]interface{}) {
		r := httptest.NewRequest("POST", "http://panel:2053/app/support/pledge", strings.NewReader(body))
		r.RemoteAddr = "203.0.113.5:1234"
		for _, m := range mut {
			m(r)
		}
		w := httptest.NewRecorder()
		s.handleSupportPledge(w, r)
		var out map[string]interface{}
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code, out := call(`{"name":"Larry Wang","amount":10}`)
	if code != 200 || !strings.HasPrefix(out["amount"].(string), "10.0") {
		t.Fatalf("%d %v", code, out)
	}
	var sent map[string]string
	if json.Unmarshal([]byte(got), &sent) != nil || sent["n"] != "Larry Wang" || sent["a"] != out["amount"] {
		t.Fatalf("发出去的: %s", got)
	}
	if code, out = call(`{"name":"http://x.com","amount":10}`); code != 400 || out["code"] != "name_bad" {
		t.Fatalf("坏名字: %d %v", code, out)
	}
	if code, out = call(`{"name":"a","amount":0.5}`); code != 400 || out["code"] != "amount" {
		t.Fatalf("金额太小: %d %v", code, out)
	}
	if code, _ = call(`{"name":"a","amount":5}`, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); code != 403 {
		t.Fatalf("跨站: %d", code)
	}
	// 同一来源 10 分钟 5 次,换同一个 /64 的地址也算一个来源
	for i := 0; i < 4; i++ {
		call(`{"name":"a","amount":5}`)
	}
	if code, out = call(`{"name":"a","amount":5}`); code != 429 {
		t.Fatalf("应该限流: %d %v", code, out)
	}
	v6 := func(r *http.Request) { r.RemoteAddr = "[2001:db8:1:2::a]:1" }
	for i := 0; i < 5; i++ {
		if code, _ = call(`{"name":"a","amount":5}`, v6); code != 200 {
			t.Fatalf("第 %d 次: %d", i, code)
		}
	}
	if code, _ = call(`{"name":"a","amount":5}`, func(r *http.Request) { r.RemoteAddr = "[2001:db8:1:2::b]:1" }); code != 429 {
		t.Fatalf("同一个 /64 换地址不该绕过限流: %d", code)
	}
}

// 总开关关掉:三个接口都不再碰外部服务,页面上也没有这些东西。
func TestSupportOnlineOff(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	defer srv.Close()
	oldD, oldP, oldT := donorsURLs, pledgeURL, tronURL
	donorsURLs, pledgeURL, tronURL = []string{srv.URL}, srv.URL, srv.URL
	defer func() { donorsURLs, pledgeURL, tronURL = oldD, oldP, oldT }()
	s, _ := newTestServerForSupport(t, "false")
	for _, call := range []func(http.ResponseWriter, *http.Request){s.handleSupportDonors, s.handleSupportPledge, s.handleSupportCheck} {
		w := httptest.NewRecorder()
		call(w, httptest.NewRequest("POST", "http://panel:2053/x?amount=1&since=1", strings.NewReader(`{"name":"a","amount":5}`)))
		if w.Code != http.StatusNotFound {
			t.Fatalf("关掉后应 404,实际 %d", w.Code)
		}
	}
	if hits != 0 {
		t.Fatalf("关掉后不该有外部请求: %d", hits)
	}
	w := httptest.NewRecorder()
	s.handleSupport(w, httptest.NewRequest("GET", "http://panel:2053/app/support", nil))
	page := w.Body.String()
	for _, gone := range []string{`id="pledge"`, `id="board"`, `id="devs"`, "support/donors"} {
		if strings.Contains(page, gone) {
			t.Fatalf("关掉后页面不该有 %s", gone)
		}
	}
	if !strings.Contains(page, `class="addr"`) {
		t.Fatal("打赏地址本身不受影响")
	}
	s, _ = newTestServerForSupport(t, "")
	w = httptest.NewRecorder()
	s.handleSupport(w, httptest.NewRequest("GET", "http://panel:2053/app/support", nil))
	if !strings.Contains(w.Body.String(), `id="pledge"`) {
		t.Fatal("默认应该是开着的")
	}
}

func TestSupportCheck(t *testing.T) {
	hits := 0
	now := time.Now().Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		fmt.Fprintf(w, `{"data":[
			{"transaction_id":"a","token_info":{"symbol":"USDT","address":"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t","decimals":6,"name":"Tether USD"},"block_timestamp":%d000,"from":"T1","to":"TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm","type":"Transfer","value":"10037000"},
			{"block_timestamp":%d000,"to":"别的地址","type":"Transfer","value":"5000000"},
			{"block_timestamp":%d000,"to":"TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm","type":"Transfer","value":"7000000"}]}`, now-30, now-30, now-3000)
	}))
	defer srv.Close()
	old := tronURL
	tronURL = srv.URL
	defer func() { tronURL = old }()
	s, _ := newTestServerForSupport(t, "")
	ask := func(amount string) map[string]interface{} {
		w := httptest.NewRecorder()
		s.handleSupportCheck(w, httptest.NewRequest("GET", "/x?amount="+amount+"&since="+itoa64(now-60), nil))
		var out map[string]interface{}
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	if out := ask("10.037"); out["arrived"] != true {
		t.Fatalf("实际 %v", out)
	}
	if out := ask("5"); out["arrived"] != false {
		t.Fatalf("转给别人的不算: %v", out)
	}
	if out := ask("10.038"); out["arrived"] != false {
		t.Fatalf("金额对不上不算: %v", out)
	}
	if out := ask("7"); out["arrived"] != false {
		t.Fatalf("留名之前到的不算: %v", out)
	}
	if hits != 1 {
		t.Fatalf("20 秒内只该查一次: %d", hits)
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func newTestServerForSupport(t *testing.T, online string) (*Server, *gorm.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "support.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	if online != "" {
		db.Create(&model.Setting{Key: "supportOnline", Value: online})
	}
	return &Server{db: db, sessions: map[string]session{}}, db
}

// getDonors 打开一次 support/donors,返回名单和响应里的服务器时间。
func getDonors(t *testing.T, s *Server) (donorBoard, int64) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleSupportDonors(w, httptest.NewRequest("GET", "/app/support/donors", nil))
	var out struct {
		donorBoard
		Now int64 `json:"now"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err, w.Body.String())
	}
	return out.donorBoard, out.Now
}

func boardJSON(updatedAt int64, devices int, devicesAt int64, names ...string) string {
	rows := make([]string, 0, len(names))
	for _, n := range names {
		rows = append(rows, fmt.Sprintf(`{"name":%q,"total":10}`, n))
	}
	return fmt.Sprintf(`{"updatedAt":%d,"devices":%d,"devicesAt":%d,"donors":[%s]}`, updatedAt, devices, devicesAt, strings.Join(rows, ","))
}

// waitDonorsIdle 等后台这一轮取名单结束。
func waitDonorsIdle(t *testing.T, s *Server) {
	t.Helper()
	for i := 0; i < 300; i++ {
		s.donors.mu.Lock()
		busy := s.donors.fetching != nil
		s.donors.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("后台取名单一直没结束")
}

// raw 卡住(一直不回)、jsDelivr 2.5 秒才回:以前一个接一个取,卡住的 raw 吃掉 12 秒预算里的 10 秒,兜底整体失败
// (之后每 5 分钟重试一次,结果一样);现在同时取,第一轮就拿到名单,第一个打开的人最多等 3 秒。
func TestDonorBoardHungPrimaryDoesNotStarveFallback(t *testing.T) {
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer hang.Close()
	defer close(release)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2500 * time.Millisecond)
		io.WriteString(w, boardJSON(2, 55, time.Now().Unix(), "Larry Wang"))
	}))
	defer slow.Close()
	old := donorsURLs
	donorsURLs = []string{hang.URL, slow.URL}
	defer func() { donorsURLs = old }()
	s, _ := newTestServerForSupport(t, "")
	start := time.Now()
	b, _ := getDonors(t, s)
	if el := time.Since(start); el > 3500*time.Millisecond {
		t.Fatalf("第一个打开的人等了 %v,最多该等 3 秒", el)
	}
	// 一般第一个人就拿到了;机器忙、刚好超过 3 秒的话,接着打开的人也该在卡住的 raw 超时(10 秒)之前拿到
	for len(b.Donors) == 0 && time.Since(start) < 6*time.Second {
		time.Sleep(50 * time.Millisecond)
		b, _ = getDonors(t, s)
	}
	if b.Devices != 55 || len(b.Donors) != 1 {
		t.Fatalf("raw 卡住时兜底应该取得到: %+v(过了 %v)", b, time.Since(start))
	}
}

// 很久没人打开这页(手上那份比 6 小时还旧):第一个打开的人等新的,不再先拿到几天前那份;
// 只过了一两个小时的,先给手上这份,后台换新的。
func TestDonorBoardTooOldWaitsForFresh(t *testing.T) {
	var body atomic.Value
	body.Store(boardJSON(1, 22, time.Now().Unix(), "Larry Wang"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body.Load().(string))
	}))
	defer srv.Close()
	old := donorsURLs
	donorsURLs = []string{srv.URL}
	defer func() { donorsURLs = old }()
	s, _ := newTestServerForSupport(t, "")
	if b, _ := getDonors(t, s); b.Devices != 22 || len(b.Donors) != 1 {
		t.Fatalf("第一次应该等到名单: %+v", b)
	}
	waitDonorsIdle(t, s)
	age := func(d time.Duration) {
		s.donors.mu.Lock()
		s.donors.got, s.donors.next = time.Now().Add(-d), time.Time{}
		s.donors.mu.Unlock()
	}

	age(72 * time.Hour)
	body.Store(boardJSON(2, 55, time.Now().Unix(), "赛博云", "maomaocloud", "Larry Wang"))
	if b, _ := getDonors(t, s); b.Devices != 55 || len(b.Donors) != 3 {
		t.Fatalf("三天前那份不该直接给第一个打开的人: %+v", b)
	}
	waitDonorsIdle(t, s)

	age(2 * time.Hour)
	body.Store(boardJSON(3, 60, time.Now().Unix(), "赛博云", "maomaocloud", "Larry Wang", "Bob"))
	if b, _ := getDonors(t, s); b.Devices != 55 {
		t.Fatalf("两小时前那份先给出去,后台再换: %+v", b)
	}
	waitDonorsIdle(t, s)
	if b, _ := getDonors(t, s); b.Devices != 60 || len(b.Donors) != 4 {
		t.Fatalf("后台换好之后应是新的: %+v", b)
	}
}

// 同一轮里先到的(比如 jsDelivr)比后到的 raw 旧:先给等着的人,raw 到了再换成新的;后到的更旧就不换。
func TestDonorBoardPrefersFresherInRound(t *testing.T) {
	now := time.Now().Unix()
	older := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, boardJSON(now-12*3600, 50, now-12*3600, "Larry Wang"))
	}))
	defer older.Close()
	newer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, boardJSON(now-600, 52, now-600, "Larry Wang", "Bob"))
	}))
	defer newer.Close()
	old := donorsURLs
	defer func() { donorsURLs = old }()

	donorsURLs = []string{newer.URL, older.URL}
	s, _ := newTestServerForSupport(t, "")
	if b, _ := getDonors(t, s); b.Devices != 50 {
		t.Fatalf("先到的先给: %+v", b)
	}
	waitDonorsIdle(t, s)
	if b, _ := getDonors(t, s); b.Devices != 52 || len(b.Donors) != 2 {
		t.Fatalf("后到的更新,应该换上: %+v", b)
	}

	slowOlder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, boardJSON(now-12*3600, 50, now-12*3600, "Larry Wang"))
	}))
	defer slowOlder.Close()
	fastNewer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, boardJSON(now-600, 52, now-600, "Larry Wang", "Bob"))
	}))
	defer fastNewer.Close()
	donorsURLs = []string{fastNewer.URL, slowOlder.URL}
	s2, _ := newTestServerForSupport(t, "")
	getDonors(t, s2)
	waitDonorsIdle(t, s2)
	if b, _ := getDonors(t, s2); b.Devices != 52 || len(b.Donors) != 2 {
		t.Fatalf("后到的更旧,不该换: %+v", b)
	}
}

// 隔一轮也不倒退:上一轮从 raw 拿到新的,这一轮只有 jsDelivr 给了更旧的一份,手上新的留着;
// 时间写错(比现在晚好几天)的那份照样换上,但挡不住之后正常的名单。
func TestDonorBoardNeverGoesBack(t *testing.T) {
	now := time.Now().Unix()
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body.Load().(string))
	}))
	defer srv.Close()
	old := donorsURLs
	donorsURLs = []string{srv.URL}
	defer func() { donorsURLs = old }()
	s, _ := newTestServerForSupport(t, "")
	round := func(b string) []string {
		body.Store(b)
		s.donors.mu.Lock()
		s.donors.next = time.Time{} // 到了该再取的时候
		s.donors.mu.Unlock()
		getDonors(t, s)
		waitDonorsIdle(t, s)
		got, _ := getDonors(t, s)
		names := []string{}
		for _, d := range got.Donors {
			names = append(names, d.Name)
		}
		return names
	}
	for i, c := range []struct {
		board string
		want  string
	}{
		{boardJSON(now-600, 52, now-600, "Larry Wang", "Bob"), "Larry Wang,Bob"},
		{boardJSON(now-12*3600, 50, now-12*3600, "Larry Wang"), "Larry Wang,Bob"},
		{boardJSON(now+10*86400, 1, now+10*86400, "Zed"), "Zed"},
		{boardJSON(now, 53, now, "Larry Wang", "Bob", "Carol"), "Larry Wang,Bob,Carol"},
	} {
		if got := strings.Join(round(c.board), ","); got != c.want {
			t.Fatalf("第 %d 轮: %s,应为 %s", i+1, got, c.want)
		}
	}
}

// 时间写在未来(超出时钟误差)的名单不可信,那个时间不参与新旧比较:以前"不倒退"只排除比现在晚一天以上的,
// 一份时间写成几小时后的名单会让所有面板在这几个小时里拒收正常的更新。时钟误差以内的照常比较,旧的照样挡住。
func TestDonorBoardFutureStampNotTrusted(t *testing.T) {
	now := time.Now().Unix()
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body.Load().(string))
	}))
	defer srv.Close()
	old := donorsURLs
	donorsURLs = []string{srv.URL}
	defer func() { donorsURLs = old }()
	s, _ := newTestServerForSupport(t, "")
	round := func(b string) string {
		body.Store(b)
		s.donors.mu.Lock()
		s.donors.next = time.Time{} // 到了该再取的时候
		s.donors.mu.Unlock()
		getDonors(t, s)
		waitDonorsIdle(t, s)
		got, _ := getDonors(t, s)
		names := []string{}
		for _, d := range got.Donors {
			names = append(names, d.Name)
		}
		return strings.Join(names, ",")
	}
	for i, c := range []struct {
		board string
		want  string
	}{
		{boardJSON(now-600, 52, now-600, "Larry Wang", "Bob"), "Larry Wang,Bob"},
		{boardJSON(now+3*3600, 50, now+3*3600, "Future"), "Future"},                     // 时间不可信:照样换上,但不靠它算"新"
		{boardJSON(now, 53, now, "Larry Wang", "Bob", "Carol"), "Larry Wang,Bob,Carol"}, // 以前这里要被拒收 3 个小时
		{boardJSON(now+3*3600, 50, now-60, "UpdatedAtWrong"), "Larry Wang,Bob,Carol"},   // updatedAt 写在未来不算,按 devicesAt 比:旧一分钟
		{boardJSON(now-12*3600, 50, now-12*3600, "Larry Wang"), "Larry Wang,Bob,Carol"},
		{boardJSON(now+120, 54, now+120, "Larry Wang", "Bob", "Carol", "Dan"), "Larry Wang,Bob,Carol,Dan"}, // 晚 2 分钟:时钟误差以内
		{boardJSON(now+60, 54, now+60, "Larry Wang"), "Larry Wang,Bob,Carol,Dan"},                          // 比手上的旧:照样挡住
	} {
		if got := round(c.board); got != c.want {
			t.Fatalf("第 %d 轮: %s,应为 %s", i+1, got, c.want)
		}
	}
}

// 面板数的统计时间没有(老版本写的名单)或超过 48 小时:不显示(返回 0),名单照常。
func TestDonorDevicesHiddenWhenStale(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, body.Load().(string))
	}))
	defer srv.Close()
	old := donorsURLs
	donorsURLs = []string{srv.URL}
	defer func() { donorsURLs = old }()
	now := time.Now().Unix()
	for _, c := range []struct {
		name string
		at   int64
		want int
	}{
		{"一小时前算的", now - 3600, 55},
		{"47 小时前算的", now - 47*3600, 55},
		{"49 小时前算的", now - 49*3600, 0},
		{"没有统计时间", 0, 0},
	} {
		body.Store(boardJSON(now, 55, c.at, "Larry Wang"))
		s, _ := newTestServerForSupport(t, "")
		b, serverNow := getDonors(t, s)
		if b.Devices != c.want || len(b.Donors) != 1 || b.DevicesAt != c.at {
			t.Fatalf("%s: %+v,应显示 %d", c.name, b, c.want)
		}
		if serverNow < now || serverNow > time.Now().Unix() {
			t.Fatalf("%s: 响应里的服务器时间不对: %d", c.name, serverNow)
		}
		waitDonorsIdle(t, s)
	}
	// 手上那份放久了(之后一直取不到新的),到显示的时候也按 48 小时算
	s, _ := newTestServerForSupport(t, "")
	body.Store(boardJSON(now, 55, now-3600, "Larry Wang"))
	getDonors(t, s)
	waitDonorsIdle(t, s)
	s.donors.mu.Lock()
	s.donors.board.DevicesAt = now - 50*3600
	s.donors.next = time.Now().Add(time.Hour)
	s.donors.mu.Unlock()
	if b, _ := getDonors(t, s); b.Devices != 0 || len(b.Donors) != 1 {
		t.Fatalf("放了 50 小时的面板数不该再显示: %+v", b)
	}
}

// TronGrid 查失败或正在查:这 20 秒内别人问到的都是 known:false("不知道"),不能说成 known:true/arrived:false。
func TestSupportCheckUnknownWhileFailingOrBusy(t *testing.T) {
	now := time.Now().Unix()
	var fail atomic.Bool
	var hits atomic.Int32
	fail.Store(true)
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "busy", http.StatusBadGateway)
			return
		}
		<-gate
		fmt.Fprintf(w, `{"data":[{"block_timestamp":%d000,"to":"TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm","type":"Transfer","value":"10037000"}]}`, now-30)
	}))
	defer srv.Close()
	old := tronURL
	tronURL = srv.URL
	defer func() { tronURL = old }()
	s, _ := newTestServerForSupport(t, "")
	ask := func() map[string]interface{} {
		w := httptest.NewRecorder()
		s.handleSupportCheck(w, httptest.NewRequest("GET", "/x?amount=10.037&since="+itoa64(now-60), nil))
		var out map[string]interface{}
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	for i := 0; i < 3; i++ {
		if out := ask(); out["known"] != false || out["arrived"] != false {
			t.Fatalf("查失败后第 %d 次: %v", i, out)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("20 秒内只该查一次: %d", hits.Load())
	}

	// 过了 20 秒,这次 TronGrid 慢:查的过程中别人问到的也是"不知道"
	s.donors.mu.Lock()
	s.donors.tronAt = time.Time{}
	s.donors.mu.Unlock()
	fail.Store(false)
	first := make(chan map[string]interface{}, 1)
	go func() { first <- ask() }()
	for i := 0; hits.Load() < 2; i++ {
		if i > 500 {
			t.Fatal("第二次查询一直没发出去")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out := ask(); out["known"] != false {
		t.Fatalf("正在查的时候应是 known:false: %v", out)
	}
	close(gate)
	if out := <-first; out["known"] != true || out["arrived"] != true {
		t.Fatalf("发起查询的那个人应拿到结果: %v", out)
	}
	out := ask()
	if out["known"] != true || out["arrived"] != true || hits.Load() != 2 {
		t.Fatalf("查好之后 20 秒内直接用结果: %v(查了 %d 次)", out, hits.Load())
	}
	if n, _ := out["now"].(float64); int64(n) < now {
		t.Fatalf("响应要带服务器时间: %v", out)
	}
}

// 留名的响应带服务器时间(页面拿它校正倒计时);要转的金额不会超过对账那边认的上限。
func TestSupportPledgeServerTimeAndLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{}`) }))
	defer srv.Close()
	old := pledgeURL
	pledgeURL = srv.URL
	defer func() { pledgeURL = old }()
	s, _ := newTestServerForSupport(t, "")
	r := httptest.NewRequest("POST", "http://panel:2053/app/support/pledge", strings.NewReader(`{"name":"Big","amount":100000}`))
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()
	before := time.Now().Unix()
	s.handleSupportPledge(w, r)
	var out map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &out)
	now, _ := out["now"].(float64)
	exp, _ := out["expiresAt"].(float64)
	if w.Code != 200 || int64(now) < before || int64(now) > time.Now().Unix() || int64(exp)-int64(now) != pledgeWindow {
		t.Fatalf("%d %v", w.Code, out)
	}
	for i := 0; i < 500; i++ {
		if m, ok := usdtMicros(payableAmount(pledgeMaxUSDT)); !ok || m > pledgeMaxMicros {
			t.Fatalf("最大金额加零头超过了对账上限: %d", m)
		}
	}
}

// 面板和 deploy/donors/settle.mjs 各写了一份的常量要一致:留名窗口、金额上限、面板数多久算过期。
func TestDonorConstantsMatchSettle(t *testing.T) {
	src, err := os.ReadFile("../deploy/donors/settle.mjs")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ re, want string }{
		{`const WINDOW = (\S+);`, itoa64(pledgeWindow)},
		{`export const MAX_MICROS = (\S+);`, "100_000_100_000"},
		{`export const DEVICES_FRESH = (.+?);`, "48 * 3600"},
	} {
		m := regexp.MustCompile(c.re).FindSubmatch(src)
		if m == nil || string(m[1]) != c.want {
			t.Errorf("settle.mjs 里 %s 应为 %s,实际 %q", c.re, c.want, m)
		}
	}
	if pledgeMaxMicros != 100_000_100_000 || devicesFresh != 48*3600 {
		t.Fatal("改了这边的常量,settle.mjs 要一起改")
	}
}
