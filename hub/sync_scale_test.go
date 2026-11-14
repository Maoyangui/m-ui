package hub

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 主机上同名用户删了又建(新的 id / 建号时间),副机那本也从头记了:计数比游标小,从 0 计 ——
// 和"同一个用户计数变小 = 账本回退"分开。副机那本没清(删了又建发生在同一轮同步里)时计数不变小,按游标算增量,
// 不会把前一个同名用户在这台机器上的全部用量记到新用户头上。
func TestApplyCountersRecreatedUserCountsFromZero(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "n2", Enabled: true})
	old := model.User{Name: "alice", Enabled: true, CreatedAt: 100}
	db.Create(&old)
	report := func(up, at int64) {
		t.Helper()
		if _, err := ApplyCounters(db, 2, "n2", "", []model.AgentCounter{{UserName: "alice", Up: up, Down: up}}, at, 60, 1); err != nil {
			t.Fatal(err)
		}
	}
	usage := func() int64 {
		var u model.User
		db.First(&u, "name = ?", "alice")
		return u.Up
	}
	report(1000, 1000)
	db.Delete(&model.User{}, old.Id)
	fresh := model.User{Name: "alice", Enabled: true, CreatedAt: 200}
	db.Create(&fresh)
	report(300, 1005) // 副机应用了删掉 alice 的快照,账本清了,新 alice 从 0 记到 300
	if got := usage(); got != 300 {
		t.Fatalf("删了又建的同名用户应从 0 计(300),实际 %d", got)
	}
	report(350, 1010)
	if got := usage(); got != 350 {
		t.Fatalf("之后照常按增量,应是 350,实际 %d", got)
	}
	// 再删再建,但副机那本没清(计数没变小):只计增量
	db.Delete(&model.User{}, fresh.Id)
	db.Create(&model.User{Name: "alice", Enabled: true, CreatedAt: 300})
	report(400, 1015)
	if got := usage(); got != 50 {
		t.Fatalf("副机账本没清时只该计这段增量 50,实际 %d", got)
	}
	// 主机上删掉以后副机还报来一份在途流量(游标记成"当时主机上没有这个用户"),之后再建同名用户、副机那本从头记:也从 0 计
	db.Where("name = ?", "alice").Delete(&model.User{})
	report(450, 1020)
	db.Create(&model.User{Name: "alice", Enabled: true, CreatedAt: 400})
	report(20, 1025)
	if got := usage(); got != 20 {
		t.Fatalf("删掉期间报过在途流量的同名用户再建出来,也应从 0 计(20),实际 %d", got)
	}
}

// 等确认超时、报告也说还没好(待重载一直不清):退避,退避期内不再整份重推;退避到期才再推一次。
func TestPushTimeoutBacksOff(t *testing.T) {
	defer shortApplyTimeout(500 * time.Millisecond)()
	db := openDB(t, "x.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})
	var applies int32
	var mu sync.Mutex
	nodeRev := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			atomic.AddInt32(&applies, 1)
			mu.Lock()
			nodeRev = snap.Revision
			mu.Unlock()
			time.Sleep(time.Second)
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			mu.Lock()
			rep := Report{Version: "t", CoreRunning: true, Revision: nodeRev, ReloadPending: true} // 数据面一直没应用成功
			mu.Unlock()
			json.NewEncoder(w).Encode(rep)
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	defer srv.Close()
	db.Create(&model.Node{Name: "n1", ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true})
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	defer h.Stop()
	for i := 0; i < 4; i++ {
		h.tick()
		h.settle()
	}
	if got := atomic.LoadInt32(&applies); got != 1 {
		t.Fatalf("等确认超时后应退避,四轮只该推 1 次,实际 %d", got)
	}
	st := h.Statuses()[2]
	h.mu.Lock()
	pf := h.pushFail[2]
	pushed := h.pushed[2]
	h.mu.Unlock()
	if pf == nil || !pf.slow || pushed != "" || !strings.Contains(st.Error, "后再试") {
		t.Fatalf("应记成等确认超时、在退避中、不算已推送: pf=%+v pushed=%q err=%q", pf, pushed, st.Error)
	}
	h.mu.Lock()
	pf.at = pf.at.Add(-time.Minute) // 退避到期
	h.mu.Unlock()
	h.tick()
	h.settle()
	if got := atomic.LoadInt32(&applies); got != 2 {
		t.Fatalf("退避到期应再推一次,实际累计 %d 次", got)
	}
}

// decomNode 能切换"失联"的副机替身:失联时 apply 回 502;记下收到的每份快照。
type decomNode struct {
	srv   *httptest.Server
	down  atomic.Bool
	mu    sync.Mutex
	snaps []Snapshot
}

func newDecomNode(t *testing.T) *decomNode {
	f := &decomNode{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			snap, err := DecodeSnapshot(w, r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.snaps = append(f.snaps, snap)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true, ApplyGzip: true})
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *decomNode) received() []Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Snapshot(nil), f.snaps...)
}

func pendingDecommissions(t *testing.T, h *Hub) []model.NodeDecommission {
	t.Helper()
	var recs []model.NodeDecommission
	if err := h.d.DB.Find(&recs).Error; err != nil {
		t.Fatal(err)
	}
	return recs
}

// 停用 / 删除时那台副机正好失联(审计 I4):以前下线推送只试一次,失败就作罢,那台一直按旧用户表为所有人服务。
// 现在落库、每轮重试,直到它确认;删掉的副机也照样重试。重新启用的不再推空表。
func TestDecommissionRetriedUntilAck(t *testing.T) {
	for _, mode := range []string{"disable", "delete"} {
		t.Run(mode, func(t *testing.T) {
			db := openDB(t, "x.db").DB
			db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
			db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})
			fake := newDecomNode(t)
			node := model.Node{Name: "n1", ApiUrl: fake.srv.URL + "/app/", Token: "t", Enabled: true}
			db.Create(&node)
			h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
			defer h.Stop()
			h.tick()
			h.settle()
			if got := fake.received(); len(got) != 1 || len(got[0].Users) != 1 {
				t.Fatalf("应先推一份完整快照: %d 份", len(got))
			}
			before, err := BuildSnapshot(db, h.d.Setting)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "disable" {
				db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", false)
			} else {
				db.Delete(&model.Node{}, node.Id)
			}
			fake.down.Store(true)
			if err := h.Decommission(node, before); err == nil {
				t.Fatal("副机失联时下线推送应报错")
			}
			if recs := pendingDecommissions(t, h); len(recs) != 1 || recs[0].Attempts != 1 || recs[0].LastError == "" {
				t.Fatalf("没送到的下线通知应落库并记下失败: %+v", recs)
			}
			h.tick() // 还在退避里:不重推
			h.settle()
			fake.down.Store(false)
			db.Model(&model.NodeDecommission{}).Where("node_id = ?", node.Id).Update("last_try", time.Now().Add(-time.Hour).Unix())
			h.tick()
			h.settle()
			got := fake.received()
			if len(got) != 2 || len(got[1].Users) != 0 || got[1].SelfNodeId != node.Id {
				t.Fatalf("恢复后应补上一份空用户表: 共 %d 份", len(got))
			}
			if recs := pendingDecommissions(t, h); len(recs) != 0 {
				t.Fatalf("确认后应删掉记录: %+v", recs)
			}
			h.tick()
			h.settle()
			if got := fake.received(); len(got) != 2 {
				t.Fatalf("确认之后不该再推,实际共 %d 份", len(got))
			}
		})
	}
}

// 停用时没送到下线通知,随后管理员又把它启用了:通知作废,不能再给正在服务的副机推空表。
func TestDecommissionDroppedAfterReenable(t *testing.T) {
	db := openDB(t, "x.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})
	fake := newDecomNode(t)
	node := model.Node{Name: "n1", ApiUrl: fake.srv.URL + "/app/", Token: "t", Enabled: true}
	db.Create(&node)
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	defer h.Stop()
	h.tick()
	h.settle()
	before, _ := BuildSnapshot(db, h.d.Setting)
	db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", false)
	fake.down.Store(true)
	_ = h.Decommission(node, before)
	fake.down.Store(false)
	db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", true)
	db.Model(&model.NodeDecommission{}).Where("node_id = ?", node.Id).Update("last_try", time.Now().Add(-time.Hour).Unix())
	h.tick()
	h.settle()
	for _, s := range fake.received() {
		if len(s.Users) == 0 {
			t.Fatal("重新启用后不该再收到空用户表")
		}
	}
	if recs := pendingDecommissions(t, h); len(recs) != 0 {
		t.Fatalf("重新启用后下线通知应作废: %+v", recs)
	}
}

// gzipAware 报不报 applyGzip 的副机替身:报了的用 DecodeSnapshot 读(和真副机同一段代码),没报的是老副机,
// 收到压缩包就当错误;记下每份快照与请求体字节数。
type gzipAware struct {
	srv      *httptest.Server
	gz       bool
	mu       sync.Mutex
	snaps    []Snapshot
	encoding []string
	wire     []int
}

func newGzipAware(t *testing.T, gz bool) *gzipAware {
	f := &gzipAware{gz: gz}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			raw, _ := io.ReadAll(r.Body)
			enc := r.Header.Get("Content-Encoding")
			var snap Snapshot
			var err error
			if f.gz {
				req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
				req.Header.Set("Content-Encoding", enc)
				snap, err = DecodeSnapshot(w, req)
			} else {
				err = json.Unmarshal(raw, &snap)
			}
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.snaps, f.encoding, f.wire = append(f.snaps, snap), append(f.encoding, enc), append(f.wire, len(raw))
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true, ApplyGzip: f.gz})
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// 每个修订只编码一次、所有副机共用(以前每台各自 json.Marshal 一整份,并发推 10 台时主机堆峰值 866 MB);
// 报过 applyGzip 的副机收压缩版,老副机照旧收不压缩的 JSON,两边解出来是同一份、各带自己的 selfNodeId。
func TestSnapshotEncodedOncePerRevision(t *testing.T) {
	db := openDB(t, "x.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Line{Name: "l1", Protocol: "shadowsocks", Port: 20001, Enabled: true})
	for i := 0; i < 300; i++ {
		u := model.User{Name: fmt.Sprintf("user%04d", i), Enabled: true, Credentials: []byte(`{"shadowsocks":{"password":"p` + fmt.Sprint(i) + `"}}`)}
		db.Create(&u)
		db.Create(&model.UserLine{UserId: u.Id, LineId: 1})
	}
	fakes := []*gzipAware{newGzipAware(t, true), newGzipAware(t, false), newGzipAware(t, true), newGzipAware(t, false)}
	for i, f := range fakes {
		db.Create(&model.Node{Name: fmt.Sprintf("n%d", i+2), ApiUrl: f.srv.URL + "/app/", Token: "t", Enabled: true})
	}
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	defer h.Stop()
	h.tick() // 第一轮还不知道谁收得了压缩包:都发不压缩的,顺便拉到报告
	h.settle()
	db.Model(&model.User{}).Where("name = ?", "user0000").Update("device_limit", 3) // 改配置:新修订
	start := wireEncodes.Load()
	h.tick()
	h.settle()
	if n := wireEncodes.Load() - start; n != 1 {
		t.Fatalf("同一修订推给 %d 台副机应只编码 1 次,实际 %d 次", len(fakes), n)
	}
	rev := h.Revision()
	for i, f := range fakes {
		f.mu.Lock()
		if len(f.snaps) != 2 {
			f.mu.Unlock()
			t.Fatalf("副机 %d 应收到两份快照,实际 %d", i+2, len(f.snaps))
		}
		got, enc, size := f.snaps[1], f.encoding[1], f.wire[1]
		plainSize := f.wire[0]
		f.mu.Unlock()
		if got.Revision != rev || got.SelfNodeId != uint(i+2) || len(got.Users) != 300 || got.Users[0].DeviceLimit != 3 {
			t.Fatalf("副机 %d 解出来的快照不对: rev=%s self=%d users=%d", i+2, got.Revision, got.SelfNodeId, len(got.Users))
		}
		if f.gz && (enc != "gzip" || size*3 > plainSize) {
			t.Fatalf("收得了压缩包的副机应收到 gzip(%q,%d 字节,不压缩时 %d 字节)", enc, size, plainSize)
		}
		if !f.gz && enc != "" {
			t.Fatalf("老副机不能收到压缩包: Content-Encoding=%q", enc)
		}
	}
}

// 同时推整份快照的副机数有上限:大快照十几台一起推,主机内存和出口带宽都扛不住。
func TestPushConcurrencyLimited(t *testing.T) {
	db := openDB(t, "x.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})
	var inFlight, peak, applies int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			n := atomic.AddInt32(&inFlight, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
					break
				}
			}
			time.Sleep(300 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			atomic.AddInt32(&applies, 1)
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true})
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	})
	for i := 0; i < 7; i++ {
		srv := httptest.NewServer(handler)
		defer srv.Close()
		db.Create(&model.Node{Name: fmt.Sprintf("n%d", i+2), ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true})
	}
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	defer h.Stop()
	h.tick()
	h.settle()
	if got := atomic.LoadInt32(&applies); got != 7 {
		t.Fatalf("7 台都应推到,实际 %d", got)
	}
	if p := atomic.LoadInt32(&peak); p > pushConcurrency {
		t.Fatalf("同时在推的副机最多 %d 台,实际峰值 %d", pushConcurrency, p)
	}
}

// 副机读快照:不压缩的、gzip 的(含主机拼出来的多段 gzip)都收;解压前后都不能超过上限 ——
// 几 MB 的压缩包能解出几个 G。
func TestDecodeSnapshotLimits(t *testing.T) {
	limit := ApplyBodyLimit
	defer func() { ApplyBodyLimit = limit }()
	ApplyBodyLimit = 64 << 10
	snap := Snapshot{Revision: "r1", Sequence: 7, Users: []model.User{{Name: "alice", Enabled: true}}}
	w, err := encodeWire(snap)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(body []byte, gz bool) (Snapshot, error) {
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		if gz {
			req.Header.Set("Content-Encoding", "gzip")
		}
		return DecodeSnapshot(httptest.NewRecorder(), req)
	}
	for _, gz := range []bool{false, true} {
		parts, err := w.parts(42, gz)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decode(bytes.Join(parts, nil), gz)
		if err != nil || got.Revision != "r1" || got.Sequence != 7 || got.SelfNodeId != 42 || len(got.Users) != 1 {
			t.Fatalf("gzip=%v 解出来的快照不对: %+v %v", gz, got, err)
		}
	}
	huge := append([]byte(`{"revision":"r1","users":[],"pad":"`), bytes.Repeat([]byte("x"), 1<<20)...)
	huge = append(huge, `"}`...)
	if _, err := decode(huge, false); err == nil {
		t.Fatal("不压缩的超过上限应报错")
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(huge)
	zw.Close()
	if buf.Len() > int(ApplyBodyLimit) {
		t.Fatalf("前提:压缩后要小于上限,实际 %d", buf.Len())
	}
	if _, err := decode(buf.Bytes(), true); err == nil {
		t.Fatal("解压后超过上限应报错")
	}
}

// 并账协程排队时每台只留最新一份:账本是累计值,旧的那份没来得及并也不丢流量。
func TestMergeQueueKeepsLatestPerNode(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	node := model.Node{Name: "n2", Enabled: true}
	db.Create(&node)
	db.Create(&model.User{Name: "bob", Enabled: true})
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	defer h.Stop()
	h.dbMu.Lock() // 并账协程卡在第一份上
	for _, v := range []int64{100, 200, 300} {
		h.enqueueResult(&nodeResult{n: node, rep: Report{Counters: []model.AgentCounter{{UserName: "bob", Up: v, Down: v}}}})
	}
	h.mergeMu.Lock()
	queued := len(h.mergeQueue) + len(h.mergePending)
	h.mergeMu.Unlock()
	h.dbMu.Unlock()
	h.waitMerged()
	if queued > 2 { // 第一份已经被取走在等锁,后两份合成一份
		t.Fatalf("同一台排队的报告应只留最新一份,实际 %d", queued)
	}
	var u model.User
	db.First(&u, "name = ?", "bob")
	if u.Up != 300 || u.Down != 300 {
		t.Fatalf("合并后按最新的累计值记账,应是 300/300,实际 %d/%d", u.Up, u.Down)
	}
}

// 报了收得了压缩包、压缩包却被拒(比如中间的反代动了请求体):当场改发不压缩的再试一次,成了以后这台就只发不压缩的,
// 不会一直被拒、一直退避。
func TestGzipRejectedFallsBackToPlain(t *testing.T) {
	db := openDB(t, "x.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})
	var plain, zipped int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			if r.Header.Get("Content-Encoding") == "gzip" {
				atomic.AddInt32(&zipped, 1)
				http.Error(w, `{"error":"invalid character"}`, http.StatusBadRequest)
				return
			}
			var snap Snapshot
			if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
				http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
				return
			}
			atomic.AddInt32(&plain, 1)
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true, ApplyGzip: true})
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	defer srv.Close()
	db.Create(&model.Node{Name: "n1", ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true})
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	defer h.Stop()
	round := func(limit int) {
		db.Model(&model.User{}).Where("name = ?", "alice").Update("device_limit", limit)
		h.tick()
		h.settle()
	}
	round(1) // 还不知道它收得了压缩包:发不压缩的
	round(2) // 报了收得了:先发压缩的,被拒,当场改发不压缩的
	if z, p := atomic.LoadInt32(&zipped), atomic.LoadInt32(&plain); z != 1 || p != 2 {
		t.Fatalf("压缩包被拒后应当场改发不压缩的: 压缩 %d 次、不压缩 %d 次", z, p)
	}
	round(3)
	if z, p := atomic.LoadInt32(&zipped), atomic.LoadInt32(&plain); z != 1 || p != 3 {
		t.Fatalf("之后这台应只发不压缩的: 压缩 %d 次、不压缩 %d 次", z, p)
	}
	h.mu.Lock()
	pushed, fail := h.pushed[2], h.pushFail[2]
	h.mu.Unlock()
	if pushed != h.Revision() || fail != nil {
		t.Fatalf("改发不压缩的成功了就算推到了,不该进退避: pushed=%s fail=%+v", pushed, fail)
	}
}
