package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"gorm.io/gorm"
)

// 副机 agent 接口的鉴权与参数校验。不起数据面,所以不带 !race 标签 —— 竞争检测那一步照样跑它
// (agent_apply_e2e_test.go 起真实数据面,会撞上上游库自己的竞争,才在 -race 下跳过)。

func TestAgentApplyAuthAndValidation(t *testing.T) {
	db := openAgentTestDB(t, "x.db")
	s := &Server{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc(innerBase+"api/agent/", s.handleAgent)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	post := func(token string, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/app/api/agent/apply", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("X-Agent-Token", token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := post("tok", `{"revision":"r1"}`); code != http.StatusForbidden {
		t.Fatalf("没开副机模式应 403,实际 %d %s", code, body)
	}
	db.Create(&model.Setting{Key: "nodeMode", Value: "true"})
	db.Create(&model.Setting{Key: "nodeToken", Value: "tok"})
	if code, body := post("nope", `{"revision":"r1"}`); code != http.StatusUnauthorized {
		t.Fatalf("令牌错误应 401,实际 %d %s", code, body)
	}
	if code, body := post("", `{"revision":"r1"}`); code != http.StatusUnauthorized {
		t.Fatalf("缺令牌应 401,实际 %d %s", code, body)
	}
	if code, body := post("tok", `{"users":[]}`); code != http.StatusBadRequest {
		t.Fatalf("缺修订号应 400,实际 %d %s", code, body)
	}
	if code, body := post("tok", `{"revision":"r1"}`); code != http.StatusServiceUnavailable || !strings.Contains(body, "未初始化") {
		t.Fatalf("数据面未初始化时不能确认应用,应 503,实际 %d %s", code, body)
	}
	if got := s.setting("hubRevision"); got != "" {
		t.Fatalf("被拒的请求不该落库,hubRevision=%q", got)
	}
}

func openAgentTestDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	return db
}
