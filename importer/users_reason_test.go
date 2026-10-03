package importer

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// 只导用户:停用的要记上原因(以前原因为空,之后延期 / 清零时被当成自动停用而复活);旧库里启用的同名用户
// 把本站的停用原因一并清掉;代理名下的同名用户不碰(覆盖用量等于洗额度),计入跳过(审计 MB16)。
func TestImportUsersReasonAndResellerSkip(t *testing.T) {
	dir := t.TempDir()
	from := oldDB(t, dir)
	open := func(name string) *gorm.DB {
		db, err := database.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { database.Close(db) })
		return db
	}
	get := func(db *gorm.DB, name string) model.User {
		var u model.User
		db.Where("name = ?", name).First(&u)
		return u
	}

	fresh := open("fresh.db")
	if _, err := ImportUsersOnly(from, fresh, false); err != nil {
		t.Fatal(err)
	}
	if u := get(fresh, "paused"); u.Enabled || u.DisabledReason != model.DisabledManual {
		t.Fatalf("旧库停用(没超量没到期)应记为手动停用: %+v", u)
	}

	cur := open("cur.db")
	cur.Create(&model.User{Name: "alive", Enabled: true, ResellerId: 5, Up: 999, SubToken: "a1a1a1a1a1a1a1a1a1a1a1a1"})
	cur.Create(&model.User{Name: "paused", Enabled: true, SubToken: "p1p1p1p1p1p1p1p1p1p1p1p1"})
	sum, err := ImportUsersOnly(from, cur, false)
	if err != nil {
		t.Fatal(err)
	}
	if u := get(cur, "alive"); u.Up != 999 || len(sum.Skipped) != 1 || sum.Skipped[0] != "alive" {
		t.Fatalf("代理名下的同名用户应跳过且不动: %+v %+v", u, sum)
	}
	if u := get(cur, "paused"); u.Enabled || u.DisabledReason != model.DisabledManual {
		t.Fatalf("同名更新成停用也要记原因: %+v", u)
	}

	manual := open("manual.db")
	manual.Create(&model.User{Name: "alive", Enabled: true, SubToken: "m1m1m1m1m1m1m1m1m1m1m1m1"})
	manual.Model(&model.User{}).Where("name = ?", "alive").Updates(map[string]interface{}{"enabled": false, "disabled_reason": model.DisabledManual})
	if _, err := ImportUsersOnly(from, manual, false); err != nil {
		t.Fatal(err)
	}
	if u := get(manual, "alive"); !u.Enabled || u.DisabledReason != "" {
		t.Fatalf("旧库启用的同名用户启用后不该留着停用原因: %+v", u)
	}
}
