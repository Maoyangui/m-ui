package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func openDB(t *testing.T, name string) *database.DBHandle {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	return &database.DBHandle{DB: db}
}

func TestSnapshotRevisionAndApply(t *testing.T) {
	src := openDB(t, "hub.db").DB
	src.Create(&model.Node{Name: "香港", Domain: "hk.x", IsLocal: true, Token: "secret"})
	src.Create(&model.Node{Name: "台湾", Domain: "tw.x", ApiUrl: "https://tw:2053/ad/", Token: "tok"})
	src.Create(&model.Upstream{Name: "warp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`)})
	src.Create(&model.Line{Name: "l1", Protocol: "hysteria2", Port: 1000, UpstreamId: 1, Enabled: true})
	src.Create(&model.User{Name: "alice", Enabled: true, Up: 999, Down: 999, Credentials: []byte(`{"hysteria2":{"password":"p"}}`), DeviceLimit: 2})
	src.Create(&model.UserLine{UserId: 1, LineId: 1})
	setting := func(k string) string { return map[string]string{"subProfileTitle": "maoyang"}[k] }

	s1, err := BuildSnapshot(src, setting)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := BuildSnapshot(src, setting)
	if s1.Revision != s2.Revision {
		t.Fatal("修订号应稳定")
	}
	for _, n := range s1.Nodes {
		if n.Token != "" {
			t.Fatal("快照不应包含令牌")
		}
	}
	if s1.Users[0].Up != 0 || s1.Users[0].Down != 0 {
		t.Fatal("快照不应携带主机计量")
	}
	src.Model(&model.User{}).Where("name = ?", "alice").Update("enabled", false)
	s3, _ := BuildSnapshot(src, setting)
	if s3.Revision == s1.Revision {
		t.Fatal("禁用用户后修订号应变化")
	}
	// 只改流量不应改修订号
	src.Model(&model.User{}).Where("name = ?", "alice").Update("up", 12345)
	s4, _ := BuildSnapshot(src, setting)
	if s4.Revision != s3.Revision {
		t.Fatal("仅流量变化不应改修订号")
	}

	// 应用到副机库:整表替换、保留账本、标记本机
	dst := openDB(t, "node.db").DB
	dst.Create(&model.Line{Name: "stale", Protocol: "shadowsocks", Port: 5})
	dst.Create(&model.AgentCounter{UserName: "alice", Up: 10, Down: 20})
	s4.SelfNodeId = 2
	changed, upsChanged, err := ApplySnapshot(dst, s4)
	if err != nil || !changed || !upsChanged {
		t.Fatalf("应用失败: %v lines=%v ups=%v", err, changed, upsChanged)
	}
	var lines []model.Line
	dst.Find(&lines)
	if len(lines) != 1 || lines[0].Name != "l1" || lines[0].Id != 1 {
		t.Fatalf("线路未整表替换: %+v", lines)
	}
	var nodes []model.Node
	dst.Order("id").Find(&nodes)
	if len(nodes) != 2 || nodes[0].IsLocal || !nodes[1].IsLocal {
		t.Fatalf("副机应把自己标为本机: %+v", nodes)
	}
	var ac model.AgentCounter
	if dst.First(&ac).Error != nil || ac.Up != 10 {
		t.Fatal("账本应保留")
	}
	var u model.User
	dst.First(&u)
	if u.Enabled || u.DeviceLimit != 2 || len(u.Credentials) == 0 {
		t.Fatalf("用户字段未同步: %+v", u)
	}
	var rev string
	dst.Raw("SELECT value FROM settings WHERE key='hubRevision'").Scan(&rev)
	if rev != s4.Revision {
		t.Fatal("应记录已应用修订号")
	}
	var title string
	dst.Raw("SELECT value FROM settings WHERE key='subProfileTitle'").Scan(&title)
	if title != "maoyang" {
		t.Fatal("同步设置未写入")
	}
	// 再次应用相同快照:线路与上游都未变
	changed, upsChanged, err = ApplySnapshot(dst, s4)
	if err != nil || changed || upsChanged {
		t.Fatalf("相同快照不应判定变化: %v lines=%v ups=%v", err, changed, upsChanged)
	}
	// 只改上游:线路不变、上游变
	src.Model(&model.Upstream{}).Where("name = ?", "warp").Update("options", []byte(`{"server":"127.0.0.1","server_port":40001}`))
	s5, _ := BuildSnapshot(src, setting)
	s5.SelfNodeId = 2
	changed, upsChanged, err = ApplySnapshot(dst, s5)
	if err != nil || changed || !upsChanged {
		t.Fatalf("仅上游变化应只标记上游: %v lines=%v ups=%v", err, changed, upsChanged)
	}
}

func TestApplyCountersCursor(t *testing.T) {
	db := openDB(t, "hub.db").DB
	db.Create(&model.User{Name: "bob", Enabled: true})
	c := []model.AgentCounter{{UserName: "bob", Up: 100, Down: 200}}
	if n, err := ApplyCounters(db, 2, "台湾", c, 1000, 60, 1); err != nil || n != 1 {
		t.Fatalf("首次并入: %v %d", err, n)
	}
	var u model.User
	db.First(&u)
	if u.Up != 100 || u.Down != 200 || u.OnlineAt != 1000 {
		t.Fatalf("首次应全额计入: %+v", u)
	}
	// 增量
	c[0].Up, c[0].Down = 150, 260
	ApplyCounters(db, 2, "台湾", c, 1060, 60, 1)
	db.First(&u)
	if u.Up != 150 || u.Down != 260 {
		t.Fatalf("应只计增量: %+v", u)
	}
	// 无变化 → 不计
	if n, _ := ApplyCounters(db, 2, "台湾", c, 1120, 60, 1); n != 0 {
		t.Fatal("无增量不应计入")
	}
	// 回绕(副机重装):计数器变小 → 游标归零,全额计入
	c[0].Up, c[0].Down = 5, 7
	ApplyCounters(db, 2, "台湾", c, 1180, 60, 1)
	db.First(&u)
	if u.Up != 155 || u.Down != 267 {
		t.Fatalf("回绕后应重认: %+v", u)
	}
	var cur model.TrafficCursor
	db.Where("node_id = 2").First(&cur)
	if cur.Up != 5 || cur.Down != 7 {
		t.Fatalf("游标应跟随: %+v", cur)
	}
	var st []model.Stats
	db.Where("resource = ? AND tag = ?", "node", "台湾").Find(&st)
	if len(st) == 0 {
		t.Fatal("应记录副机流量时序")
	}
	// 不同副机独立游标
	ApplyCounters(db, 3, "日本", []model.AgentCounter{{UserName: "bob", Up: 1, Down: 1}}, 1240, 60, 1)
	db.First(&u)
	if u.Up != 156 || u.Down != 268 {
		t.Fatalf("多副机应各自计入: %+v", u)
	}
	// 倍率:2 倍服务器的 100/200 增量计为 200/400
	ApplyCounters(db, 4, "贵", []model.AgentCounter{{UserName: "bob", Up: 100, Down: 200}}, 1300, 60, 2)
	db.First(&u)
	if u.Up != 356 || u.Down != 668 {
		t.Fatalf("倍率应按 2 倍计入: %+v", u)
	}
	var cur4 model.TrafficCursor
	db.Where("node_id = 4").First(&cur4)
	if cur4.Up != 100 || cur4.Down != 200 {
		t.Fatalf("游标应记录原始计数而非倍率后的值: %+v", cur4)
	}
}

// 代理随快照下发副机(供订阅页文案用),但不带密码与 2FA;开关不能被 gorm 默认值翻回 true。
func TestSnapshotCarriesResellers(t *testing.T) {
	src, err := database.Open(filepath.Join(t.TempDir(), "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(src)
	dst, err := database.Open(filepath.Join(t.TempDir(), "dst.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(dst)

	src.Create(&model.Reseller{Name: "dl1", Enabled: true, Password: "$2a$hash", TotpSecret: "SECRET", PageTitle: "DL1"})
	// 代理在自己面板里关掉了订阅页与临时共享(gorm 的 default:true 只对 Create 生效,这里用 map 更新)
	src.Model(&model.Reseller{}).Where("id = ?", 1).
		Updates(map[string]interface{}{"page_enabled": false, "share_on": false})
	snap, err := BuildSnapshot(src, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Resellers) != 1 || snap.Resellers[0].Password != "" || snap.Resellers[0].TotpSecret != "" {
		t.Fatalf("密码与 2FA 不应下发: %+v", snap.Resellers)
	}
	if _, _, err := ApplySnapshot(dst, snap); err != nil {
		t.Fatal(err)
	}
	var got model.Reseller
	dst.First(&got)
	if got.Name != "dl1" || got.PageTitle != "DL1" || got.PageEnabled || got.ShareOn {
		t.Fatalf("副机上的代理不对: %+v", got)
	}
}

// 副机被删后,它的在线线路与名字要一起清掉,否则用户的"在线设备"里会一直挂着
// 一台已经不存在的服务器的线路。
func TestHubForgetsRemovedNodes(t *testing.T) {
	h := &Hub{
		status:      map[uint]*NodeStatus{1: {Name: "旧机"}},
		remote:      map[uint]map[string][]string{1: {"u": {"1.2.3.4"}}},
		pushed:      map[uint]string{1: "rev"},
		remoteLines: map[uint]map[string]map[string][]string{1: {"u": {"1.2.3.4": {"香港1"}}}},
		nodeNames:   map[uint]string{1: "旧机"},
	}
	if got := h.RemoteIPLines("u"); len(got) == 0 {
		t.Fatal("清理前应能取到线路")
	}
	h.forgetNodes(map[uint]bool{}) // 一台都不再存活
	if len(h.status) != 0 || len(h.remote) != 0 || len(h.pushed) != 0 ||
		len(h.remoteLines) != 0 || len(h.nodeNames) != 0 {
		t.Fatalf("所有按节点的缓存都要清空: %+v", h)
	}
	if got := h.RemoteIPLines("u"); len(got) != 0 {
		t.Fatalf("已删服务器的线路不该再出现: %v", got)
	}
}

// fakeNode 模拟一台副机的 agent 接口:可指定报告延迟、是否失败,并数一数被推送了几次。
type fakeNode struct {
	srv      *httptest.Server
	applies  int32
	delay    time.Duration
	failRep  bool
	counters []model.AgentCounter
}

func newFakeNode(t *testing.T, delay time.Duration, failRep bool, counters []model.AgentCounter) *fakeNode {
	t.Helper()
	f := &fakeNode{delay: delay, failRep: failRep, counters: counters}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			atomic.AddInt32(&f.applies, 1)
			w.Write([]byte(`{"ok":"1","revision":"x"}`))
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			time.Sleep(f.delay)
			if f.failRep {
				http.Error(w, `{"error":"boom"}`, 500)
				return
			}
			json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true, Counters: f.counters,
				Onlines: map[string][]string{"alice": {"9.9.9.9"}}})
		case strings.HasSuffix(r.URL.Path, "/agent/external-ips"):
			w.Write([]byte(`{"ok":"1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// 各副机并发同步:两台各慢 1 秒的机器不该让整轮变成 2 秒以上;失败的机器不影响正常机器;
// 同一修订号只推一次;流量按游标并入,连跑两轮不会重复计费;不留 goroutine。
func TestTickSyncsNodesConcurrently(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, DeviceLimit: 3, Credentials: []byte(`{}`)})

	ctr := []model.AgentCounter{{UserName: "alice", Up: 100, Down: 200}}
	fakes := []*fakeNode{
		newFakeNode(t, 0, false, ctr),           // 正常
		newFakeNode(t, time.Second, false, ctr), // 慢
		newFakeNode(t, time.Second, false, ctr), // 慢
		newFakeNode(t, 0, true, nil),            // 拉报告失败
		newFakeNode(t, 0, false, nil),           // 正常
	}
	for i, f := range fakes {
		db.Create(&model.Node{Name: fmt.Sprintf("n%d", i+1), ApiUrl: f.srv.URL + "/app/", Token: "t", Enabled: true})
	}
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})

	before := runtime.NumGoroutine()
	start := time.Now()
	h.tick()
	elapsed := time.Since(start)
	if elapsed > 1800*time.Millisecond {
		t.Fatalf("两台各慢 1 秒的机器应并发等待,整轮用了 %v", elapsed)
	}
	st := h.Statuses()
	for i, f := range fakes {
		id := uint(i + 2)
		if f.failRep {
			if st[id].OK || !strings.Contains(st[id].Error, "拉取报告失败") {
				t.Fatalf("失败机器状态不对: %+v", st[id])
			}
			continue
		}
		if !st[id].OK || !st[id].CoreRunning {
			t.Fatalf("正常机器 %d 不该被失败机器拖累: %+v", id, st[id])
		}
	}
	h.tick() // 第二轮:修订号没变,不该再推;报告里同样的计数器不该再记一次
	for i, f := range fakes {
		if got := atomic.LoadInt32(&f.applies); got != 1 {
			t.Fatalf("机器 %d 应只被推送一次,实际 %d", i+1, got)
		}
	}
	var u model.User
	db.Where("name = ?", "alice").First(&u)
	if u.Up != 300 || u.Down != 600 {
		t.Fatalf("三台带计数器的机器各 100/200,应并入 300/600 且第二轮不重复,实际 %d/%d", u.Up, u.Down)
	}
	if ips := h.RemoteIPs("alice"); len(ips) != 1 {
		t.Fatalf("在线 IP 应汇总去重为 1 个,实际 %v", ips)
	}
	// 多出来的只该是 keep-alive 连接两头的读写协程:关掉空闲连接后应回落到同步前的水平
	for _, c := range h.clients {
		c.CloseIdleConnections()
	}
	time.Sleep(200 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+3 {
		t.Fatalf("同步后多出 goroutine: %d → %d", before, after)
	}
}
