package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
)

// 关掉「自动生成默认策略组」而 Clash 模板(空 = 内置)还用着 Proxy / Auto:照常存下,回提示代码 clashGroupsRef,
// 设置页据此说明「订阅里仍会生成用到的那个」。只在这次动了开关或模板时看,别的设置保存时不打扰。
func TestSettingsHintClashGroupsRef(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	post := func(body interface{}) []interface{} {
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
		hints, _ := out["hints"].([]interface{})
		return hints
	}
	has := func(hints []interface{}) bool {
		for _, h := range hints {
			if h == "clashGroupsRef" {
				return true
			}
		}
		return false
	}
	// 模板没写(内置模板的规则是 MATCH,Proxy)时关掉开关:提示
	if h := post(map[string]string{"subClashDefaultGroups": "false"}); !has(h) {
		t.Fatalf("关掉开关、模板为空应提示: %v", h)
	}
	if s.setting("subClashDefaultGroups") != "false" {
		t.Fatalf("开关应照常存下: %q", s.setting("subClashDefaultGroups"))
	}
	// 开关关着,换成只用自己组的模板:不提示
	own := "rules:\n  - MATCH,我的组\nproxy-groups:\n  - name: 我的组\n    type: select\n    proxies: [DIRECT]\n"
	if h := post(map[string]string{"subClashExt": own}); has(h) {
		t.Fatalf("模板不用 Proxy / Auto 不该提示: %v", h)
	}
	// 开关关着,模板改成用 Auto(DNS 的 #出站也算):提示
	if h := post(map[string]string{"subClashExt": "dns:\n  enable: true\n  nameserver:\n    - https://1.1.1.1/dns-query#Auto\nrules:\n  - MATCH,DIRECT\n"}); !has(h) {
		t.Fatalf("模板在 DNS 里用着 Auto 应提示: %v", h)
	}
	// 没动开关和模板的保存:不提示
	if h := post(map[string]string{"timezone": "UTC"}); has(h) {
		t.Fatalf("没动开关和模板不该提示: %v", h)
	}
	// 开关打开:不提示
	if h := post(map[string]string{"subClashDefaultGroups": "true"}); has(h) {
		t.Fatalf("开关开着不该提示: %v", h)
	}
	// 同一次保存里既关开关又换成不用的模板:按这次的新值判断,不提示
	if h := post(map[string]string{"subClashDefaultGroups": "false", "subClashExt": own}); has(h) {
		t.Fatalf("按这次的新值判断,不该提示: %v", h)
	}
}
