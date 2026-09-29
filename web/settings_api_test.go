package web

import (
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 设置接口:密钥不发给浏览器(要显示"已填"的只发掩码),只收设置页上的键 ——
// 不能借它关掉两步验证、换外部 API 令牌、改配对令牌。
func TestSettingsAPIHidesSecretsAndRejectsManagedKeys(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	for k, v := range map[string]string{"totpEnabled": "true", "totpSecret": "JBSWY3DPEHPK3PXP", "apiToken": "api-secret",
		"nodeToken": "node-secret", "acmeCfToken": "cf-secret", "acmeAccountKey": "acct-secret", "tgToken": "123:tg-secret", "timezone": "UTC"} {
		db.Create(&model.Setting{Key: k, Value: v})
	}
	call := func(method, body string) (int, string) {
		r := httptest.NewRequest(method, "http://x/app/api/settings", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleSettings(w, r)
		return w.Code, w.Body.String()
	}

	code, body := call("GET", "")
	var got map[string]string
	if code != 200 || json.Unmarshal([]byte(body), &got) != nil {
		t.Fatalf("GET 失败: %d %s", code, body)
	}
	for _, secret := range []string{"JBSWY3DPEHPK3PXP", "api-secret", "node-secret", "cf-secret", "acct-secret", "tg-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("设置接口不该返回密钥 %q: %s", secret, body)
		}
	}
	if got["tgToken"] != secretMask || got["reachToken"] != "" || got["timezone"] != "UTC" || got["totpEnabled"] != "true" {
		t.Fatalf("已填的 tgToken 发掩码、没填的不发、普通设置照常: %+v", got)
	}

	for _, bad := range []string{`{"totpEnabled":"false"}`, `{"totpSecret":""}`, `{"apiToken":"123","apiEnabled":"true"}`, `{"nodeToken":"x"}`} {
		if code, _ := call("POST", bad); code != 400 {
			t.Fatalf("%s 应被拒,得 %d", bad, code)
		}
	}
	if s.setting("totpEnabled") != "true" || s.setting("apiToken") != "api-secret" {
		t.Fatal("被拒的请求不该改动任何设置")
	}

	// 掩码原样交回 = 不改;清空 = 删除;新值照写
	if code, body := call("POST", `{"tgToken":"`+secretMask+`","tgChatId":"42"}`); code != 200 {
		t.Fatalf("保存通知分组失败: %d %s", code, body)
	}
	if s.setting("tgToken") != "123:tg-secret" || s.setting("tgChatId") != "42" {
		t.Fatalf("掩码应表示不改: tgToken=%q", s.setting("tgToken"))
	}
	call("POST", `{"tgToken":"456:new"}`)
	if s.setting("tgToken") != "456:new" {
		t.Fatal("填了新值应写入")
	}
	call("POST", `{"tgToken":""}`)
	if s.setting("tgToken") != "" {
		t.Fatal("清空应删除")
	}
}

// 前端会提交的每个设置键都要在白名单里,否则那个分组保存就会报错。
func TestPanelSettingsCoverFrontend(t *testing.T) {
	read := func(p string) string {
		b, err := fs.ReadFile(assets, "assets/js/pages/"+p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var keys []string
	for _, m := range regexp.MustCompile(`\['([A-Za-z0-9]+)', t\('[^']+'\), '[a-zA-Z]+'`).FindAllStringSubmatch(read("settings.js"), -1) {
		keys = append(keys, m[1])
	}
	if len(keys) < 50 {
		t.Fatalf("没从设置页解析出字段(%d 个),正则需要更新", len(keys))
	}
	keys = append(keys, "nodeMode")
	for _, m := range regexp.MustCompile(`post\('settings', \{([^}]*)\}`).FindAllStringSubmatch(read("backup.js"), -1) {
		for _, k := range regexp.MustCompile(`([A-Za-z0-9]+):`).FindAllStringSubmatch(m[1], -1) {
			keys = append(keys, k[1])
		}
	}
	for _, m := range regexp.MustCompile(`'([A-Za-z0-9]+Age)'`).FindAllStringSubmatch(read("logs.js"), -1) {
		keys = append(keys, m[1])
	}
	for _, k := range keys {
		if !panelSettings[k] {
			t.Errorf("前端会提交 %q,但设置接口不收", k)
		}
	}
}

// 两步验证已开启时不能经 setup / enable 直接换密钥(那等于绕过"关闭要密码 + 验证码")。
func TestAdminTotpCannotBeReplacedWhileEnabled(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Setting{Key: "totpEnabled", Value: "true"})
	db.Create(&model.Setting{Key: "totpSecret", Value: "JBSWY3DPEHPK3PXP"})
	for _, p := range []string{"totp/setup", "totp/enable"} {
		r := httptest.NewRequest("POST", "http://x/app/api/admin/"+p, strings.NewReader(`{"secret":"KRSXG5CTMVRXEZLU"}`))
		w := httptest.NewRecorder()
		s.handleAdmin(w, r)
		if w.Code != 409 {
			t.Fatalf("%s 在已开启时应被拒,得 %d", p, w.Code)
		}
	}
	if s.setting("totpSecret") != "JBSWY3DPEHPK3PXP" {
		t.Fatal("密钥不该被换掉")
	}
}
