package jobs

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 已删掉的用户还有在途流量:不能落成孤儿时序(以后同名的新用户会看到前人的历史),计数器照样扣掉(审计 MB28)。
func TestStatsSkipsDeletedUsers(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "orphan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.User{Name: "alice", Enabled: true})
	tr := core.NewStatsTracker()
	tr.RestoreStats([]model.Stats{
		{Resource: "user", Tag: "alice", Direction: false, Traffic: 10},
		{Resource: "user", Tag: "gone", Direction: false, Traffic: 99},
		{Resource: "inbound", Tag: "in", Direction: false, Traffic: 109},
	})
	s := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(string) string { return "" }})
	if !s.runStatsTracker(tr) {
		t.Fatal("落库失败")
	}
	var gone, alice, in int64
	db.Model(&model.Stats{}).Where("resource = ? AND tag = ?", "user", "gone").Count(&gone)
	db.Model(&model.Stats{}).Where("resource = ? AND tag = ?", "user", "alice").Count(&alice)
	db.Model(&model.Stats{}).Where("resource = ?", "inbound").Count(&in)
	if gone != 0 || alice != 1 || in != 1 {
		t.Fatalf("已删用户不该有时序,其它照记: gone=%d alice=%d inbound=%d", gone, alice, in)
	}
	if left := tr.SnapshotStats(); len(*left) != 0 {
		t.Fatalf("计数器应全部扣掉: %+v", *left)
	}
}
