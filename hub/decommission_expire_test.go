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

// 服务器列表读不出来时(审查 F-C1):existingNodeIds 要把错误报出来,不能当成「一台都不在列表里」——
// 那样停用着(还在列表里)的那台超过 7 天的下线通知会被当成已删除的作废掉。
func TestExistingNodeIdsReportsReadError(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	db.Create(&model.Node{Name: "停用的", ApiUrl: "http://192.0.2.1:9", Enabled: true, Sort: 1})
	recs := []model.NodeDecommission{{NodeId: 1}, {NodeId: 7}}
	got, err := h.existingNodeIds(recs)
	if err != nil || !got[1] || got[7] {
		t.Fatalf("读得出来时:在列表里的那台为真、删掉的为假: %v %v", got, err)
	}
	database.Close(db) // 库关了:查询必然出错
	if _, err := h.existingNodeIds(recs); err == nil {
		t.Fatal("读不出来时应报错,不能当成一台都不在")
	}
}
