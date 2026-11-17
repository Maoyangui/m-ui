package jobs

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

func localNode(t *testing.T, db *gorm.DB) model.Node {
	t.Helper()
	var n model.Node
	if db.Where("is_local = ?", true).Limit(1).Find(&n); n.Id == 0 {
		n = model.Node{Name: "本机", IsLocal: true, Enabled: true}
		if err := db.Create(&n).Error; err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// 服务器的流量额度按数据面的全部字节算:各条线路的入站加上经本机转的中转入口(中转不认用户,不在任何人的账本里),
// 真实字节、不乘倍率。主机把自己这台的累计并进它的本期用量:头一轮就计得进去(基线落在这一轮之前),之后按增量。
func TestNodeTotalCountsAllInboundBytesOnMaster(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "total.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	self := localNode(t, db)
	db.Create(&model.User{Name: "alice", Enabled: true})
	tr := core.NewStatsTracker()
	feed := func() {
		tr.RestoreStats([]model.Stats{
			{Resource: "user", Tag: "alice", Direction: false, Traffic: 1000},
			{Resource: "user", Tag: "alice", Direction: true, Traffic: 100},
			{Resource: "user", Tag: model.RelayUserName(3), Direction: false, Traffic: 540}, // 别的服务器把本机的线路当上游:也是经本机的流量
			{Resource: "inbound", Tag: "香港", Direction: false, Traffic: 1540},
			{Resource: "inbound", Tag: "香港", Direction: true, Traffic: 100},
			{Resource: "inbound", Tag: "日本 ⇄8443", Direction: false, Traffic: 800}, // 中转入口:没有用户
			{Resource: "inbound", Tag: "日本 ⇄8443", Direction: true, Traffic: 50},
			{Resource: "outbound", Tag: "direct", Direction: false, Traffic: 99999}, // 出站那一侧不重复算
		})
	}
	s := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(string) string { return "" }, LocalRatio: func() float64 { return 3 }})
	feed()
	if !s.runStatsTracker(tr) {
		t.Fatal("第 1 轮落库失败")
	}
	total, _ := model.ReadLocalTotal(db)
	if total.Up != 150 || total.Down != 2340 || total.LedgerId == 0 {
		t.Fatalf("本机累计应为 上 150 下 2340(不乘倍率),实际 %+v", total)
	}
	// 头一轮的流量就要计进去:打开库时本机那本账已经建好(全零),基线落在这一轮之前。
	// 以前头一轮整笔成了基线 —— 实机上升级后跑的第一段流量一个字节没记上
	var q model.NodeQuota
	db.Where("node_id = ?", self.Id).Limit(1).Find(&q)
	if q.NodeId == 0 || q.Up != 150 || q.Down != 2340 || !q.Tracked {
		t.Fatalf("头一轮就该计进去(上 150 下 2340): %+v", q)
	}
	feed()
	if !s.runStatsTracker(tr) {
		t.Fatal("第 2 轮落库失败")
	}
	db.Where("node_id = ?", self.Id).Limit(1).Find(&q)
	if q.Up != 300 || q.Down != 4680 {
		t.Fatalf("第二轮按增量:本期应为 上 300 下 4680,实际 上 %d 下 %d", q.Up, q.Down)
	}
	// 只有中转流量的一轮(没有任何用户的流量):照样记
	tr.RestoreStats([]model.Stats{{Resource: "inbound", Tag: "日本 ⇄8443", Direction: false, Traffic: 7000}})
	if !s.runStatsTracker(tr) {
		t.Fatal("只有中转流量的一轮落库失败")
	}
	db.Where("node_id = ?", self.Id).Limit(1).Find(&q)
	if q.Up != 300 || q.Down != 11680 {
		t.Fatalf("只有中转的一轮也要计:应为 上 300 下 11680,实际 上 %d 下 %d", q.Up, q.Down)
	}
	// 接任 / 设为主机之后:这台以前是副机,原主机并到过它的某个数(游标在库里),本机的累计比游标多出来的那一段
	// (原主机最后一次并账到接任之间的流量)在下一轮一起计进去,不丢、也不重复
	db.Model(&model.NodeQuota{}).Where("node_id = ?", self.Id).Updates(map[string]interface{}{"cur_up": 250, "cur_down": 11000, "up": 0, "down": 0})
	tr.RestoreStats([]model.Stats{{Resource: "inbound", Tag: "香港", Direction: false, Traffic: 20}})
	if !s.runStatsTracker(tr) {
		t.Fatal("接任后的一轮落库失败")
	}
	db.Where("node_id = ?", self.Id).Limit(1).Find(&q)
	if q.Up != 50 || q.Down != 700 {
		t.Fatalf("接任后:游标之后的那一段(上 50 下 680)加上这一轮(下 20)应为 上 50 下 700,实际 上 %d 下 %d", q.Up, q.Down)
	}
	// 用户的用量照旧(乘倍率),没有被这次改动带歪
	var alice model.User
	db.Where("name = ?", "alice").First(&alice)
	if alice.Down != 2*1000*3 || alice.Up != 2*100*3 {
		t.Fatalf("用户用量应按倍率照旧记:下 %d 上 %d", alice.Down, alice.Up)
	}
}

// 副机只在本机累计,不判、不并(主机拉报告时并);主机正在让位(这一刻已经是副机)的那一轮也按副机记。
func TestNodeTotalOnNodeOnlyAccumulates(t *testing.T) {
	for _, mode := range []string{"node", "handover"} {
		db, err := database.Open(filepath.Join(t.TempDir(), mode+".db"))
		if err != nil {
			t.Fatal(err)
		}
		localNode(t, db)
		isNode := mode == "node"
		if !isNode { // 进事务之后才发现本机已经变为副机
			db.Create(&model.Setting{Key: "nodeMode", Value: "true"})
		}
		tr := core.NewStatsTracker()
		tr.RestoreStats([]model.Stats{
			{Resource: "user", Tag: "alice", Direction: false, Traffic: 1000},
			{Resource: "inbound", Tag: "香港", Direction: false, Traffic: 1000},
			{Resource: "inbound", Tag: "日本 ⇄8443", Direction: true, Traffic: 30},
		})
		s := New(Deps{DB: db, IsNode: func() bool { return isNode }, Setting: func(string) string { return "" }})
		if !s.runStatsTracker(tr) {
			t.Fatalf("%s: 落库失败", mode)
		}
		total, _ := model.ReadLocalTotal(db)
		if total.Up != 30 || total.Down != 1000 {
			t.Fatalf("%s: 本机累计应为 上 30 下 1000,实际 %+v", mode, total)
		}
		var rows int64
		db.Model(&model.NodeQuota{}).Count(&rows)
		if rows != 0 {
			t.Fatalf("%s: 副机不并自己的用量,却有 %d 行", mode, rows)
		}
		var ledger model.AgentCounter
		db.Where("user_name = ?", "alice").First(&ledger)
		if ledger.Down != 1000 {
			t.Fatalf("%s: 账本照旧:alice 下行 %d", mode, ledger.Down)
		}
		database.Close(db)
	}
}

// 判定的钩子:并账之后的即时判定、每分钟那一轮各叫一次;副机不叫。
func TestNodeQuotaHookRuns(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "hook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	calls := 0
	s := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(string) string { return "" }, NodeQuota: func() { calls++ }})
	s.depleteNow()
	s.runDeplete()
	if calls != 2 {
		t.Fatalf("主机上应各叫一次,实际 %d 次", calls)
	}
	n := New(Deps{DB: db, IsNode: func() bool { return true }, Setting: func(string) string { return "" }, NodeQuota: func() { calls++ }})
	n.depleteNow()
	n.runDeplete()
	if calls != 2 {
		t.Fatalf("副机不判,却又叫了 %d 次", calls-2)
	}
	// 钩子里出了意外不能带垮定时任务
	p := New(Deps{DB: db, IsNode: func() bool { return false }, Setting: func(string) string { return "" }, NodeQuota: func() { panic("boom") }})
	p.depleteNow()
	p.runDeplete()
}
