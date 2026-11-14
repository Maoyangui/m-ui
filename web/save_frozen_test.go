package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ha"
)

// 设为主机冻结期间保存线路:在写闸那里直接回 503,不进保存流程 —— 不先排队等 sing-box 干跑、做完校验再被拒,
// 也不留下任何改动。干跑锁被别人占着也照样立刻返回(进了保存流程就会卡在干跑上)。
func TestLineSaveFrozenReturns503BeforeValidation(t *testing.T) {
	s, run, _ := saveEnv(t)
	db := run.DB()
	var line model.Line
	db.Where("name = ?", "香港1").First(&line)
	db.Exec("INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		ha.KeyFreezeUntil, strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
	if !run.HA().Frozen() {
		t.Fatal("前提:应处于冻结中")
	}
	release := core.HoldValidation()
	defer release()

	tok := s.newSession("admin")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		body := fmt.Sprintf(`{"name":"香港1-冻结中","protocol":"socks","port":%d,"enabled":false}`, line.Port)
		req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("http://x/app/api/lines/%d", line.Id), strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		w := httptest.NewRecorder()
		s.auth(s.masterOnly(s.handleLineItem))(w, req)
		done <- w
	}()
	select {
	case w := <-done:
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "写操作暂停") {
			t.Fatalf("冻结期间保存应直接 503: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("冻结期间保存进了保存流程、在排队等干跑:应在写闸那里直接 503")
	}
	var n int64
	db.Model(&model.Line{}).Where("name = ?", "香港1-冻结中").Count(&n)
	if n != 0 {
		t.Fatal("冻结期间的保存不能落库")
	}
}

// 面板慢请求日志的路由形状:路径已改写到内部前缀,对外路径不出现;id 打码。
func TestPanelRouteShape(t *testing.T) {
	for in, want := range map[string]string{
		innerBase + "api/lines/12":        "/api/lines/:id",
		innerBase + "api/v1/users/alice":  "/api/v1/users/:name",
		"/my-secret-panel/api/lines":      "{other}",
		innerBase + "api/upstreams/3?x=y": "/api/upstreams/:id",
	} {
		r := httptest.NewRequest(http.MethodGet, "http://x"+in, nil)
		if got := panelRouteShape(r); got != want {
			t.Errorf("panelRouteShape(%q) = %q,应为 %q", in, got, want)
		}
	}
}
