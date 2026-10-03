package jobs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 用量并进来就判超量 / 到期,不再等每分钟一轮(以前超量后最长约 80 秒才断,审计 MB10)。
// 没有该停的人时不重载数据面。
func TestKickDepleteDisablesWithoutWaitingAMinute(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.User{Name: "ok", Enabled: true, Volume: 1000, Up: 1})
	db.Create(&model.User{Name: "over", Enabled: true, Volume: 1000, Up: 600})
	reloads := make(chan struct{}, 10)
	s := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(string) string { return "" },
		ReloadUsers: func() error { reloads <- struct{}{}; return nil }})
	s.Start()
	defer s.Stop()

	s.KickDeplete()
	select {
	case <-reloads:
		t.Fatal("没人超量时不该重载")
	case <-time.After(300 * time.Millisecond):
	}

	db.Model(&model.User{}).Where("name = ?", "over").Update("down", 400) // 刚好用满
	db.Model(&model.User{}).Where("name = ?", "ok").Update("expiry", time.Now().Unix()-1)
	s.KickDeplete()
	select {
	case <-reloads:
	case <-time.After(3 * time.Second):
		t.Fatal("触发后应立刻判定并重载,不该等每分钟那一轮")
	}
	var over, ok model.User
	db.Where("name = ?", "over").First(&over)
	db.Where("name = ?", "ok").First(&ok)
	if over.Enabled || over.DisabledReason != model.DisabledQuota {
		t.Fatalf("超量应停用并记 quota: %+v", over)
	}
	if ok.Enabled || ok.DisabledReason != model.DisabledExpired {
		t.Fatalf("到期应停用并记 expired: %+v", ok)
	}
}
