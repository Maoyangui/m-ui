package ha

import (
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
)

// 两台同时当主机(AWS 演练里真遇到的网络分区):新主机 B(纪元 2)联系不上副机 C,原主机 A(纪元 1)回来后
// 一直回话说自己是主机。B 不会自动让位(拿不到 C 还跟着 A 的证据),A 也不会变为副机(C 没跟 B)。
// 持续一阵后两边都看得见(Rivals)、告警一次;B 可以手动让回(YieldTo):变为副机、记下是让出。
func TestRivalMastersVisibleAndYield(t *testing.T) {
	b := testService(t, false, map[string]string{KeyEpoch: "2", KeyMode: ModeConfirm})
	restarted := 0
	b.d.Restart = func(time.Duration) { restarted++ }
	var sent []string
	b.d.Notify = func(_, m string) { sent = append(sent, m) }
	clk := &testClock{t: time.Now()}
	b.SetNow(clk.now)
	b.d.DB.Create(&model.Node{Name: "B", IsLocal: true, Enabled: true})
	a := model.Node{Name: "A", ApiUrl: "https://a/app/", Token: "t", Enabled: true}
	c := model.Node{Name: "C", ApiUrl: "https://c/app/", Token: "t", Enabled: true}
	b.d.DB.Create(&a)
	b.d.DB.Create(&c)
	round := func() {
		b.NoteReply(a, hub.EpochReply{Aware: true, Code: CodeFollowing, Role: "master", Epoch: 1, MasterId: a.Id})
		b.NoteReply(c, hub.EpochReply{Down: true})
	}
	round()
	if len(b.Rivals()) != 0 {
		t.Fatal("刚出现:可能是正常换主机的那一会儿,先不提示")
	}
	for i := 0; i < 7; i++ { // 3.5 分钟
		clk.add(30 * time.Second)
		round()
	}
	rs := b.Rivals()
	if len(rs) != 1 || rs[0].Id != a.Id || rs[0].Name != "A" || rs[0].Epoch != 1 || rs[0].Higher || !rs[0].CanYield || rs[0].Followers != 0 || rs[0].Others != 1 {
		t.Fatalf("持续一阵后看得见:A(纪元 1)也在当主机,本机可以让回: %+v", rs)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "两台同时当主机") || !strings.Contains(sent[0], "m-ui ha yield") {
		t.Fatalf("告警一次,写清怎么处理: %q", sent)
	}
	for i := 0; i < 4; i++ {
		clk.add(30 * time.Second)
		round()
	}
	if len(sent) != 1 {
		t.Fatal("同一台同一纪元只告一次")
	}
	if sm := b.Summary(); len(sm.Rivals) != 1 {
		t.Fatalf("页面顶部的小结里也有: %+v", sm.Rivals)
	}
	if b.IsNode() {
		t.Fatal("不自动让位(没有副机跟着原主机的证据)")
	}
	if err := b.YieldTo(c.Id, "test"); err == nil {
		t.Fatal("没在当主机的那台不能让给它")
	}
	if err := b.YieldTo(0, "test"); err != nil {
		t.Fatal(err)
	}
	var d Demoted
	if !b.IsNode() || restarted != 1 || !b.getJSON(KeyDemoted, &d) || !d.Yield || d.Epoch != 1 || d.MasterId != a.Id {
		t.Fatalf("让回:变为副机、记下是让给 A(纪元 1): node=%v restarted=%d %+v", b.IsNode(), restarted, d)
	}

	// 原主机那一边(纪元 1)看到的是纪元更高的 B:提示,但不能由它来让
	o := testService(t, false, map[string]string{KeyEpoch: "1", KeyMode: ModeConfirm})
	o.SetNow(clk.now)
	o.d.DB.Create(&model.Node{Name: "A", IsLocal: true, Enabled: true})
	nb := model.Node{Name: "B", ApiUrl: "https://b/app/", Token: "t", Enabled: true}
	nc := model.Node{Name: "C", ApiUrl: "https://c/app/", Token: "t", Enabled: true}
	o.d.DB.Create(&nb)
	o.d.DB.Create(&nc)
	for i := 0; i < 8; i++ {
		o.NoteReply(nb, hub.EpochReply{Aware: true, Code: CodeStale, Role: "master", Epoch: 2, MasterId: nb.Id})
		o.NoteReply(nc, hub.EpochReply{Aware: true, NodeEpoch: 1})
		clk.add(30 * time.Second)
	}
	o.NoteReply(nb, hub.EpochReply{Aware: true, Code: CodeStale, Role: "master", Epoch: 2, MasterId: nb.Id})
	o.NoteReply(nc, hub.EpochReply{Aware: true, NodeEpoch: 1})
	rs = o.Rivals()
	if o.IsNode() {
		t.Fatal("多数副机(C)还跟着原主机:原主机不变为副机")
	}
	if len(rs) != 1 || !rs[0].Higher || rs[0].CanYield || rs[0].Followers != 1 {
		t.Fatalf("原主机这边:看得见纪元更高的 B,本机有 1 台副机跟着,不由本机让: %+v", rs)
	}
	if err := o.YieldTo(nb.Id, "test"); err == nil || !strings.Contains(err.Error(), "该让的是它") {
		t.Fatalf("纪元更低的这边不能让: %v", err)
	}
	// B 不再自称主机:提示消失
	o.NoteReply(nb, hub.EpochReply{Aware: true, NodeEpoch: 1})
	if len(o.Rivals()) != 0 {
		t.Fatal("B 回来认本机了:不再提示")
	}
}
