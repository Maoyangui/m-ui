package hub

import (
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 副机报上来已删用户的在途流量:不记用户时序(审计 MB28),服务器维度照记。
func TestApplyCountersSkipsDeletedUserStats(t *testing.T) {
	db := openDB(t, "orphan.db").DB
	ApplyCounters(db, 2, "台湾", "", []model.AgentCounter{{UserName: "gone", Up: 100, Down: 200}}, 1000, 60, 1)
	var user, node int64
	db.Model(&model.Stats{}).Where("resource = ?", "user").Count(&user)
	db.Model(&model.Stats{}).Where("resource = ? AND traffic > 0", "node").Count(&node)
	if user != 0 || node != 2 {
		t.Fatalf("已删用户不记用户时序、服务器维度照记: user=%d node=%d", user, node)
	}
}
