package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 延期:不限期的不动(以前批量延期 30 天把它们改成 30 天后到期);到期被停的自动恢复;手动停用的仍停着
// (单个「延长 30 天」以前带 enabled:true,把手动停用的也启用了,现在同走这里,审计 MB11)。
func TestExtendKeepsUnlimitedAndManualDisable(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	now := time.Now().Unix()
	db.Create(&model.User{Name: "forever", Enabled: true, SubToken: "t1t1t1t1t1t1t1t1t1t1t1t1"})
	db.Create(&model.User{Name: "expired", Enabled: true, Expiry: now - 86400, SubToken: "t2t2t2t2t2t2t2t2t2t2t2t2"})
	db.Create(&model.User{Name: "manual", Enabled: true, Expiry: now + 86400, SubToken: "t3t3t3t3t3t3t3t3t3t3t3t3"})
	db.Model(&model.User{}).Where("name = ?", "expired").Updates(map[string]interface{}{"enabled": false, "disabled_reason": model.DisabledExpired})
	db.Model(&model.User{}).Where("name = ?", "manual").Updates(map[string]interface{}{"enabled": false, "disabled_reason": model.DisabledManual})

	w := httptest.NewRecorder()
	s.handleUsersBatch(w, httptest.NewRequest("POST", "http://x/app/api/users/batch", strings.NewReader(`{"ids":[1,2,3],"action":"extend","days":30}`)))
	if w.Code != 200 {
		t.Fatalf("批量延期失败: %d %s", w.Code, w.Body.String())
	}
	var res map[string]int64
	json.Unmarshal(w.Body.Bytes(), &res)
	if res["affected"] != 2 {
		t.Fatalf("不限期的不算在内,应影响 2 个,实际 %d", res["affected"])
	}
	get := func(name string) model.User {
		var u model.User
		db.Where("name = ?", name).First(&u)
		return u
	}
	if u := get("forever"); u.Expiry != 0 {
		t.Fatalf("不限期的被设了到期: %d", u.Expiry)
	}
	if u := get("expired"); !u.Enabled || u.Expiry < now+29*86400 {
		t.Fatalf("到期被停的延期后应自动恢复: %+v", u)
	}
	if u := get("manual"); u.Enabled || u.DisabledReason != model.DisabledManual || u.Expiry != now+31*86400 {
		t.Fatalf("手动停用的应只顺延到期、仍停着: %+v", u)
	}
}
