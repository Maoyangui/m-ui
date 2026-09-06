package hub

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 主机版本、副机最低版本、外部订阅的抓取记录都不该改变修订号。
func TestRevisionIgnoresVersionAndFetchMeta(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.ExtNode{Name: "x", Type: "sub", Value: "https://example.com/s", Enabled: true, Cache: "ss://a", LastFetch: 1, NodeCount: 1})
	setting := func(string) string { return "" }
	a, _ := BuildSnapshot(db, setting)
	db.Model(&model.ExtNode{}).Where("id = ?", 1).Updates(map[string]interface{}{"last_fetch": 999, "last_error": "timeout", "node_count": 3})
	b, _ := BuildSnapshot(db, setting)
	if a.Revision != b.Revision {
		t.Fatal("只改抓取时间 / 报错 / 节点数不该改变修订号")
	}
	b.Version, b.MinNode = "9.9.9", "0.5.0"
	if revisionOf(b) != a.Revision {
		t.Fatal("版本字段不该进修订号")
	}
	db.Model(&model.ExtNode{}).Where("id = ?", 1).Update("cache", "ss://b")
	c, _ := BuildSnapshot(db, setting)
	if c.Revision == a.Revision {
		t.Fatal("抓到的内容变了必须改变修订号")
	}
}
