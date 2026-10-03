package web

import (
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 重置代理流量只恢复因超量被停、而且没到期的用户:先超量后到期的人原因一直是 quota,以前也被启用(审计 MB14)。
func TestResellerResetSkipsExpired(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	rs := model.Reseller{Name: "dl", Enabled: true}
	db.Create(&rs)
	now := time.Now().Unix()
	db.Create(&model.User{Name: "live", ResellerId: rs.Id, Volume: 100, Up: 100, Expiry: now + 86400, SubToken: "l1l1l1l1l1l1l1l1l1l1l1l1"})
	db.Create(&model.User{Name: "dead", ResellerId: rs.Id, Volume: 100, Up: 100, Expiry: now - 60, SubToken: "d1d1d1d1d1d1d1d1d1d1d1d1"})
	db.Model(&model.User{}).Where("reseller_id = ?", rs.Id).Updates(map[string]interface{}{"enabled": false, "disabled_reason": model.DisabledQuota})

	w := httptest.NewRecorder()
	if !s.dispatchResellerSubroute(w, httptest.NewRequest("POST", "/app/api/resellers/"+strconv.FormatUint(uint64(rs.Id), 10)+"/reset", nil)) || w.Code != 200 {
		t.Fatalf("重置失败: %d %s", w.Code, w.Body.String())
	}
	var live, dead model.User
	db.Where("name = ?", "live").First(&live)
	db.Where("name = ?", "dead").First(&dead)
	if !live.Enabled || live.Up != 0 {
		t.Fatalf("没到期的应清零并恢复: %+v", live)
	}
	if dead.Enabled {
		t.Fatalf("已到期的不该被启用: %+v", dead)
	}
}
