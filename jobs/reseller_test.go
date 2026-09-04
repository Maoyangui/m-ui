package jobs

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 代理额度按"名下用户用量之和"判定:超了就把这个代理的用户全部停掉,
// 没超的代理与主面板直属用户一律不受影响。
func TestResellerQuotaDisablesItsUsers(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)

	db.Create(&model.Reseller{Name: "over", Enabled: true, Volume: 10 << 30})
	db.Create(&model.Reseller{Name: "under", Enabled: true, Volume: 10 << 30})
	db.Create(&model.User{Name: "a1", Enabled: true, ResellerId: 1, Up: 6 << 30})
	db.Create(&model.User{Name: "a2", Enabled: true, ResellerId: 1, Down: 5 << 30}) // a1+a2 = 11G > 10G
	db.Create(&model.User{Name: "b1", Enabled: true, ResellerId: 2, Up: 1 << 30})
	db.Create(&model.User{Name: "direct", Enabled: true, Up: 100 << 30})

	reloaded := false
	s := New(Deps{
		DB:          db,
		IsNode:      func() bool { return false },
		ReloadUsers: func() error { reloaded = true; return nil },
	})
	s.runDeplete()

	enabled := func(name string) bool {
		var u model.User
		db.Where("name = ?", name).First(&u)
		return u.Enabled
	}
	if enabled("a1") || enabled("a2") {
		t.Fatal("超额代理的用户应全部停用")
	}
	if !enabled("b1") {
		t.Fatal("未超额的代理不该受影响")
	}
	if !enabled("direct") {
		t.Fatal("主面板直属用户不该被代理额度影响")
	}
	if !reloaded {
		t.Fatal("停用后应热更新数据面")
	}
}
