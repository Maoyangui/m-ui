package hub

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 停用副机时已经给它推过一份空用户表(Decommission),之后在面板上对它点「推送」不能把完整快照推过去 ——
// 那等于把停用的副机重新开放给所有用户。重新启用之后照常能推。
func TestPushNowRefusesDisabledNode(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})

	var applies atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent/apply") {
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			applies.Add(1)
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
			return
		}
		w.Write([]byte(`{"ok":"1"}`))
	}))
	defer srv.Close()
	n := model.Node{Name: "n1", ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true}
	db.Create(&n)
	db.Model(&model.Node{}).Where("id = ?", n.Id).Update("enabled", false)
	n.Enabled = false
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})

	if err := h.PushNow(n); !errors.Is(err, ErrNodeDisabled) {
		t.Fatalf("停用的副机点推送应被拒绝,实际: %v", err)
	}
	if got := applies.Load(); got != 0 {
		t.Fatalf("停用的副机不该收到任何配置,实际收到 %d 次", got)
	}
	// 「立即同步全部」只算启用的副机,停用的不推也不报错
	if err := h.SyncNow(); err != nil {
		t.Fatalf("同步全部不该因停用的副机报错: %v", err)
	}
	if got := applies.Load(); got != 0 {
		t.Fatalf("同步全部不该推给停用的副机,实际收到 %d 次", got)
	}

	db.Model(&model.Node{}).Where("id = ?", n.Id).Update("enabled", true)
	n.Enabled = true
	if err := h.PushNow(n); err != nil {
		t.Fatalf("重新启用后应能推送: %v", err)
	}
	if got := applies.Load(); got != 1 {
		t.Fatalf("重新启用后应推一次,实际 %d 次", got)
	}
}
