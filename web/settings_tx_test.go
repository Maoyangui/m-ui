package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 审计 L2:保存设置以前逐项写库、不查错,有一项写不进时其余照存,回复还是「成功」。
// 现在整笔在一个事务里:任何一项写不进就一项都不存,回错误;故障排除后照常保存。
func TestSettingsSaveIsAllOrNothing(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Setting{Key: "subProfileTitle", Value: "old"})
	snapshot := func() map[string]string {
		var rows []model.Setting
		db.Find(&rows)
		m := map[string]string{}
		for _, r := range rows {
			m[r.Key] = r.Value
		}
		return m
	}
	before := snapshot()
	// 让 timezone 这一项写不进(插入、更新都拦),模拟库被锁太久 / 磁盘满时某一项写失败
	for _, q := range []string{
		`CREATE TRIGGER fail_tz_i BEFORE INSERT ON settings WHEN NEW.key = 'timezone' BEGIN SELECT RAISE(ABORT, 'disk full'); END`,
		`CREATE TRIGGER fail_tz_u BEFORE UPDATE ON settings WHEN NEW.key = 'timezone' BEGIN SELECT RAISE(ABORT, 'disk full'); END`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	body := `{"subProfileTitle":"new","subUpdates":"6","subPageTitle":"T","subPageBrand":"B","tgChatId":"42","timezone":"Asia/Tokyo"}`
	post := func() (int, string) {
		r := httptest.NewRequest("POST", "http://x/app/api/settings", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleSettings(w, r)
		return w.Code, w.Body.String()
	}
	// map 的遍历顺序是随机的:多跑几遍,覆盖「写失败的那项之前已经写了别的项」的情形
	for i := 0; i < 20; i++ {
		code, resp := post()
		if code != 400 || !strings.Contains(resp, "一项都没存上") {
			t.Fatalf("有一项写不进时应回错误,得 %d %s", code, resp)
		}
		after := snapshot()
		if len(after) != len(before) {
			t.Fatalf("写失败时不该多出设置项: %v → %v", before, after)
		}
		for k, v := range before {
			if after[k] != v {
				t.Fatalf("写失败时 %s 不该被改:%q → %q", k, v, after[k])
			}
		}
	}
	// 故障排除后照常保存
	db.Exec(`DROP TRIGGER fail_tz_i`)
	db.Exec(`DROP TRIGGER fail_tz_u`)
	if code, resp := post(); code != 200 {
		t.Fatalf("故障排除后应能保存: %d %s", code, resp)
	}
	got := snapshot()
	for k, v := range map[string]string{"subProfileTitle": "new", "subUpdates": "6", "subPageTitle": "T", "subPageBrand": "B", "tgChatId": "42", "timezone": "Asia/Tokyo"} {
		if got[k] != v {
			t.Fatalf("%s 应为 %q,得 %q", k, v, got[k])
		}
	}
}
