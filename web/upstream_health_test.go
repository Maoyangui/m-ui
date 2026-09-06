package web

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 上游健康按"谁在用"汇总:只列出真正部署了引用它的线路的服务器;没人用的标未使用。
func TestUpstreamHealthRows(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	db.Create(&model.Node{Name: "A", ApiUrl: "http://a", Enabled: true, Sort: 2})
	db.Create(&model.Node{Name: "B", ApiUrl: "http://b", Enabled: true, Sort: 3})
	db.Create(&model.Upstream{Name: "warp", Type: "socks", Sort: 1})   // 只有 B 上的线路用
	db.Create(&model.Upstream{Name: "jp", Type: "tuic", Sort: 2})      // A 和 B 都用
	db.Create(&model.Upstream{Name: "idle", Type: "tuic", Sort: 3})    // 没人用
	db.Create(&model.Upstream{Name: "offline", Type: "tuic", Sort: 4}) // 只被停用的线路用

	db.Create(&model.Line{Name: "b-line", Protocol: "hysteria2", Port: 30443, Enabled: true, UpstreamId: 1, NodeIds: []byte(`[3]`)})
	db.Create(&model.Line{Name: "ab-line", Protocol: "anytls", Port: 30444, Enabled: true, UpstreamId: 2, NodeIds: []byte(`[2,3]`)})
	db.Create(&model.Line{Name: "off-line", Protocol: "anytls", Port: 30445, UpstreamId: 4, NodeIds: []byte(`[2]`)})
	// gorm 对 default:true 的字段,Create 时给 false 会写成 true,得显式改回来
	db.Model(&model.Line{}).Where("name = ?", "off-line").Update("enabled", false)

	rows := s.upstreamHealthRows()
	if len(rows) != 4 {
		t.Fatalf("四条上游各一行: %d", len(rows))
	}
	by := map[string]upRow{}
	for _, r := range rows {
		by[r.Name] = r
	}
	if got := by["warp"].Servers; len(got) != 1 || got[0].Name != "B" {
		t.Fatalf("warp 只有 B 在用: %+v", got)
	}
	if got := by["jp"].Servers; len(got) != 2 || got[0].Name != "A" || got[1].Name != "B" {
		t.Fatalf("jp 应列出 A、B 两台(按服务器排序): %+v", got)
	}
	if !by["idle"].Unused || len(by["idle"].Servers) != 0 {
		t.Fatalf("没人用的上游应标未使用: %+v", by["idle"])
	}
	if !by["offline"].Unused {
		t.Fatalf("只被停用线路引用的上游也算没人用: %+v", by["offline"])
	}
	// 还没有任何机器上报结果时是"待测",不是故障 —— 副机刚接入不该一片红
	for _, sv := range by["jp"].Servers {
		if sv.State != "pending" {
			t.Fatalf("尚无结果时应为待测: %+v", sv)
		}
	}

	// 线路部署范围为空 = 全部服务器,三台都该列出来
	db.Model(&model.Line{}).Where("id = ?", 1).Update("node_ids", nil)
	rows = s.upstreamHealthRows()
	for _, r := range rows {
		if r.Name == "warp" && len(r.Servers) != 3 {
			t.Fatalf("部署范围为空 = 全部服务器: %+v", r.Servers)
		}
	}
}
