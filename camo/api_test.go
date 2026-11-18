package camo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// apiDo 对伪装站发一个请求;任何回应都不许带 Cookie(伪装站可能和面板、订阅在同一个域名上)
func apiDo(t *testing.T, s *Site, method, path, ctype, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("%s %s 不该发 Cookie", method, path)
	}
	return w
}

func find(t *testing.T, re, page string) string {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("页面里没找到 %s: %.400s", re, page)
	}
	return m[1]
}

// errText 报错里给人看的那一句(几种「框架」写法:message、detail 字符串、detail 列表的第一项)
func errText(t *testing.T, body []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("不是 JSON: %s", body)
	}
	if s, ok := m["message"].(string); ok {
		return s
	}
	if s, ok := m["detail"].(string); ok {
		return s
	}
	if l, ok := m["detail"].([]any); ok && len(l) > 0 {
		return l[0].(map[string]any)["msg"].(string)
	}
	t.Fatalf("报错里没有给人看的话: %s", body)
	return ""
}

// 视频站的「后端」:顶栏「登录」→ 登录页,能到注册、找回密码;JSON 提交:格式不对 422、邮箱密码不对 401、邀请码一律不对 403、
// 找回密码 202;没开脚本的表单提交回一页带提示的 HTML(不回显提交的内容);当前用户 401、视频列表 12 个、播放 401;
// 接口前缀下没有的路径 JSON 404、方法不对 405、HEAD 不带正文;请求体超过 16 KB 413;页面引用的脚本能取到、用的是这个站的接口前缀。
func TestVideoAPI(t *testing.T) {
	styles := map[int]bool{}
	for _, seed := range seeds {
		s := New("video", seed, testNow)
		styles[s.errStyle] = true
		home := string(s.Home())
		login := find(t, `href="([^"]+)">Sign in</a></div></header>`, home)
		w := apiDo(t, s, http.MethodGet, login, "", "")
		if w.Code != 200 {
			t.Fatalf("登录页 %s: %d", login, w.Code)
		}
		lp := w.Body.String()
		if !strings.Contains(lp, `role="alert" hidden`) {
			t.Fatal("登录页的提示框平时是藏着的")
		}
		loginAPI := find(t, `<form method="post" action="([^"]+)"`, lp)
		regPage := apiDo(t, s, http.MethodGet, find(t, `href="([^"]+)">Create an account</a>`, lp), "", "").Body.String()
		regAPI := find(t, `<form method="post" action="([^"]+)"`, regPage)
		invite := find(t, `name="(invite|invite_code|inviteCode|code)"`, regPage)
		resetPage := apiDo(t, s, http.MethodGet, find(t, `href="([^"]+)">Forgot password\?</a>`, lp), "", "").Body.String()
		resetAPI := find(t, `<form method="post" action="([^"]+)"`, resetPage)
		const js = "application/json"
		if w := apiDo(t, s, http.MethodPost, loginAPI, js, `{}`); w.Code != 422 || errText(t, w.Body.Bytes()) == "" {
			t.Fatalf("空的登录应 422: %d %s", w.Code, w.Body)
		}
		if w := apiDo(t, s, http.MethodPost, loginAPI, js, `{"email":"a@example.com","password":"x"}`); w.Code != 401 || !strings.Contains(strings.ToLower(errText(t, w.Body.Bytes())), "password") {
			t.Fatalf("邮箱密码不对应 401: %d %s", w.Code, w.Body)
		}
		if w := apiDo(t, s, http.MethodPost, regAPI, js, `{"email":"a@example.com","password":"longenough1","`+invite+`":"ABCD-1234"}`); w.Code != 403 || !strings.Contains(strings.ToLower(errText(t, w.Body.Bytes())), "invite") {
			t.Fatalf("邀请码应一律不对: %d %s", w.Code, w.Body)
		}
		if w := apiDo(t, s, http.MethodPost, regAPI, js, `{"email":"a@example.com","password":"short"}`); w.Code != 422 {
			t.Fatalf("没填邀请码、密码太短应 422: %d %s", w.Code, w.Body)
		}
		if w := apiDo(t, s, http.MethodPost, resetAPI, js, `{"email":"a@example.com"}`); w.Code != 202 || !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("找回密码应 202: %d %s", w.Code, w.Body)
		}
		form := url.Values{"email": {"a@example.com"}, "password": {"secret-typed-by-someone"}}.Encode()
		w = apiDo(t, s, http.MethodPost, loginAPI, "application/x-www-form-urlencoded", form)
		if w.Code != 401 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), `role="alert">`) ||
			strings.Contains(w.Body.String(), "secret-typed-by-someone") || !strings.Contains(w.Body.String(), s.Name()) {
			t.Fatalf("没开脚本的提交应回带提示的登录页: %d %.300s", w.Code, w.Body)
		}
		// 没开脚本、空着提交:这一页上不会标出字段,提示直接写各字段的问题(自审 G2)
		w = apiDo(t, s, http.MethodPost, loginAPI, "application/x-www-form-urlencoded", "")
		if b := w.Body.String(); w.Code != 422 || !strings.Contains(b, "Enter a valid email address.") || !strings.Contains(b, "Enter your password.") || strings.Contains(b, "highlighted") {
			t.Fatalf("没开脚本的空提交应写出各字段的提示: %d %.300s", w.Code, b)
		}
		// 注册页空着提交:几句提示按表单从上到下的顺序 —— 邮箱、密码、邀请码(自审第五遍 G9)
		w = apiDo(t, s, http.MethodPost, regAPI, "application/x-www-form-urlencoded", "")
		if b := w.Body.String(); w.Code != 422 || strings.Index(b, "Enter a valid email address.") < 0 ||
			strings.Index(b, "Use at least 8 characters.") < strings.Index(b, "Enter a valid email address.") ||
			strings.Index(b, "Enter your invite code.") < strings.Index(b, "Use at least 8 characters.") {
			t.Fatalf("没开脚本时注册页的提示应按表单的先后: %d %.400s", w.Code, b)
		}
		if w := apiDo(t, s, http.MethodPost, loginAPI, js, `{"email":"`+strings.Repeat("a", 20<<10)+`"}`); w.Code != 413 {
			t.Fatalf("请求体太大应 413: %d", w.Code)
		}
		if w := apiDo(t, s, http.MethodGet, s.apiPre+"me", "", ""); w.Code != 401 || w.Header().Get("Content-Type") != jsonType {
			t.Fatalf("当前用户应 401: %d", w.Code)
		}
		w = apiDo(t, s, http.MethodGet, s.apiPre+"videos", "", "")
		var list struct{ Data []map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Data) != 12 {
			t.Fatalf("视频列表: %d %.200s", w.Code, w.Body)
		}
		id := find(t, `href="/watch/([a-z0-9]+)"`, home)
		if w := apiDo(t, s, http.MethodPost, s.apiPre+"videos/"+id+"/play", js, `{}`); w.Code != 401 {
			t.Fatalf("播放应要先登录: %d", w.Code)
		}
		if w := apiDo(t, s, http.MethodGet, s.apiPre+"videos/"+id+"/play", "", ""); w.Code != 405 || w.Header().Get("Allow") != "POST" {
			t.Fatalf("方法不对应 405: %d %v", w.Code, w.Header())
		}
		if w := apiDo(t, s, http.MethodHead, s.apiPre+"videos/"+id, "", ""); w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("HEAD: %d %d", w.Code, w.Body.Len())
		}
		if w := apiDo(t, s, http.MethodGet, s.apiPre+"admin/users", "", ""); w.Code != 404 || w.Header().Get("Content-Type") != jsonType {
			t.Fatalf("接口前缀下没有的路径应 JSON 404: %d %v", w.Code, w.Header())
		}
		src := find(t, `<script src="([^"]+)" defer></script>`, home)
		if w := apiDo(t, s, http.MethodGet, src, "", ""); w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") || !strings.Contains(w.Body.String(), `"`+s.apiPre+`"`) {
			t.Fatalf("脚本 %s: %d", src, w.Code)
		}
	}
	if len(styles) < 2 {
		t.Fatalf("报错的写法应随种子变: %v", styles)
	}
}

// 报错里各字段按表单从上到下的顺序(自审第五遍 G9):邮箱、密码在前,别的按名字;FastAPI 那种逐字段的列表也这样排。
func TestFieldOrder(t *testing.T) {
	if got := strings.Join(fieldOrder(map[string]string{"zz": "d", "invite_code": "c", "password": "b", "email": "a"}), ","); got != "email,password,invite_code,zz" {
		t.Fatalf("顺序不对: %s", got)
	}
	for seed := uint64(1); seed < 200; seed++ {
		s := New("video", seed, testNow)
		if s.errStyle != 1 {
			continue
		}
		lp := apiDo(t, s, http.MethodGet, find(t, `href="([^"]+)">Sign in</a></div></header>`, string(s.Home())), "", "").Body.String()
		regPage := apiDo(t, s, http.MethodGet, find(t, `href="([^"]+)">Create an account</a>`, lp), "", "").Body.String()
		regAPI := find(t, `<form method="post" action="([^"]+)"`, regPage)
		invite := find(t, `name="(invite|invite_code|inviteCode|code)"`, regPage)
		w := apiDo(t, s, http.MethodPost, regAPI, "application/json", `{}`)
		var d struct{ Detail []struct{ Loc []string } }
		if json.Unmarshal(w.Body.Bytes(), &d) != nil || len(d.Detail) != 3 || d.Detail[0].Loc[1] != "email" || d.Detail[1].Loc[1] != "password" || d.Detail[2].Loc[1] != invite {
			t.Fatalf("FastAPI 那种逐字段列表应按表单的先后: %s", w.Body)
		}
		return
	}
	t.Fatal("200 个种子里没有一个用 FastAPI 写法的")
}

// 别的几种站的接口:软件下载的检查更新(版本、三个平台的文件,和页面一致)、邮件订阅(格式不对 422,其余 200;
// 没开脚本时回带提示的订阅页);博客的订阅;镜像站的同步状态(项目数和表格一致)。
func TestOtherAPIs(t *testing.T) {
	for _, seed := range seeds {
		d := New("downloads", seed, testNow)
		w := apiDo(t, d, http.MethodGet, d.apiPre+"releases/latest", "", "")
		var rel struct {
			Version string
			Files   []map[string]any
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rel) != nil || len(rel.Files) != 3 || !strings.Contains(string(d.Home()), "Version "+rel.Version+" ") {
			t.Fatalf("检查更新: %d %.300s", w.Code, w.Body)
		}
		for _, s := range []*Site{d, New("blog", seed, testNow)} {
			api := s.apiPre + "subscribe"
			if w := apiDo(t, s, http.MethodPost, api, "application/json", `{"email":"nope"}`); w.Code != 422 {
				t.Fatalf("%s 订阅:邮箱不对应 422: %d", s.template, w.Code)
			}
			if w := apiDo(t, s, http.MethodPost, api, "application/json", `{"email":"a@example.com"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
				t.Fatalf("%s 订阅: %d %s", s.template, w.Code, w.Body)
			}
			w := apiDo(t, s, http.MethodPost, api, "application/x-www-form-urlencoded", "email=a%40example.com")
			if w.Code != 200 || !strings.Contains(w.Body.String(), `role="alert">`) {
				t.Fatalf("%s 没开脚本的订阅: %d", s.template, w.Code)
			}
			if w := apiDo(t, s, http.MethodGet, "/subscribe/", "", ""); w.Code != 200 {
				t.Fatalf("%s 订阅页: %d", s.template, w.Code)
			}
		}
		m := New("mirror", seed, testNow)
		home := string(m.Home())
		w = apiDo(t, m, http.MethodGet, find(t, `Machine-readable status: <a href="([^"]+)"`, home), "", "")
		var st struct{ Projects []map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil || len(st.Projects) != strings.Count(home, "<tr><td><a class=") {
			t.Fatalf("同步状态: %d %d", w.Code, len(st.Projects))
		}
	}
}
