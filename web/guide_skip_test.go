package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/hub"
)

// 概览「开始使用」的「跳过」(用户 11-23):走普通的设置接口存进面板(guideSkipped,按开关规整成 true / false),
// 读设置时浏览器拿得到;是管理员的偏好 —— 接任后的新主机照旧(容灾的全局设置里有它),不推给副机(副机本来就不出指引)。
func TestGuideSkippedSetting(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	post := func(body map[string]string) int {
		t.Helper()
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		s.handleSettings(w, httptest.NewRequest("POST", "http://x/app/api/settings", strings.NewReader(string(b))))
		return w.Code
	}
	read := func() map[string]interface{} {
		w := httptest.NewRecorder()
		s.handleSettings(w, httptest.NewRequest("GET", "http://x/app/api/settings", nil))
		var got map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return got
	}
	if code := post(map[string]string{"guideSkipped": "1"}); code != 200 {
		t.Fatalf("存「跳过」应成功: %d", code)
	}
	if got := s.setting("guideSkipped"); got != "true" {
		t.Fatalf("应规整成 true,实际 %q", got)
	}
	if read()["guideSkipped"] != "true" {
		t.Fatalf("读设置应带上 guideSkipped: %v", read()["guideSkipped"])
	}
	if code := post(map[string]string{"guideSkipped": "maybe"}); code != 400 {
		t.Fatalf("不是开关值应拒绝: %d", code)
	}
	if code := post(map[string]string{"guideSkipped": "false"}); code != 200 || s.setting("guideSkipped") != "false" {
		t.Fatalf("改回来应生效: %d %q", code, s.setting("guideSkipped"))
	}
	if !ha.IsGlobal("guideSkipped") {
		t.Fatal("跳过指引是管理员的偏好,接任后的新主机要照旧")
	}
	for _, k := range hub.SyncedSettings {
		if k == "guideSkipped" {
			t.Fatal("不该推给副机(副机本来就不出指引)")
		}
	}
}
