package hub

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 「要不要记用户 × 线路的用量」由主机按规则算好随快照下发(副机没有规则表):启用的突发规则勾了还在的线路才打开;
// 开关变了修订号也变(副机要收到);副机应用快照后库里有这个设置。忽略的线路随限速状态一起下发。
func TestSnapshotLineMeterSetting(t *testing.T) {
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
	master.Create(&model.Line{Name: "便宜", Protocol: "anytls", Port: 30444, Enabled: true})
	master.Create(&model.User{Name: "alice", Enabled: true})
	build := func() Snapshot {
		s, err := BuildSnapshot(master, setting)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	off := build()
	if off.Settings[LineMeterSetting] != "" {
		t.Fatal("没有规则用得着时不打开")
	}
	master.Create(&model.Rule{Name: "突发", Enabled: true, Kind: "burst", AllUsers: true, WindowMin: 10, ThresholdBytes: 1 << 30, PenaltyMin: 20, DownMbps: 5, SkipLineIds: []byte(`[1]`)})
	master.Create(&model.LimitState{RuleId: 1, UserId: 1, UserName: "alice", RuleName: "突发", DownMbps: 5, Until: 1 << 40, SkipLineIds: []byte(`[1]`)})
	on := build()
	if on.Settings[LineMeterSetting] != "true" || on.Revision == off.Revision {
		t.Fatalf("启用的突发规则勾了还在的线路要打开,修订号要变: %q %s %s", on.Settings[LineMeterSetting], on.Revision, off.Revision)
	}
	on.SelfNodeId = 2
	if _, _, err := ApplySnapshot(node, on); err != nil {
		t.Fatal(err)
	}
	var v string
	node.Raw("SELECT value FROM settings WHERE key = ?", LineMeterSetting).Scan(&v)
	var st []model.LimitState
	node.Find(&st)
	if v != "true" || len(st) != 1 || string(st[0].SkipLineIds) != `[1]` {
		t.Fatalf("副机应收到开关和带忽略线路的限速状态: %q %+v", v, st)
	}
	// 报告里的累计能原样传过去(副机报、主机解)
	b, _ := json.Marshal(Report{LineMeter: &model.LineMeter{Boot: "x", Rows: []model.LineMeterRow{{User: "alice", Line: 1, Raw: 7}}}})
	var back Report
	if err := json.Unmarshal(b, &back); err != nil || back.LineMeter == nil || back.LineMeter.Rows[0].Raw != 7 || back.LineMeter.Boot != "x" {
		t.Fatalf("报告里的累计传不过去: %s %v", b, err)
	}
}
