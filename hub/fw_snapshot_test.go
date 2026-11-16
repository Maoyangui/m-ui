package hub

import (
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 面板防火墙的规则随快照走:主机发整张表(一条都没有也发空表),副机整表替换;规则变了修订号要变;
// 旧版主机不发这个字段(nil),副机不动自己的规则。
func TestSnapshotCarriesFwRules(t *testing.T) {
	src := openDB(t, "hub.db").DB
	src.Create(&model.Node{Name: "香港", IsLocal: true})
	src.Create(&model.Node{Name: "台湾", ApiUrl: "https://tw:2053/ad/", Token: "tok"})
	setting := func(string) string { return "" }

	s0, err := BuildSnapshot(src, setting)
	if err != nil {
		t.Fatal(err)
	}
	if s0.FwRules == nil || len(s0.FwRules) != 0 {
		t.Fatalf("没有规则时也要发空表(不是 nil): %#v", s0.FwRules)
	}
	src.Create(&model.FwRule{Kind: "deny", Sources: "203.0.113.9/32", Enabled: true, Origin: "manual"})
	src.Create(&model.FwRule{Kind: "only", Proto: "tcp", Ports: "22", Sources: "198.51.100.7/32", Enabled: true, NodeIds: []byte(`[2]`), Expiry: 1900000000})
	s1, _ := BuildSnapshot(src, setting)
	if len(s1.FwRules) != 2 || s1.Revision == s0.Revision {
		t.Fatalf("加了规则:快照里应有 2 条、修订号要变(%d 条,%s / %s)", len(s1.FwRules), s0.Revision, s1.Revision)
	}
	src.Model(&model.FwRule{}).Where("id = ?", 1).Update("enabled", false)
	s2, _ := BuildSnapshot(src, setting)
	if s2.Revision == s1.Revision {
		t.Fatal("关掉一条规则,修订号要变(副机要跟着撤)")
	}

	dst := openDB(t, "node.db").DB
	dst.Create(&model.FwRule{Kind: "deny", Sources: "192.0.2.99/32", Enabled: true}) // 副机上的旧规则:主机已经没有了
	s2.SelfNodeId = 2
	if _, _, err := ApplySnapshot(dst, s2); err != nil {
		t.Fatal(err)
	}
	var got []model.FwRule
	dst.Order("id asc").Find(&got)
	if len(got) != 2 || got[0].Sources != "203.0.113.9/32" || got[0].Enabled || got[1].Kind != "only" || got[1].Expiry != 1900000000 || string(got[1].NodeIds) != "[2]" {
		t.Fatalf("副机的规则表应整表换成主机的: %+v", got)
	}

	// 旧版主机的快照没有这个字段:副机的规则原样留着
	s2.FwRules = nil
	if _, _, err := ApplySnapshot(dst, s2); err != nil {
		t.Fatal(err)
	}
	var n int64
	dst.Model(&model.FwRule{}).Count(&n)
	if n != 2 {
		t.Fatalf("旧版主机不发规则时不该动副机的规则表: %d", n)
	}
	// 主机把规则删光:发空表,副机清空
	s2.FwRules = []model.FwRule{}
	if _, _, err := ApplySnapshot(dst, s2); err != nil {
		t.Fatal(err)
	}
	dst.Model(&model.FwRule{}).Count(&n)
	if n != 0 {
		t.Fatalf("主机删光后副机应清空: %d", n)
	}
}
