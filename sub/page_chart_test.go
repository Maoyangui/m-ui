package sub

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 副机的落地页不出流量图:它只有经过自己的原始流量,没算倍率也没有别的服务器,和总用量对不上。
func TestNodeLandingPageHasNoChart(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := NewServer(db)
	u := model.User{Name: "alice", Enabled: true}
	db.Create(&u)
	req := httptest.NewRequest("GET", "/sub/alice?stats=24", nil)
	w := httptest.NewRecorder()
	s.serveStats(w, req, u)
	if w.Code != 200 {
		t.Fatalf("主机应给图: %d", w.Code)
	}
	db.Create(&model.Setting{Key: "nodeMode", Value: "true"})
	w = httptest.NewRecorder()
	s.serveStats(w, req, u)
	if w.Code != 404 {
		t.Fatalf("副机不该给图: %d", w.Code)
	}
}
