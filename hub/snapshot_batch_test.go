package hub

import (
	"fmt"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 用户上千、"用户 × 线路"上万时,副机应用快照不能超过 SQLite 一条语句 32766 个变量的上限。
// 以前每张表一条 INSERT:约 1310 个用户、或 16384 条用户线路、或 10923 条用户线路收窄就整份失败,
// 所有副机从此收不到任何配置。这里三张表都越过各自的临界点。
func TestApplySnapshotManyUsers(t *testing.T) {
	const users, lines, narrowed = 1400, 12, 1000 // 用户线路 16800 条;收窄行 1000 × 11 = 11000 条
	src := openDB(t, "src.db").DB
	src.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true})
	src.Create(&model.Node{Name: "副机", Enabled: true})
	for i := 1; i <= lines; i++ {
		src.Create(&model.Line{Name: fmt.Sprintf("l%d", i), Protocol: "hysteria2", Port: 20000 + i, Enabled: true})
	}
	us := make([]model.User, 0, users)
	for i := 1; i <= users; i++ {
		us = append(us, model.User{Name: fmt.Sprintf("u%04d", i), Enabled: true, Credentials: []byte(`{"hysteria2":{"password":"p"}}`)})
	}
	if err := src.CreateInBatches(&us, 500).Error; err != nil {
		t.Fatal(err)
	}
	src.Model(&model.User{}).Where("id % 3 = 0").Update("enabled", false)
	var uls []model.UserLine
	var ulns []model.UserLineNode
	for u := 1; u <= users; u++ {
		for l := 1; l <= lines; l++ {
			uls = append(uls, model.UserLine{UserId: uint(u), LineId: uint(l)})
			if u <= narrowed && l < lines {
				ulns = append(ulns, model.UserLineNode{UserId: uint(u), LineId: uint(l), NodeId: 2})
			}
		}
	}
	if err := src.CreateInBatches(&uls, 1000).Error; err != nil {
		t.Fatal(err)
	}
	if err := src.CreateInBatches(&ulns, 1000).Error; err != nil {
		t.Fatal(err)
	}

	snap, err := BuildSnapshot(src, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	snap.SelfNodeId = 2
	dst := openDB(t, "dst.db").DB
	if _, _, err := ApplySnapshot(dst, snap); err != nil {
		t.Fatalf("上千用户的快照应能应用: %v", err)
	}
	count := func(m interface{}, where string) int64 {
		var n int64
		dst.Model(m).Where(where).Count(&n)
		return n
	}
	if n := count(&model.User{}, "1 = 1"); n != users {
		t.Fatalf("用户应有 %d 个,实际 %d", users, n)
	}
	if n := count(&model.User{}, "enabled = 0"); n != users/3 {
		t.Fatalf("主机停用的 %d 个用户在副机上也得是停用,实际 %d", users/3, n)
	}
	if n := count(&model.UserLine{}, "1 = 1"); n != users*lines {
		t.Fatalf("用户线路应有 %d 条,实际 %d", users*lines, n)
	}
	if n := count(&model.UserLineNode{}, "1 = 1"); n != int64(len(ulns)) {
		t.Fatalf("收窄行应有 %d 条,实际 %d", len(ulns), n)
	}
}
