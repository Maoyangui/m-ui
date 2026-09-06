package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func TestHealthLoopbackOnlyAndLinesGate(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}

	req := httptest.NewRequest("GET", "/app/api/health", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("公网来源应 403,得到 %d", rec.Code)
	}

	// 没有线路:数据面不跑也算健康
	req = httptest.NewRequest("GET", "/app/api/health", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec = httptest.NewRecorder()
	s.handleHealth(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("无线路时应 200,得到 %d %s", rec.Code, rec.Body.String())
	}
	var st healthStatus
	json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.OK || st.Lines != 0 {
		t.Fatalf("状态不对: %+v", st)
	}

	// 有一条启用线路而数据面没起:不健康
	db.Create(&model.Line{Name: "l1", Protocol: "shadowsocks", Port: 30001, Enabled: true})
	req = httptest.NewRequest("GET", "/app/api/health", nil)
	req.RemoteAddr = "[::1]:1234"
	rec = httptest.NewRecorder()
	s.handleHealth(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("有线路无数据面应 503,得到 %d %s", rec.Code, rec.Body.String())
	}
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st.OK || st.Lines != 1 || st.Core {
		t.Fatalf("状态不对: %+v", st)
	}
}
