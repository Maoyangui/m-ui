package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/appdomains"
)

// 按应用名查域名:接口把名字交给查询客户端、原样返回结果;不填 / 太多 / 太长的名字直接拒。
func TestHandleRouteApps(t *testing.T) {
	repo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/data/netflix" {
			w.Write([]byte("netflix.com\nnflxvideo.net\nfull:www.netflix.com\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer repo.Close()
	old := routeApps
	routeApps = &appdomains.Client{Bases: []string{repo.URL + "/data/"}, HTTP: &http.Client{Timeout: 5 * time.Second}, TTL: time.Hour}
	defer func() { routeApps = old }()

	s := &Server{}
	call := func(body string) (int, string) {
		w := httptest.NewRecorder()
		s.handleRouteApps(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	code, body := call(`{"names":["奈飞","没这个"]}`)
	if code != http.StatusOK {
		t.Fatalf("应返回 200: %d %s", code, body)
	}
	var out struct {
		Results []appdomains.Result `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Results) != 2 {
		t.Fatalf("结果解析失败: %v %s", err, body)
	}
	if r := out.Results[0]; r.List != "netflix" || len(r.Suffix) != 2 || len(r.Full) != 0 {
		t.Fatalf("奈飞应查到 netflix 的两个后缀(www.netflix.com 被盖住): %+v", r)
	}
	if r := out.Results[1]; r.Error == "" {
		t.Fatalf("没有的应用应报没找到: %+v", r)
	}
	for _, bad := range []string{`{"names":[]}`, `{"names":["` + strings.Repeat("长", 65) + `"]}`, `{"names":[` + strings.Repeat(`"a",`, 20) + `"b"]}`, `nope`} {
		if code, _ := call(bad); code != http.StatusBadRequest {
			t.Errorf("%s 应被拒,得到 %d", bad, code)
		}
	}
	w := httptest.NewRecorder()
	s.handleRouteApps(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405,得到 %d", w.Code)
	}
}

// 前端靠"没找到"这句原文区分没这个应用与查询出错(后者不该提示换名字),两边要一致。
func TestRouteAppsNotFoundTextMatchesUI(t *testing.T) {
	js, err := os.ReadFile("assets/js/pages/lines.js")
	if err != nil {
		t.Fatal(err)
	}
	if want := "const RR_NOT_FOUND = '" + appdomains.ErrNotFound.Error() + "'"; !strings.Contains(string(js), want) {
		t.Fatalf("lines.js 里应有 %s", want)
	}
}
