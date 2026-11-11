package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// newApplyNode 只应答推送的副机替身。keeps:本版副机,确认时说明"收到空用户表不清账本";
// 否则是老副机(0.7.16 及以前),应用停用推来的空用户表时把账本整本清掉,确认时什么都不说。
func newApplyNode(t *testing.T, keeps bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/agent/apply") {
			w.Write([]byte(`{"ok":"1"}`))
			return
		}
		var snap Snapshot
		_ = json.NewDecoder(r.Body).Decode(&snap)
		out := map[string]string{"ok": "1", "revision": snap.Revision}
		if keeps {
			out["ledgerKept"] = "1"
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 本版主机配老副机,停用再启用:老副机收到停用推的空用户表就把账本整本清掉,重新启用后每个人从 0 重开一本。
// 老副机不报账本代号,同一个人计数变小一般只能当账本回退(对齐不计、告警);主机这时知道它清了账,
// 重新启用后第一次变小按重开从 0 计,不当回退、不告警(以前应计 1300,实计 1000,还误报"账本回退")。
func TestOldAgentDecommissionClearsLedger(t *testing.T) {
	for _, tc := range []struct {
		name   string
		keeps  bool  // 本版副机
		legacy bool  // 游标是升级前老主机写的,主机升级后还没动过
		after  int64 // 重新启用后第一份报告里的计数
		want   int64 // 那之后主机上的用量
		regr   int
	}{
		{name: "老副机清了账从 0 重开", after: 300, want: 1300},
		{name: "老副机清了账从 0 重开(游标是升级前老主机写的)", legacy: true, after: 300, want: 1300},
		{name: "老副机没清掉(计数没变小)照常按增量", after: 1050, want: 1050},
		{name: "本版副机停用不清账本:变小仍是账本回退", keeps: true, after: 300, want: 1000, regr: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openDB(t, "m.db").DB
			srv := newApplyNode(t, tc.keeps)
			db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
			node := model.Node{Name: "n1", ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true}
			db.Create(&node)
			db.Create(&model.User{Name: "alice", Enabled: true, CreatedAt: 100})
			h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
			defer h.Stop()
			merge := func(at, n int64) MergeResult {
				t.Helper()
				res, err := MergeCounters(db, node.Id, "n1", "", []model.AgentCounter{{UserName: "alice", Up: n, Down: n}}, at, 60, 1)
				if err != nil {
					t.Fatal(err)
				}
				return res
			}
			usage := func() int64 {
				t.Helper()
				var u model.User
				db.Where("name = ?", "alice").First(&u)
				return u.Down
			}
			// 老副机的报告不带主人和代号
			if tc.legacy {
				mergeV0716(t, db, node.Id, "", []model.AgentCounter{{UserName: "alice", Up: 1000, Down: 1000}})
			} else {
				merge(1000, 1000)
			}
			if got := usage(); got != 1000 {
				t.Fatalf("停用前应计 1000,实际 %d", got)
			}
			before, err := BuildSnapshot(db, h.d.Setting)
			if err != nil {
				t.Fatal(err)
			}
			db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", false)
			if err := h.Decommission(node, before); err != nil {
				t.Fatal(err)
			}
			var cur model.TrafficCursor
			db.Where("node_id = ? AND user_name = ?", node.Id, "alice").First(&cur)
			if cur.Cleared == tc.keeps {
				t.Fatalf("老副机确认了空用户表才标记账本已清空(本版副机 %v),实际游标 %+v", tc.keeps, cur)
			}
			db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", true)
			res := merge(1005, tc.after)
			if got := usage(); got != tc.want || res.Regressed != tc.regr {
				t.Fatalf("重新启用后第一份(%d)应计到 %d、账本回退 %d 个,实际 %d、%+v", tc.after, tc.want, tc.regr, got, res)
			}
			if reopened := tc.want-1000 == tc.after; (res.Reopened == 1) != reopened {
				t.Fatalf("从 0 重开的才算 Reopened: %+v", res)
			}
			// 只管停用后的第一次:之后同一本账真的回退,照常按账本回退处理
			merge(1010, tc.after+50)
			if res := merge(1015, tc.after+40); res.Regressed != 1 || usage() != tc.want+50 {
				t.Fatalf("之后的回退应照常对齐不计,应是 %d,实际 %d(%+v)", tc.want+50, usage(), res)
			}
		})
	}
}

// 老副机上改名后又改回:老副机应用改名后的快照时把原名那本清掉,改回后同一个人用原名从 0 重开。主机推完快照、
// 老副机确认后,给主机上已经没有的名字的游标打上清账标记,改回后的第一次变小按重开从 0 计、不告警
// (以前当账本回退:首份不计,还误报)。
func TestOldAgentRenamedBackReopensLedger(t *testing.T) {
	db := openDB(t, "m.db").DB
	srv := newApplyNode(t, false)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	node := model.Node{Name: "n1", ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true}
	db.Create(&node)
	u := model.User{Name: "alice", Enabled: true, CreatedAt: 100, Credentials: []byte(`{}`)}
	db.Create(&u)
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	defer h.Stop()
	merge := func(at, n int64) MergeResult {
		t.Helper()
		res, err := MergeCounters(db, node.Id, "n1", "", []model.AgentCounter{{UserName: "alice", Up: n, Down: n}}, at, 60, 1)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	usage := func() int64 {
		t.Helper()
		var x model.User
		db.First(&x, u.Id)
		return x.Down
	}
	cleared := func() bool {
		t.Helper()
		var cur model.TrafficCursor
		db.Where("node_id = ? AND user_name = ?", node.Id, "alice").First(&cur)
		return cur.Cleared
	}
	if err := h.PushNow(node); err != nil {
		t.Fatal(err)
	}
	merge(1000, 1000)
	if cleared() {
		t.Fatal("名字还在,不该打清账标记")
	}
	db.Model(&model.User{}).Where("id = ?", u.Id).Update("name", "bob")
	if err := h.PushNow(node); err != nil {
		t.Fatal(err)
	}
	if !cleared() {
		t.Fatal("老副机确认了没有 alice 的快照,alice 的游标应打上清账标记")
	}
	db.Model(&model.User{}).Where("id = ?", u.Id).Update("name", "alice")
	if err := h.PushNow(node); err != nil {
		t.Fatal(err)
	}
	res := merge(1005, 300)
	if usage() != 1300 || res.Regressed != 0 || res.Reopened != 1 {
		t.Fatalf("改回原名后从 0 重开的一本应从 0 计,应是 1300,实际 %d(%+v)", usage(), res)
	}
	merge(1010, 350)
	if res := merge(1015, 340); res.Regressed != 1 || usage() != 1350 {
		t.Fatalf("标记只管一次,之后的回退照常对齐不计,应是 1350,实际 %d(%+v)", usage(), res)
	}
}

// 推给老副机的每份快照之后都要看一眼有没有要打清账标记的游标(快照里没有的名字),绝大多数时候没有:那就不该去拿写锁
// (别的写事务占着锁时要干等满 busy_timeout,推送那一轮跟着拖住)。
func TestMarkLedgerClearedNoopTakesNoWriteLock(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "n2", Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, CreatedAt: 100})
	if _, err := MergeCounters(db, 2, "n2", "", []model.AgentCounter{{UserName: "alice", Up: 10, Down: 10}}, 1000, 60, 1); err != nil {
		t.Fatal(err)
	}
	release := holdWriteLock(t, db)
	defer release()
	t0 := time.Now()
	err := markLedgerCleared(db, 2, map[string]bool{"alice": true})
	if d := time.Since(t0); err != nil || d > 2*time.Second {
		t.Fatalf("推过去的快照里还有 alice、没有要标的,不该等写锁:用了 %v, err=%v", d, err)
	}
	release()
	if err := markLedgerCleared(db, 2, map[string]bool{"bob": true}); err != nil {
		t.Fatal(err)
	}
	var cur model.TrafficCursor
	db.Where("node_id = ? AND user_name = ?", 2, "alice").First(&cur)
	if !cur.Cleared || cur.ClearedAt == 0 {
		t.Fatalf("推过去的快照里没有 alice,游标应打上清账标记并记下时刻: %+v", cur)
	}
}
