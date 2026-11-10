package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
)

// 保存写了 ipv6: false 的 Clash 模板:照常存下,但回一个提示代码,设置页据此说明"有 IPv6 节点时会改成 true"。
func TestSettingsHintClashIPv6False(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	post := func(body interface{}) map[string]interface{} {
		t.Helper()
		b, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "http://x/app/api/settings", strings.NewReader(string(b)))
		w := httptest.NewRecorder()
		s.handleSettings(w, r)
		if w.Code != 200 {
			t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
		}
		var out map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	tmpl := "mixed-port: 7890\nipv6: false\nrules:\n  - MATCH,Proxy\n"
	out := post(map[string]string{"subClashExt": tmpl})
	hints, _ := out["hints"].([]interface{})
	if len(hints) != 1 || hints[0] != "clashIPv6" {
		t.Fatalf("应带提示代码: %v", out)
	}
	if s.setting("subClashExt") != strings.TrimSpace(tmpl) {
		t.Fatalf("模板应照常存下: %q", s.setting("subClashExt"))
	}
	if out := post(map[string]string{"subClashExt": "ipv6: true\n"}); out["hints"] != nil {
		t.Fatalf("没写 false 不提示: %v", out)
	}
	if out := post(map[string]string{"timezone": "UTC"}); out["hints"] != nil {
		t.Fatalf("没改模板不提示: %v", out)
	}
}
