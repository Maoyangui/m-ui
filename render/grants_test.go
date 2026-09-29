package render

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 代理名下的用户按代理当前授权取交集:收回的线路不再下发,收窄到部分服务器的只在那几台下发;
// 主面板用户不受影响;库里的分配原样保留(重新授权即恢复);副机不再求交集(主机下发的已经是交集)。
func TestEffectiveLinesFollowResellerGrant(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true})     // 1
	db.Create(&model.Node{Name: "B", ApiUrl: "http://b", Enabled: true}) // 2
	db.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})
	db.Create(&model.Line{Name: "jp", Protocol: "anytls", Port: 30444, Enabled: true})
	rs := model.Reseller{Name: "r", Enabled: true}
	db.Create(&rs)
	db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 1}) // hk 全部服务器
	db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 2}) // jp 只授权了 B
	db.Create(&model.ResellerLineNode{ResellerId: rs.Id, LineId: 2, NodeId: 2})
	m := model.User{Name: "m", Enabled: true}
	r1 := model.User{Name: "r1", Enabled: true, ResellerId: rs.Id}
	r2 := model.User{Name: "r2", Enabled: true, ResellerId: rs.Id}
	for _, u := range []*model.User{&m, &r1, &r2} {
		db.Create(u)
	}
	for _, ul := range []model.UserLine{{UserId: m.Id, LineId: 1}, {UserId: m.Id, LineId: 2},
		{UserId: r1.Id, LineId: 1}, {UserId: r1.Id, LineId: 2}, {UserId: r2.Id, LineId: 2}} {
		db.Create(&ul)
	}
	db.Create(&model.UserLineNode{UserId: r2.Id, LineId: 2, NodeId: 1}) // r2 只要主机上的 jp —— 不在授权里

	on := func(self uint, line uint) []string {
		t.Helper()
		by, err := loadLineUsers(db, self)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, u := range by[line] {
			out = append(out, u.Name)
		}
		sort.Strings(out)
		return out
	}
	eq := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if got := on(1, 1); !eq(got, "m", "r1") {
		t.Fatalf("hk 全部授权,主机上应有 m、r1: %v", got)
	}
	if got := on(1, 2); !eq(got, "m") {
		t.Fatalf("jp 只授权了 B,主机上只该有 m: %v", got)
	}
	if got := on(2, 2); !eq(got, "m", "r1") {
		t.Fatalf("B 上 jp 应有 m、r1(r2 选的服务器不在授权里): %v", got)
	}

	// 收回 hk:r1 立刻从 hk 撤下,主面板用户不受影响,库里的分配不动
	db.Where("reseller_id = ? AND line_id = ?", rs.Id, 1).Delete(&model.ResellerLine{})
	if got := on(1, 1); !eq(got, "m") {
		t.Fatalf("收回后 hk 上只该剩 m: %v", got)
	}
	var n int64
	db.Model(&model.UserLine{}).Where("user_id = ?", r1.Id).Count(&n)
	if n != 2 {
		t.Fatalf("收回授权不该删用户自己的分配,剩 %d 条", n)
	}
	links, scopes, err := EffectiveLines(db, r1.Id)
	if err != nil || len(links) != 1 || links[0].LineId != 2 || len(scopes) != 1 || scopes[0].NodeId != 2 {
		t.Fatalf("r1 生效的应只有 jp@B: links=%+v scopes=%+v err=%v", links, scopes, err)
	}
	// 重新授权即恢复
	db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 1})
	if got := on(1, 1); !eq(got, "m", "r1") {
		t.Fatalf("重新授权后 r1 应回到 hk: %v", got)
	}

	// 副机:按库里的分配原样渲染(主机下发时已取过交集)
	db.Where("reseller_id = ?", rs.Id).Delete(&model.ResellerLine{})
	db.Create(&model.Setting{Key: "nodeMode", Value: "true"})
	if got := on(1, 1); !eq(got, "m", "r1") {
		t.Fatalf("副机不该再按本机授权表求交集: %v", got)
	}
}
