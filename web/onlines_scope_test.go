package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// 代理面板的在线接口只给授权给他的线路名,上游名(主面板的布局)一概不给。
func TestResellerOnlinesHideUngrantedLinesAndUpstreams(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	db := run.DB()
	defer database.Close(db)
	s := NewServer(run)
	db.Create(&model.Line{Name: "给他的", Protocol: "hysteria2", Port: 30443, Enabled: true})
	db.Create(&model.Line{Name: "没给他的", Protocol: "anytls", Port: 30444, Enabled: true})
	rs := model.Reseller{Name: "r", Enabled: true}
	db.Create(&rs)
	db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 1})

	if got := s.grantedLineNames(rs.Id, []string{"没给他的", "给他的", "warp"}); len(got) != 1 || got[0] != "给他的" {
		t.Fatalf("只应留授权的线路名: %v", got)
	}
	r := httptest.NewRequest("GET", "http://x/dl/api/onlines", nil)
	r = r.WithContext(withScope(r, rs.Id))
	w := httptest.NewRecorder()
	s.handleOnlines(w, r)
	var out struct{ Upstreams []string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Upstreams == nil || len(out.Upstreams) != 0 {
		t.Fatalf("代理拿到的上游应是空数组: %d %s", w.Code, w.Body.String())
	}
}
