package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// fakeHA 容灾钩子的桩:纪元可调,记下每次请求副机的纪元结果。
type fakeHA struct {
	mu      sync.Mutex
	epoch   uint64
	master  uint
	rev     string
	replies []EpochReply
}

func (f *fakeHA) Header() (uint64, uint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.epoch, f.master
}

func (f *fakeHA) StateFor(n model.Node) ([]byte, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.epoch == 0 {
		return nil, ""
	}
	return []byte(`{"epoch":2}`), f.rev
}

func (f *fakeHA) NoteReply(n model.Node, r EpochReply) {
	f.mu.Lock()
	f.replies = append(f.replies, r)
	f.mu.Unlock()
}

// haNode 一台记下请求头的假副机。mode: "legacy" 没有 ha/state 接口;"stale" 一律 409 stale_epoch;其余正常且认得纪元。
type haNode struct {
	mu      sync.Mutex
	mode    string
	paths   []string
	headers []http.Header
}

func (n *haNode) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.paths = append(n.paths, r.URL.Path[strings.LastIndex(r.URL.Path, "/agent/")+len("/agent/"):])
		n.headers = append(n.headers, r.Header.Clone())
		mode := n.mode
		n.mu.Unlock()
		if mode == "stale" {
			w.Header().Set("X-HA-Node", "3")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "x", "code": "stale_epoch", "epoch": 3, "masterId": 7, "sinceChange": 42})
			return
		}
		if mode != "legacy" {
			w.Header().Set("X-HA-Node", "2")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/ha/state"):
			if mode == "legacy" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(`{"ok":"1"}`))
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true})
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	})
}

func (n *haNode) snapshot() ([]string, []http.Header) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.paths...), append([]http.Header(nil), n.headers...)
}

// 没开过容灾(纪元 0):请求一个容灾请求头都不带、不推容灾状态 —— 和以前一字不差。
// 开了之后:每个请求都带纪元与主机 id,先推容灾状态;老副机(没有 ha/state 接口)照常推配置、拉报告,10 分钟内不再试。
func TestHAHeadersAndStatePush(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	aware, legacy := &haNode{}, &haNode{mode: "legacy"}
	sa, sl := httptest.NewServer(aware.handler()), httptest.NewServer(legacy.handler())
	defer sa.Close()
	defer sl.Close()
	db.Create(&model.Node{Name: "A", ApiUrl: sa.URL + "/app/", Token: "t", Enabled: true})
	db.Create(&model.Node{Name: "L", ApiUrl: sl.URL + "/app/", Token: "t", Enabled: true})
	ha := &fakeHA{}
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }, HA: ha})

	h.RunOnce()
	paths, hdrs := aware.snapshot()
	for i, p := range paths {
		if p == "ha/state" || hdrs[i].Get("X-HA-Epoch") != "" || hdrs[i].Get("X-HA-Master") != "" {
			t.Fatalf("纪元为 0 时不能推容灾状态、不能带容灾请求头: %s %v", p, hdrs[i])
		}
	}
	if len(ha.replies) != 0 {
		t.Fatal("纪元为 0 时不该把回复交给容灾")
	}

	ha.mu.Lock()
	ha.epoch, ha.master, ha.rev = 2, 1, "r1"
	ha.mu.Unlock()
	before := len(paths)
	h.RunOnce()
	paths, hdrs = aware.snapshot()
	sawState := false
	for i, p := range paths[before:] {
		hd := hdrs[before+i]
		if hd.Get("X-HA-Epoch") != "2" || hd.Get("X-HA-Master") != "1" {
			t.Fatalf("开了容灾后每个请求都要带纪元与主机 id: %s %v", p, hd)
		}
		sawState = sawState || p == "ha/state"
	}
	if !sawState {
		t.Fatalf("开了容灾后应先推容灾状态: %v", paths)
	}
	h.RunOnce() // 修订没变:不再推
	paths, _ = aware.snapshot()
	n := 0
	for _, p := range paths {
		if p == "ha/state" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("容灾状态没变时不重推,实际推了 %d 次", n)
	}
	lp, _ := legacy.snapshot()
	states, reports := 0, 0
	for _, p := range lp {
		switch p {
		case "ha/state":
			states++
		case "report":
			reports++
		}
	}
	if states != 1 || reports < 3 {
		t.Fatalf("老副机:ha/state 404 后不再试,配置和报告照常: state=%d report=%d %v", states, reports, lp)
	}
	st := h.Statuses()
	for _, s := range st {
		if !s.OK {
			t.Fatalf("老副机和认得纪元的副机都应正常: %+v", s)
		}
	}
	legacyNoted, awareNoted := false, false
	for _, r := range ha.replies {
		if r.Aware {
			awareNoted = true
		} else if !r.Down {
			legacyNoted = true
		}
	}
	if !legacyNoted || !awareNoted {
		t.Fatalf("老副机与认得纪元的副机的回复都要交给容灾: %+v", ha.replies)
	}
}

// 副机按纪元拒绝(409 + code):这一轮不推配置、不拉报告,状态里写明原因;不进推送退避(交接一完成副机就收)。
func TestHAStaleEpochStopsRound(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	node := &haNode{mode: "stale"}
	srv := httptest.NewServer(node.handler())
	defer srv.Close()
	db.Create(&model.Node{Name: "S", ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true})
	ha := &fakeHA{epoch: 2, master: 1, rev: "r"}
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }, HA: ha})
	h.RunOnce()
	paths, _ := node.snapshot()
	if len(paths) != 1 || paths[0] != "ha/state" {
		t.Fatalf("推容灾状态被纪元拒绝后这一轮不该再发别的请求: %v", paths)
	}
	var st NodeStatus
	for _, s := range h.Statuses() {
		st = s
	}
	if st.OK || !strings.Contains(st.Error, "纪元 3") {
		t.Fatalf("状态应写明副机跟随了更高纪元: %+v", st)
	}
	if len(ha.replies) == 0 || ha.replies[0].Code != "stale_epoch" || ha.replies[0].Epoch != 3 || ha.replies[0].MasterId != 7 || ha.replies[0].SinceChange != 42 {
		t.Fatalf("纪元判定要交给容灾做多数判定: %+v", ha.replies)
	}
	h.mu.Lock()
	pf := h.pushFail
	h.mu.Unlock()
	if len(pf) != 0 {
		t.Fatalf("纪元判定不进推送退避: %+v", pf)
	}
	err := h.Send(model.Node{Id: 9, Name: "S", ApiUrl: srv.URL + "/app/", Token: "t"}, http.MethodGet, "ping", nil, 0, nil, 0, nil)
	if !IsEpochError(err) {
		t.Fatalf("Send 也要把 409 纪元判定归成 EpochError: %v", err)
	}
}
