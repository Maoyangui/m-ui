package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
)

// 服务器页对下线通知和账本记录的几处收尾(审计 I4):
//   - 删服务器时顺手清掉老版本留下的孤儿游标与账本纪元(删副机时和在途并账竞态写回去的),
//     不然同一台机器重新加回来会一直被当成重复账本、流量不计;
//   - 停用着的副机改了地址 / 令牌,还没送到的下线通知按新的接着重试;
//   - 重新启用时还没送到的下线通知作废,不能再给正在服务的副机推空表。
func TestNodeAPIDecommissionBookkeeping(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	db.Create(&model.Node{Name: "A", ApiUrl: "http://a", Enabled: true, Sort: 2})
	db.Create(&model.Node{Name: "B", ApiUrl: "http://b", Token: "old", Enabled: true, Sort: 3})
	db.Model(&model.Node{}).Where("id = ?", 3).Update("enabled", false)
	for _, id := range []uint{2, 3, 9} { // 9 早就删了,游标和纪元是老版本竞态留下的
		db.Create(&model.TrafficCursor{NodeId: id, UserName: "alice", Up: 1, Down: 1})
		db.Create(&model.Setting{Key: hub.LedgerEpochSetting(id), Value: "epoch-" + string(rune('0'+id))})
	}
	call := func(method, path, body string) string {
		t.Helper()
		req := httptest.NewRequest(method, "http://x/app/api/nodes/"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleNodeItem(w, req)
		if w.Code != 200 {
			t.Fatalf("%s %s 应成功: %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	count := func(m interface{}, where string, args ...interface{}) int64 {
		var n int64
		db.Model(m).Where(where, args...).Count(&n)
		return n
	}

	call("DELETE", "2", "")
	for _, id := range []uint{2, 9} {
		if c, e := count(&model.TrafficCursor{}, "node_id = ?", id), count(&model.Setting{}, "key = ?", hub.LedgerEpochSetting(id)); c != 0 || e != 0 {
			t.Fatalf("副机 #%d 的游标(%d)和纪元(%d)应清掉", id, c, e)
		}
	}
	if c, e := count(&model.TrafficCursor{}, "node_id = ?", 3), count(&model.Setting{}, "key = ?", hub.LedgerEpochSetting(3)); c != 1 || e != 1 {
		t.Fatalf("还在的副机 #3 的游标(%d)和纪元(%d)不能动", c, e)
	}

	db.Create(&model.NodeDecommission{NodeId: 3, Name: "B", ApiUrl: "http://b", Token: "old", Revision: "r", CreatedAt: 1})
	call("PUT", "3", `{"name":"B","apiUrl":"http://b2","token":"new","enabled":false}`)
	var rec model.NodeDecommission
	db.First(&rec, "node_id = ?", 3)
	if rec.ApiUrl != "http://b2" || rec.Token != "new" {
		t.Fatalf("停用着改了地址和令牌,下线通知应按新的重试: %s / %s", rec.ApiUrl, rec.Token)
	}
	call("PUT", "3", `{"name":"B","apiUrl":"http://b2","enabled":true}`)
	if n := count(&model.NodeDecommission{}, "node_id = ?", 3); n != 0 {
		t.Fatalf("重新启用后还没送到的下线通知应作废,剩 %d 条", n)
	}
}
