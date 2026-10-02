package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// recordingNode 记下收到的每一份快照的副机替身。
type recordingNode struct {
	srv   *httptest.Server
	mu    sync.Mutex
	snaps []Snapshot
}

func newRecordingNode(t *testing.T) *recordingNode {
	f := &recordingNode{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			f.mu.Lock()
			f.snaps = append(f.snaps, snap)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true})
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *recordingNode) last() (Snapshot, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.snaps) == 0 {
		return Snapshot{}, 0
	}
	return f.snaps[len(f.snaps)-1], len(f.snaps)
}

// 停用 / 删除副机时先推一份空用户表(用户 2026-09-29 拍板):那台立刻停止为任何人服务;
// 线路与它自己那一行留着(删掉的副机认不出本机就会把所有线路当成自己的去渲染);
// 序号不低于它收过的;之后的定时同步不再给它推完整快照;重新启用后推回完整快照。
func TestDecommissionPushesEmptyUserTable(t *testing.T) {
	for _, mode := range []string{"disable", "delete"} {
		t.Run(mode, func(t *testing.T) {
			db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close(db)
			fake := newRecordingNode(t)
			db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
			node := model.Node{Name: "n1", ApiUrl: fake.srv.URL + "/app/", Token: "t", Enabled: true}
			db.Create(&node)
			line := model.Line{Name: "l1", Protocol: "vless", Port: 20001, Enabled: true, NodeIds: json.RawMessage(`[2]`)}
			db.Create(&line)
			u := model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)}
			db.Create(&u)
			db.Create(&model.UserLine{UserId: u.Id, LineId: line.Id})
			h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})

			h.tick()
			h.settle()
			full, n := fake.last()
			if n != 1 || len(full.Users) != 1 {
				t.Fatalf("应先推一份带用户的完整快照,实际 %d 份,用户 %d", n, len(full.Users))
			}

			before, err := BuildSnapshot(db, h.d.Setting)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "disable" {
				db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", false)
			} else { // 照 web 层删除:独占的线路停用,服务器行删掉
				db.Model(&model.Line{}).Where("id = ?", line.Id).Updates(map[string]interface{}{"enabled": false, "node_ids": nil})
				db.Delete(&model.Node{}, node.Id)
			}
			if err := h.Decommission(node, before); err != nil {
				t.Fatal(err)
			}
			got, n := fake.last()
			if n != 2 || len(got.Users) != 0 || len(got.UserLines) != 0 {
				t.Fatalf("下线推送应是空用户表,实际第 %d 份,用户 %d,分配 %d", n, len(got.Users), len(got.UserLines))
			}
			if len(got.Lines) != 1 || !got.Lines[0].Enabled || string(got.Lines[0].NodeIds) != "[2]" {
				t.Fatalf("线路要原样留着(那台照旧监听、谁连都认证不过): %+v", got.Lines)
			}
			self := false
			for _, x := range got.Nodes {
				self = self || x.Id == node.Id
			}
			if !self || got.SelfNodeId != node.Id {
				t.Fatalf("快照里要有它自己那一行,否则它认不出本机: %+v", got.Nodes)
			}
			if got.Sequence < full.Sequence || got.Revision == full.Revision {
				t.Fatalf("序号不能低于已推过的(%d < %d),修订号要变: %s", got.Sequence, full.Sequence, got.Revision)
			}

			h.tick()
			h.settle()
			if _, n := fake.last(); n != 2 {
				t.Fatalf("停用 / 删除之后定时同步不该再推完整快照,实际共 %d 份", n)
			}
			if mode == "disable" {
				db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", true)
				h.tick()
				h.settle()
				if back, n := fake.last(); n != 3 || len(back.Users) != 1 {
					t.Fatalf("重新启用后应推回完整快照,实际第 %d 份,用户 %d", n, len(back.Users))
				}
			}
		})
	}
}
