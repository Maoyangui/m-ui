package hub

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
	for _, r := range ha.replies {
		if r.Code != "" {
			t.Fatalf("纪元为 0 时副机正常回话,不该有纪元判定: %+v", r)
		}
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

// 本机纪元为 0 时副机回 409 纪元判定:照样解析成 EpochError 交给容灾(没开过容灾就设为主机、从开启前的备份还原回来的原主机要认得出);
// 别的 409(版本门槛之类)不是纪元判定。
func TestHAEpochZeroParses409(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	node := &haNode{mode: "stale"}
	srv := httptest.NewServer(node.handler())
	defer srv.Close()
	ha := &fakeHA{}
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }, HA: ha})
	n := model.Node{Id: 9, Name: "S", ApiUrl: srv.URL + "/app/", Token: "t"}
	err := h.Send(n, http.MethodGet, "ha/incoming", nil, 0, nil, 0, nil)
	var ee *EpochError
	if !errors.As(err, &ee) || ee.Code != "stale_epoch" || ee.Epoch != 3 {
		t.Fatalf("纪元为 0 也要把 409 纪元判定解析成 EpochError: %v", err)
	}
	ha.mu.Lock()
	got := append([]EpochReply(nil), ha.replies...)
	ha.mu.Unlock()
	if len(got) != 1 || got[0].Code != "stale_epoch" || got[0].MasterId != 7 {
		t.Fatalf("要交给容灾判定: %+v", got)
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"副机版本太旧","code":"version"}`))
	}))
	defer other.Close()
	err = h.Send(model.Node{Id: 10, Name: "O", ApiUrl: other.URL + "/app/", Token: "t"}, http.MethodGet, "ping", nil, 0, nil, 0, nil)
	if IsEpochError(err) {
		t.Fatalf("不在白名单里的 409 不是纪元判定: %v", err)
	}
}

// 老副机(ha/state 404)升级上来以后:回话带了纪元头,下一轮立刻推容灾状态,不等 10 分钟。
func TestHALegacyMarkClearedOnUpgrade(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	node := &haNode{mode: "legacy"}
	srv := httptest.NewServer(node.handler())
	defer srv.Close()
	db.Create(&model.Node{Name: "L", ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true})
	ha := &fakeHA{epoch: 2, master: 1, rev: "r"}
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }, HA: ha})
	h.RunOnce()
	node.mu.Lock()
	node.mode = "" // 升级到认得纪元的版本
	node.mu.Unlock()
	h.RunOnce() // 这一轮回话带了纪元头:清掉老副机标记
	h.RunOnce()
	paths, _ := node.snapshot()
	states := 0
	for _, p := range paths {
		if p == "ha/state" {
			states++
		}
	}
	if states < 2 {
		t.Fatalf("升级后要马上重推容灾状态: %v", paths)
	}
}

// 设为主机的整库、大副本给的时限比客户端兜底的总时限长:不能被客户端的总时限截断,只按请求自己的时限。
func TestHALongTransferNoClientTimeout(t *testing.T) {
	db := openDB(t, "m.db").DB
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{"ok":"1"}`))
	}))
	defer slow.Close()
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	n := model.Node{Id: 9, Name: "S", ApiUrl: slow.URL + "/app/", Token: "t"}
	h.clients = map[uint]*nodeClient{n.Id: {key: nodeClientKey(n), c: &http.Client{Timeout: 100 * time.Millisecond}}}
	if err := h.Send(n, http.MethodPost, "ha/handover", nil, 0, nil, 5*time.Second, nil); err != nil {
		t.Fatalf("请求自己的时限更长:不受客户端兜底时限限制: %v", err)
	}
	if err := h.Send(n, http.MethodPost, "ha/handover", nil, 0, nil, 50*time.Millisecond, nil); err == nil {
		t.Fatal("请求自己的时限照样管用")
	}
}
