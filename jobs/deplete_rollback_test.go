package jobs

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// 配额判定事务失败回滚:这一轮代理到期的变化不能丢,通知也不能先发。以前事务里就把不可用集合写回内存、发了通知,
// 回滚后下一轮集合相同,数据面一直不撤这个代理的用户(审计 MB15)。
func TestDepleteRollbackKeepsResellerChange(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	now := time.Now().Unix()
	rs := model.Reseller{Name: "r", Enabled: true, Expiry: now + 3600}
	db.Create(&rs)
	db.Create(&model.User{Name: "u", Enabled: true, ResellerId: rs.Id})
	db.Create(&model.User{Name: "x", Enabled: true, Volume: 100})

	var fail bool
	db.Callback().Update().Before("gorm:update").Register("test:fail", func(d *gorm.DB) {
		// 只让第 2 步「停用用户」那次写库失败:前面的周期重置、代理判定都已经做完
		if m, ok := d.Statement.Dest.(map[string]interface{}); ok && fail && m["enabled"] == false {
			d.AddError(errors.New("模拟写库失败"))
		}
	})
	var mu sync.Mutex
	var notes []string
	reloads := 0
	s := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(string) string { return "" },
		ReloadUsers: func() error { reloads++; return nil },
		Notify:      func(n string) { mu.Lock(); notes = append(notes, n); mu.Unlock() }})
	s.runDeplete() // 第一轮:建立不可用集合(空)

	db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("expiry", now-1)
	db.Model(&model.User{}).Where("name = ?", "x").Update("up", 100) // 第 2 步要停用 x,让它写库失败
	fail = true
	s.runDeplete()
	fail = false
	if len(notes) != 0 || reloads != 0 {
		t.Fatalf("事务回滚了就不该发通知或重载: notes=%v reloads=%d", notes, reloads)
	}
	s.runDeplete()
	joined := strings.Join(notes, "|")
	if reloads != 1 || !strings.Contains(joined, "代理已到期") || !strings.Contains(joined, "用户已禁用") {
		t.Fatalf("下一轮应补上代理到期与停用并重载: notes=%v reloads=%d", notes, reloads)
	}
}
