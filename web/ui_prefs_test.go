package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
)

// 用户 11-21:侧栏里隐藏的栏目(设置 uiNavHidden)写法同 attnMute;「设置」不能隐藏,存的时候就去掉。
func TestNormNavHidden(t *testing.T) {
	for in, want := range map[string]string{
		"":                                   "",
		"settings":                           "",
		"exts,rules":                         "exts,rules",
		" Exts , exts ,SETTINGS,,upstreams ": "exts,upstreams",
		"lines,<script>,a b,settings,fw":     "lines,fw",
	} {
		if got := normNavHidden(in); got != want {
			t.Fatalf("normNavHidden(%q) = %q,应为 %q", in, got, want)
		}
	}
}

// 界面的两个设置走普通的设置接口:简洁模式按开关规整成 true / false,隐藏的栏目规整后存下。
func TestUIPrefsSaved(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	post := func(body map[string]string) {
		t.Helper()
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		s.handleSettings(w, httptest.NewRequest("POST", "http://x/app/api/settings", strings.NewReader(string(b))))
		if w.Code != 200 {
			t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
		}
	}
	post(map[string]string{"uiConcise": "1", "uiNavHidden": "Exts,settings,rules,exts"})
	if got := s.setting("uiConcise"); got != "true" {
		t.Fatalf("uiConcise 应规整成 true,实际 %q", got)
	}
	if got := s.setting("uiNavHidden"); got != "exts,rules" {
		t.Fatalf("uiNavHidden 应为 exts,rules,实际 %q", got)
	}
	post(map[string]string{"uiConcise": "false", "uiNavHidden": ""})
	if s.setting("uiConcise") != "false" || s.setting("uiNavHidden") != "" {
		t.Fatalf("改回来应生效: %q %q", s.setting("uiConcise"), s.setting("uiNavHidden"))
	}
	// 读设置:浏览器拿得到这两项(前端靠它们画侧栏、切简洁模式)
	post(map[string]string{"uiNavHidden": "fw"})
	w := httptest.NewRecorder()
	s.handleSettings(w, httptest.NewRequest("GET", "http://x/app/api/settings", nil))
	var got map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["uiNavHidden"] != "fw" || got["uiConcise"] != "false" {
		t.Fatalf("读设置应带上这两项: uiNavHidden=%v uiConcise=%v", got["uiNavHidden"], got["uiConcise"])
	}
}
