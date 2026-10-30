package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
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
		io.WriteString(w, `{"updatedAt":1,"devices":12,"donors":[{"name":"Larry Wang","total":10}]}`)
	}))
	defer good.Close()
	old := donorsURLs
	donorsURLs = []string{bad.URL, good.URL}
	defer func() { donorsURLs = old }()
	s := &Server{}
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
	s := &Server{}
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
	s := &Server{}
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
