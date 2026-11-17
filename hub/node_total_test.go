package hub

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 副机报告里带着它数据面的累计:主机并进这台服务器的本期用量(只计增量),有新增就叫一次即时判定;
// 老副机不带(Total 为空)什么都不记。
func TestReportTotalMergesIntoNodeUsage(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	n2 := model.Node{Name: "n2", ApiUrl: "http://n2", Token: "t", Enabled: true}
	db.Create(&n2)
	old := model.Node{Name: "老副机", ApiUrl: "http://old", Token: "t", Enabled: true}
	db.Create(&old)
	kicks := 0
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }, CountersMerged: func() { kicks++ }})
	report := func(n model.Node, total *model.NodeTotal) {
		t.Helper()
		h.applyResult(&nodeResult{n: n, rep: Report{Version: "0.8.0", Total: total}})
	}
	used := func(id uint) (int64, bool) {
		var q model.NodeQuota
		db.Where("node_id = ?", id).Limit(1).Find(&q)
		return q.Used(), q.NodeId != 0
	}
	report(n2, &model.NodeTotal{Up: 5000, Down: 90000, LedgerId: 42})
	if u, ok := used(n2.Id); !ok || u != 0 || kicks != 0 {
		t.Fatalf("第一份只建基线、不触发判定: 用量 %d 有记录 %v 触发 %d 次", u, ok, kicks)
	}
	report(n2, &model.NodeTotal{Up: 5600, Down: 91000, LedgerId: 42})
	if u, _ := used(n2.Id); u != 1600 || kicks != 1 {
		t.Fatalf("增量 1600 并进去并触发一次判定: 用量 %d 触发 %d 次", u, kicks)
	}
	report(n2, &model.NodeTotal{Up: 5600, Down: 91000, LedgerId: 42})
	if u, _ := used(n2.Id); u != 1600 || kicks != 1 {
		t.Fatalf("没有新增不重计、不触发: 用量 %d 触发 %d 次", u, kicks)
	}
	report(old, nil) // 老副机不报累计
	if _, ok := used(old.Id); ok {
		t.Fatal("不报累计的服务器不该有用量记录")
	}
	// 在线状态照旧记下
	if st, ok := h.Statuses()[n2.Id]; !ok || !st.OK {
		t.Fatalf("状态没记下: %+v", st)
	}
}

// 副机应用快照:自己被停止服务 / 恢复服务(流量额度用完、到期)要全量重载 —— 用户全撤、经本机的中转入口要关;
// 别的服务器的这个标记变了不用重载(本机的入站不因它变)。标记要原样落进副机的库。
func TestApplySnapshotReloadsWhenSelfIsHeld(t *testing.T) {
	master, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(master)
	node, err := database.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(node)
	setting := func(string) string { return "" }
	master.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	master.Create(&model.Node{Name: "副机", ApiUrl: "http://n", Token: "t", Enabled: true, Sort: 2})
	master.Create(&model.Node{Name: "另一台", ApiUrl: "http://o", Token: "t", Enabled: true, Sort: 3})
	master.Create(&model.Line{Name: "l1", Protocol: "hysteria2", Port: 30443, Enabled: true})
	apply := func() bool {
		t.Helper()
		snap, err := BuildSnapshot(master, setting)
		if err != nil {
			t.Fatal(err)
		}
		snap.SelfNodeId = 2
		lc, _, err := ApplySnapshot(node, snap)
		if err != nil {
			t.Fatal(err)
		}
		return lc
	}
	held := func(id uint) bool {
		var n model.Node
		node.First(&n, id)
		return n.Suspended
	}
	revision := func() string {
		snap, err := BuildSnapshot(master, setting)
		if err != nil {
			t.Fatal(err)
		}
		return snap.Revision
	}
	if !apply() {
		t.Fatal("首次应用应报线路变了")
	}
	if apply() {
		t.Fatal("原样再应用不该报变化")
	}
	base := revision()
	// 别的服务器被停止服务:修订号要变(各台的订阅里要去掉它的节点),但本机不用全量重载
	master.Model(&model.Node{}).Where("id = ?", 3).Update("suspended", true)
	if revision() == base {
		t.Fatal("停止服务标记变了,修订号要跟着变,否则推不到副机")
	}
	if apply() {
		t.Fatal("别的服务器被停止服务:本机不该全量重载")
	}
	if !held(3) || held(2) {
		t.Fatalf("标记要原样落库: 另一台 %v 本机 %v", held(3), held(2))
	}
	// 本机被停止服务:全量重载
	master.Model(&model.Node{}).Where("id = ?", 2).Update("suspended", true)
	if !apply() {
		t.Fatal("本机被停止服务:应全量重载(用户全撤、中转入口要关)")
	}
	if !held(2) {
		t.Fatal("本机的停止服务标记没落库")
	}
	if apply() {
		t.Fatal("标记没再变,不该再重载")
	}
	// 恢复:同样全量重载
	master.Model(&model.Node{}).Where("id = ?", 2).Update("suspended", false)
	if !apply() || held(2) {
		t.Fatalf("恢复服务:应全量重载并清掉标记(标记 %v)", held(2))
	}
	// 没有额度的集群:快照里不带这个字段,修订号和加这个功能之前一样(升级后不必整份重推)
	master.Model(&model.Node{}).Where("id = ?", 3).Update("suspended", false)
	if revision() != base {
		t.Fatal("标记都清掉之后修订号应回到原来的值")
	}
}
