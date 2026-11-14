package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 外部设备租约接口也要有请求体上限(以前没有)。
func TestAgentExternalIPsBodyLimit(t *testing.T) {
	defer func(n int64) { externalIPsLimit = n }(externalIPsLimit)
	externalIPsLimit = 1 << 10
	db := openAgentTestDB(t, "x.db")
	db.Create(&model.Setting{Key: "nodeMode", Value: "true"})
	db.Create(&model.Setting{Key: "nodeToken", Value: "tok"})
	s := &Server{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc(innerBase+"api/agent/", s.handleAgent)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	body := `{"alice":["` + strings.Repeat("1", 4<<10) + `"]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/app/api/agent/external-ips", strings.NewReader(body))
	req.Header.Set("X-Agent-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("超过上限的请求体应 400,实际 %d", resp.StatusCode)
	}
}
