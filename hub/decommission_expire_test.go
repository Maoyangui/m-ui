package hub

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 审计 A4:已删除的副机,下线通知删除 7 天后还没送到就不再重试(提示随之消失);停用着的那台一直重试。
func TestRetryDecommissionsGivesUpOnDeletedAfterAWeek(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	db.Create(&model.Node{Name: "停用的", ApiUrl: "http://192.0.2.1:9", Enabled: true, Sort: 1})
	db.Model(&model.Node{}).Where("id = ?", 1).Update("enabled", false)
	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	recent := time.Now().Add(-time.Hour).Unix()
	// LastTry 设成刚试过,退避期内这一轮不会真的去推(测试里不发网络请求)
	now := time.Now().Unix()
	db.Create(&model.NodeDecommission{NodeId: 1, Name: "停用的", ApiUrl: "http://192.0.2.1:9", Revision: "r", CreatedAt: old, Attempts: 5, LastTry: now})
	db.Create(&model.NodeDecommission{NodeId: 7, Name: "删了八天", ApiUrl: "http://192.0.2.2:9", Revision: "r", CreatedAt: old, Attempts: 5, LastTry: now})
	db.Create(&model.NodeDecommission{NodeId: 8, Name: "删了一小时", ApiUrl: "http://192.0.2.3:9", Revision: "r", CreatedAt: recent, Attempts: 5, LastTry: now})
	h.retryDecommissions()
	h.rounds.Wait()
	count := func(id uint) int64 {
		var n int64
		db.Model(&model.NodeDecommission{}).Where("node_id = ?", id).Count(&n)
		return n
	}
	if count(7) != 0 {
		t.Fatal("删除八天还没送到的应不再重试")
	}
	if count(8) != 1 {
		t.Fatal("刚删的应继续重试")
	}
	if count(1) != 1 {
		t.Fatal("停用着的那台应一直重试")
	}
}
