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

// 面板挂在同机反代后面时,外面的请求对端也是回环:带转发头的一律当外部请求拒掉。
func TestHealthRejectsProxiedLoopback(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded"} {
		req := httptest.NewRequest("GET", "/app/api/health", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set(h, "203.0.113.5")
		rec := httptest.NewRecorder()
		s.handleHealth(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("带 %s 的回环请求应 403,得到 %d", h, rec.Code)
		}
	}
}

// 面板只监听某个地址时,本机健康检查从这个地址连进来:对端是它自己,也算本机;别的地址照样拒(审计 MB06)。
func TestHealthAllowsOwnListenAddress(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Setting{Key: "webListen", Value: "10.8.0.1"})
	for addr, want := range map[string]int{"10.8.0.1:1234": http.StatusOK, "10.8.0.2:1234": http.StatusForbidden, "127.0.0.1:1234": http.StatusOK} {
		req := httptest.NewRequest("GET", "/app/api/health", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		s.handleHealth(rec, req)
		if rec.Code != want {
			t.Fatalf("来源 %s 应得 %d,得到 %d", addr, want, rec.Code)
		}
	}
}
