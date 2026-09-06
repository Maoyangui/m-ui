package sub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

func TestRateLimitPerIP(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Setting{Key: "subRateLimit", Value: "5"})
	got429 := false
	for i := 0; i < 8; i++ {
		r := httptest.NewRequest("GET", "http://hk.example:2056/sub/alice", nil)
		r.RemoteAddr = "203.0.113.9:4000"
		r.Header.Set("User-Agent", "clash")
		w := httptest.NewRecorder()
		s.handle()(w, r)
		if w.Code == http.StatusTooManyRequests {
			got429 = true
			if w.Header().Get("Retry-After") == "" {
				t.Fatal("429 应带 Retry-After")
			}
		}
	}
	if !got429 {
		t.Fatal("同一 IP 超过每分钟上限后应收到 429")
	}
	// 别的 IP 不受影响
	r := httptest.NewRequest("GET", "http://hk.example:2056/sub/alice", nil)
	r.RemoteAddr = "198.51.100.7:4000"
	r.Header.Set("User-Agent", "clash")
	w := httptest.NewRecorder()
	s.handle()(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("另一 IP 应正常,得到 %d", w.Code)
	}
}

func TestMissesAggregatedPerIP(t *testing.T) {
	s, db := shareServer(t)
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("GET", "http://hk.example:2056/sub/nobody"+string(rune('a'+i)), nil)
		r.RemoteAddr = "203.0.113.10:4000"
		r.Header.Set("User-Agent", "curl/8")
		w := httptest.NewRecorder()
		s.handle()(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("对不上的地址应 404,得到 %d", w.Code)
		}
	}
	var n int64
	db.Model(&model.SubLog{}).Count(&n)
	if n != 0 {
		t.Fatalf("404 不应逐条落库,现有 %d 行", n)
	}
	s.flushMisses()
	var rows []model.SubLog
	db.Find(&rows)
	if len(rows) != 1 || rows[0].Ip != "203.0.113.10" || rows[0].Format != "miss" || !strings.Contains(rows[0].Ua, "×3") {
		t.Fatalf("应聚合成一行: %+v", rows)
	}
}

func TestExhaustedUserBlockedForClientsButLandingOpens(t *testing.T) {
	s, db := shareServer(t)
	db.Model(&model.User{}).Where("name = ?", "alice").Updates(map[string]interface{}{"volume": 100, "up": 60, "down": 40})
	if w := doReq(s, "GET", "/sub/alice", "clash"); w.Code != http.StatusNotFound {
		t.Fatalf("流量用尽的用户客户端应 404,得到 %d", w.Code)
	}
	if w := doReq(s, "GET", "/sub/alice", "Mozilla/5.0 Chrome Safari"); w.Code != http.StatusOK {
		t.Fatalf("落地页仍要能打开,得到 %d", w.Code)
	}
}
