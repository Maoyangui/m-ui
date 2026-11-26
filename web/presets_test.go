package web

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/sub"
)

// 订阅模板的现成写法:
//   - 接口(GET sub-templates)给出全部正文,老的 clash / singbox 两项还在、还是内置模板;
//   - 设置页选择框里列的(settings.js 的 TMPL_IDS)和后端的一套不多、一套不少、顺序一样,每套的名字和介绍中英文都有;
//   - 演示站用的那份(site/demo/sub-templates.json)和接口现在给的一字不差 —— 演示里选一套,填进去的就是真面板会填的。
//     改了模板以后跑 UPDATE_DEMO_FIXTURE=1 go test ./web -run TestSubTemplatePresets 重写它。
func TestSubTemplatePresets(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleSubTemplates(w, httptest.NewRequest("GET", "http://x/app/api/sub-templates", nil))
	var got struct {
		Clash   string       `json:"clash"`
		Singbox string       `json:"singbox"`
		Presets []sub.Preset `json:"presets"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("接口: %d %v", w.Code, err)
	}
	if got.Clash != sub.DefaultClashTemplate() || got.Singbox != sub.DefaultSingBoxTemplate() {
		t.Fatal("clash / singbox 两项应还是内置模板")
	}
	want := sub.Presets()
	if len(got.Presets) != len(want) {
		t.Fatalf("接口给了 %d 套,应有 %d 套", len(got.Presets), len(want))
	}
	byKind := map[string][]string{}
	for i, p := range got.Presets {
		if p != want[i] {
			t.Errorf("第 %d 套和 sub.Presets 不一样", i+1)
		}
		byKind[p.Kind] = append(byKind[p.Kind], p.ID)
	}

	// 前端的选择框
	js, err := os.ReadFile("assets/js/pages/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const TMPL_IDS = \{ clash: \[([^\]]*)\], singbox: \[([^\]]*)\] \};`).FindStringSubmatch(string(js))
	if m == nil {
		t.Fatal("settings.js 里找不到 TMPL_IDS(写法变了?)")
	}
	ids := func(s string) string {
		var out []string
		for _, x := range strings.Split(s, ",") {
			out = append(out, strings.Trim(strings.TrimSpace(x), "'"))
		}
		return strings.Join(out, ",")
	}
	if a, b := ids(m[1]), strings.Join(byKind["clash"], ","); a != b {
		t.Errorf("Clash 的现成模板:前端列的是 [%s],后端是 [%s]", a, b)
	}
	if a, b := ids(m[2]), strings.Join(byKind["singbox"], ","); a != b {
		t.Errorf("sing-box 的现成模板:前端列的是 [%s],后端是 [%s]", a, b)
	}
	i18n, err := os.ReadFile("assets/js/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range want {
		for _, key := range []string{"'set.tmpl." + p.ID + "': '", "'set.tmpl." + p.ID + ".d': '"} {
			if n := strings.Count(string(i18n), key); n != 2 {
				t.Errorf("文案 %s 应中英文各一条,实际 %d 条", key, n)
			}
		}
	}

	// 演示站的那份
	const fixture = "../site/demo/sub-templates.json"
	fresh, _ := json.MarshalIndent(map[string]interface{}{"clash": got.Clash, "singbox": got.Singbox, "presets": got.Presets}, "", " ")
	fresh = append(fresh, '\n')
	if os.Getenv("UPDATE_DEMO_FIXTURE") != "" {
		if err := os.WriteFile(fixture, fresh, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("已重写 %s", fixture)
		return
	}
	old, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("演示站的现成模板文件读不到(UPDATE_DEMO_FIXTURE=1 生成): %v", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(old, []byte("\r\n"), []byte("\n")), fresh) {
		t.Fatalf("%s 和接口现在给的不一样:模板改过的话,跑 UPDATE_DEMO_FIXTURE=1 go test ./web -run TestSubTemplatePresets 重写它", fixture)
	}
}
