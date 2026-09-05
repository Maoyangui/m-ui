package hub

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 副机整表替换后判断"线路 / 上游变没变"不能受顺序和存储形态影响:
// 主机快照按 sort 排、副机库按 id 排,只改了用户(比如生成一条临时共享)时必须判成"没变",
// 否则副机会走全量重启,该机上所有人掉线几秒。
func TestApplySnapshotDetectsOnlyRealLineChanges(t *testing.T) {
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
	master.Create(&model.Upstream{Name: "direct-2", Type: "direct", Options: []byte(`{}`), Sort: 2})
	master.Create(&model.Upstream{Name: "warp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`), Sort: 1})
	// 排序和 id 顺序故意相反;一条 NodeIds 为空(全部服务器),一条带 Options 空白差异
	master.Create(&model.Line{Name: "l1", Protocol: "hysteria2", Port: 30443, Enabled: true, Sort: 9, Options: []byte(`{ "up_mbps": 0 }`)})
	master.Create(&model.Line{Name: "l2", Protocol: "anytls", Port: 30444, Enabled: true, Sort: 1, NodeIds: []byte(`[1,2]`)})
	master.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{"hysteria2":{"name":"alice","password":"p1"}}`)})

	apply := func() (bool, bool) {
		snap, err := BuildSnapshot(master, setting)
		if err != nil {
			t.Fatal(err)
		}
		snap.SelfNodeId = 2
		lc, uc, err := ApplySnapshot(node, snap)
		if err != nil {
			t.Fatal(err)
		}
		return lc, uc
	}
	if lc, uc := apply(); !lc || !uc {
		t.Fatalf("首次应用:副机原本是空的,应报线路与上游都变了,实际 lines=%v ups=%v", lc, uc)
	}
	if lc, uc := apply(); lc || uc {
		t.Fatalf("原样再应用一次:不该报变化,实际 lines=%v ups=%v", lc, uc)
	}
	// 只改用户(临时共享 / 改密码 / 停用):不该报线路或上游变化
	master.Model(&model.User{}).Where("name = ?", "alice").Updates(map[string]interface{}{"share_token": "abc", "share_creds": []byte(`{"hysteria2":{"name":"alice#share","password":"p2"}}`)})
	if lc, uc := apply(); lc || uc {
		t.Fatalf("只改了用户:不该报线路或上游变化,实际 lines=%v ups=%v", lc, uc)
	}
	// 只改排序(拖动线路顺序):也不该重启
	master.Model(&model.Line{}).Where("name = ?", "l1").Update("sort", 0)
	if lc, uc := apply(); lc || uc {
		t.Fatalf("只改了线路排序:不该报变化(它不影响数据面),实际 lines=%v ups=%v", lc, uc)
	}
	// 真改了线路端口:要报
	master.Model(&model.Line{}).Where("name = ?", "l1").Update("port", 30445)
	if lc, _ := apply(); !lc {
		t.Fatal("改了线路端口应报线路变化")
	}
	// 真改了上游参数:要报
	master.Model(&model.Upstream{}).Where("name = ?", "warp").Update("options", []byte(`{"server":"127.0.0.1","server_port":40001}`))
	if lc, uc := apply(); lc || !uc {
		t.Fatalf("只改了上游:应只报上游变化,实际 lines=%v ups=%v", lc, uc)
	}
}
