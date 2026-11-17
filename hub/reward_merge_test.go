package hub

import (
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 流量奖励:副机报上来的账本并进主机时,真实流量按原始字节的增量累计 —— 这台服务器倍率是 3 也不乘,
// 重复的报告不重计,面板内部的用户(线路作上游的专用凭据)不算,已经删掉的用户在途的流量照算(流量是真的)。
func TestMergeAddsRewardBytes(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "n2", Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, CreatedAt: 100})
	relay := model.RelayUserName(7)
	merge := func(at int64, cs ...model.AgentCounter) {
		t.Helper()
		if _, err := MergeCounters(db, 2, "n2", "", cs, at, 60, 3); err != nil {
			t.Fatal(err)
		}
	}
	merge(1000, model.AgentCounter{UserName: "alice", Up: 1000, Down: 5000}, model.AgentCounter{UserName: relay, Up: 300, Down: 300})
	if got := model.RewardBytes(db); got != 6000 {
		t.Fatalf("第一份从 0 计:应为 6000,实际 %d", got)
	}
	merge(1060, model.AgentCounter{UserName: "alice", Up: 1000, Down: 5000}) // 同一份再报一遍
	if got := model.RewardBytes(db); got != 6000 {
		t.Fatalf("重复的报告不重计:应为 6000,实际 %d", got)
	}
	merge(1120, model.AgentCounter{UserName: "alice", Up: 1500, Down: 7000}, model.AgentCounter{UserName: "ghost", Up: 10, Down: 20})
	if got := model.RewardBytes(db); got != 6000+2500+30 {
		t.Fatalf("按增量累计、删掉的用户在途流量照算:应为 8530,实际 %d", got)
	}
	var u model.User
	db.Where("name = ?", "alice").First(&u)
	if u.Up != 1500*3 || u.Down != 7000*3 {
		t.Fatalf("用户用量照旧按服务器倍率记: up=%d down=%d", u.Up, u.Down)
	}
}
