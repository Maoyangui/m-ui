package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 显式字段优先于套餐:修改时同时给套餐和 days,days 替代套餐自带的天数,从原到期(未过期时)起算;
// 以前在套餐延好的到期上再加一遍,计费系统按文档对接会多给一个周期(审计 MB09)。
func TestPatchPlanWithDaysReplacesPlanDays(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})
	a := model.Reseller{Name: "a", Enabled: true, ApiEnabled: true, ApiToken: "tok-a-0123456789"}
	db.Create(&a)
	s.setResellerLines(a.Id, []uint{1})
	db.Create(&model.Plan{Name: "月付", ResellerId: a.Id, VolumeGB: 10, Days: 30, LineIds: []byte(`[1]`)})
	expiry := time.Now().Unix() + 10*86400
	db.Create(&model.User{Name: "a-u", Enabled: true, ResellerId: a.Id, Expiry: expiry, SubToken: "uuuuuuuuuuuuuuuuuuuuuuuu"})

	req := httptest.NewRequest("PATCH", "http://rs.example:2054/dl/api/v1/users/a-u", strings.NewReader(`{"plan":"月付","days":60}`))
	req.Header.Set("Authorization", "Bearer tok-a-0123456789")
	w := httptest.NewRecorder()
	s.handleResellerPublicAPI(w, req)
	if w.Code != 200 {
		t.Fatalf("修改失败: %d %s", w.Code, w.Body.String())
	}
	var u model.User
	db.Where("name = ?", "a-u").First(&u)
	if want := expiry + 60*86400; u.Expiry != want {
		t.Fatalf("到期应为原到期 + 60 天(%d),实际 %d(差 %d 天)", want, u.Expiry, (u.Expiry-want)/86400)
	}
	if u.Volume != 10<<30 {
		t.Fatalf("套餐的配额照样套用: %d", u.Volume)
	}
}
