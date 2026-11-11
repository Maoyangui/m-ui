package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 外部 API(主面板令牌与代理令牌)鉴权失败按来源 IP 限流并记审计(审计 Q-32 #40):
// 10 分钟内失败 10 次,这个来源先挡 5 分钟(期间带对的令牌也 429),别的来源不受影响;两个入口共用一个计数。
func TestPublicAPIAuthFailureLimited(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Setting{Key: "apiEnabled", Value: "true"})
	db.Create(&model.Setting{Key: "apiToken", Value: "good-token-0123456789"})
	db.Create(&model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-token-0123456789"})

	call := func(reseller bool, ip, token string) int {
		path := "http://x/app/api/v1/users"
		if reseller {
			path = "http://x/dl/api/v1/users"
		}
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = ip + ":40000"
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		if reseller {
			s.handleResellerPublicAPI(w, req)
		} else {
			s.handlePublicAPI(w, req)
		}
		return w.Code
	}
	const bad = "203.0.113.7"
	if code := call(false, bad, "good-token-0123456789"); code != 200 {
		t.Fatalf("对的令牌应通过: %d", code)
	}
	for i := 0; i < 5; i++ {
		if code := call(false, bad, "guess"); code != 401 {
			t.Fatalf("第 %d 次错令牌应 401: %d", i+1, code)
		}
		if code := call(true, bad, "guess"); code != 401 {
			t.Fatalf("代理入口第 %d 次错令牌应 401: %d", i+1, code)
		}
	}
	if code := call(false, bad, "good-token-0123456789"); code != http.StatusTooManyRequests {
		t.Fatalf("失败 10 次后这个来源应被挡(两个入口一起算): %d", code)
	}
	if code := call(true, bad, "rs-token-0123456789"); code != http.StatusTooManyRequests {
		t.Fatalf("被挡期间代理入口同样 429: %d", code)
	}
	if code := call(false, "198.51.100.9", "good-token-0123456789"); code != 200 {
		t.Fatalf("别的来源不受影响: %d", code)
	}
	if code := call(true, "198.51.100.9", "rs-token-0123456789"); code != 200 {
		t.Fatalf("别的来源的代理令牌不受影响: %d", code)
	}
	// 审计:来源第一次失败记一条,被挡时记一条(不是每次失败都写,扫描器撑不大审计表)
	var fails, blocks []model.Change
	db.Where("key = ? AND action = ?", "api", "auth:fail").Find(&fails)
	db.Where("key = ? AND action = ?", "api", "auth:block").Find(&blocks)
	if len(fails) != 1 || len(blocks) != 1 || !strings.Contains(string(fails[0].Obj), bad) || !strings.Contains(string(blocks[0].Obj), bad) {
		t.Fatalf("审计应有一条首次失败、一条被挡,都带来源 IP: %+v %+v", fails, blocks)
	}
}

// 面板与代理面板的 JSON 接口统一限制请求体:超大请求体读到上限就停,返回能看懂的错误;外部 API 同样。
// 上传类(multipart:备份还原、旧面板导入)各有自己的处理,不受这个上限影响。
func TestJSONBodyLimited(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db, sessions: map[string]session{}}
	db.Create(&model.Setting{Key: "apiEnabled", Value: "true"})
	db.Create(&model.Setting{Key: "apiToken", Value: "good-token-0123456789"})
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-token-0123456789"}
	db.Create(&rs)
	admin := s.newSession("admin")
	rsTok := s.newResellerSession(rs, false)

	huge := func() *countingReader {
		return &countingReader{r: io.MultiReader(strings.NewReader(`{"name":"`), strings.NewReader(strings.Repeat("a", 64<<20)), strings.NewReader(`"}`))}
	}
	var gotBody *countingReader
	echo := func(w http.ResponseWriter, r *http.Request) {
		var v map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			badRequest(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"ok": "1"})
	}
	cases := map[string]struct {
		limit int
		run   func(body io.Reader, ct string) *httptest.ResponseRecorder
	}{
		"主面板": {panelBodyLimit, func(body io.Reader, ct string) *httptest.ResponseRecorder {
			req := httptest.NewRequest("POST", "http://x/app/api/users", body)
			req.Header.Set("Content-Type", ct)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: admin})
			w := httptest.NewRecorder()
			s.auth(echo)(w, req)
			return w
		}},
		"代理面板": {resellerBodyLimit, func(body io.Reader, ct string) *httptest.ResponseRecorder {
			req := httptest.NewRequest("POST", "http://x/dl/api/users", body)
			req.Header.Set("Content-Type", ct)
			req.AddCookie(&http.Cookie{Name: resellerCookie, Value: rsTok})
			w := httptest.NewRecorder()
			s.rauth(echo)(w, req)
			return w
		}},
		"外部 API": {apiBodyLimit, func(body io.Reader, ct string) *httptest.ResponseRecorder {
			req := httptest.NewRequest("POST", "http://x/app/api/v1/users", body)
			req.Header.Set("Content-Type", ct)
			req.Header.Set("Authorization", "Bearer good-token-0123456789")
			w := httptest.NewRecorder()
			s.handlePublicAPI(w, req)
			return w
		}},
	}
	for name, c := range cases {
		gotBody = huge()
		w := c.run(gotBody, "application/json")
		if w.Code != 400 || !strings.Contains(w.Body.String(), "请求内容过大") {
			t.Fatalf("%s:超大 JSON 应被拒并说明: %d %s", name, w.Code, w.Body.String())
		}
		if gotBody.n > c.limit+64<<10 {
			t.Fatalf("%s:应在读到上限时就停,实际读了 %d 字节", name, gotBody.n)
		}
		// 上限以内照常
		if w := c.run(strings.NewReader(`{"name":"`+strings.Repeat("a", 512<<10)+`"}`), "application/json"); name != "外部 API" && w.Code != 200 {
			t.Fatalf("%s:上限以内应照常: %d %s", name, w.Code, w.Body.String())
		}
	}
	// 主面板的 multipart 上传(备份还原、旧面板导入)不套这个上限
	gotBody = &countingReader{r: strings.NewReader(strings.Repeat("a", panelBodyLimit+1<<20))}
	var read int64
	upload := func(w http.ResponseWriter, r *http.Request) {
		read, _ = io.Copy(io.Discard, r.Body)
		writeJSON(w, 200, map[string]string{"ok": "1"})
	}
	req := httptest.NewRequest("POST", "http://x/app/api/backup/restore", gotBody)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: admin})
	w := httptest.NewRecorder()
	s.auth(upload)(w, req)
	if w.Code != 200 || read != int64(panelBodyLimit+1<<20) {
		t.Fatalf("multipart 上传不该被 JSON 上限截断: %d 读了 %d", w.Code, read)
	}
}
