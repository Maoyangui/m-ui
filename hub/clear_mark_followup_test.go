package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// ---- 老副机清账标记的几处跟进:先后不看时钟、没清成不打、看报告确认的也打、名单没变不再整份读游标 ----

// oldAgent 照 0.7.16 真代码的老副机替身(比 oldNode 多几样):应用快照时先落库(记下修订号、待重载),再清掉快照里没有的
// 名字的账本 —— deleteFails 为真时这条 DELETE 没删成(0.7.16 不看它的错误,照样确认);slow 是下一次应用里数据面重载要的
// 时长(用一次就清零),期间报告里是新修订、待重载。确认时不回 ledgerKept;报告带修订号、待重载,账本只有名字和计数。
type oldAgent struct {
	mu          sync.Mutex
	ledger      map[string]int64
	rev         string
	pending     bool
	deleteFails bool
	slow        time.Duration
	afterApply  func(snap Snapshot)
	srv         *httptest.Server
}

func newOldAgent(t *testing.T) *oldAgent {
	a := &oldAgent{ledger: map[string]int64{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			a.mu.Lock()
			a.rev, a.pending = snap.Revision, true
			if !a.deleteFails {
				keep := map[string]bool{}
				for _, u := range snap.Users {
					keep[u.Name] = true
				}
				for name := range a.ledger {
					if !keep[name] {
						delete(a.ledger, name)
					}
				}
			}
			slow, f := a.slow, a.afterApply
			a.slow = 0
			a.mu.Unlock()
			time.Sleep(slow)
			a.mu.Lock()
			a.pending = false
			a.mu.Unlock()
			if f != nil {
				f(snap)
			}
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			a.mu.Lock()
			rep := Report{Version: "0.7.16", CoreRunning: true, Revision: a.rev, ReloadPending: a.pending}
			for name, v := range a.ledger {
				rep.Counters = append(rep.Counters, model.AgentCounter{UserName: name, Up: v, Down: v})
			}
			a.mu.Unlock()
			json.NewEncoder(w).Encode(rep)
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *oldAgent) use(name string, v int64) {
	a.mu.Lock()
	a.ledger[name] += v
	a.mu.Unlock()
}

func (a *oldAgent) with(f func(a *oldAgent)) {
	a.mu.Lock()
	f(a)
	a.mu.Unlock()
}

// oldRig 一台本版主机 + 一台老副机 n1,走真实的一轮:推送(确认后打标记)→ 拉报告 → 并账。
type oldRig struct {
	t    *testing.T
	db   *gorm.DB
	a    *oldAgent
	node model.Node
	h    *Hub
}

func newOldRig(t *testing.T, users ...string) *oldRig {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	a := newOldAgent(t)
	node := model.Node{Name: "n1", ApiUrl: a.srv.URL + "/app/", Token: "t", Enabled: true}
	db.Create(&node)
	for i, name := range users {
		db.Create(&model.User{Name: name, Enabled: true, CreatedAt: int64(100 * (i + 1)), Credentials: []byte(`{}`)})
	}
	r := &oldRig{t: t, db: db, a: a, node: node}
	r.h = r.newHub()
	return r
}

// newHub 主机进程(重启就是再起一个)。
func (r *oldRig) newHub() *Hub {
	h := New(Deps{DB: r.db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	r.t.Cleanup(h.Stop)
	return h
}

func (r *oldRig) usage(name string) int64 {
	r.t.Helper()
	var u model.User
	if err := r.db.Where("name = ?", name).First(&u).Error; err != nil {
		r.t.Fatal(err)
	}
	return u.Down
}

func (r *oldRig) nodeDown() int64 {
	var v int64
	r.db.Model(&model.Stats{}).Where("resource = ? AND tag = ? AND direction = ?", "node", r.node.Name, false).
		Select("COALESCE(SUM(traffic), 0)").Scan(&v)
	return v
}

func (r *oldRig) cursor(name string) model.TrafficCursor {
	var c model.TrafficCursor
	r.db.Where("node_id = ? AND user_name = ?", r.node.Id, name).Limit(1).Find(&c)
	return c
}

func (r *oldRig) rename(from, to string) {
	r.t.Helper()
	if err := r.db.Model(&model.User{}).Where("name = ?", from).Update("name", to).Error; err != nil {
		r.t.Fatal(err)
	}
}

// disable 停用 n1:真实的下线推送(空用户表),老副机确认。
func (r *oldRig) disable() {
	r.t.Helper()
	before, err := BuildSnapshot(r.db, r.h.d.Setting)
	if err != nil {
		r.t.Fatal(err)
	}
	r.db.Model(&model.Node{}).Where("id = ?", r.node.Id).Update("enabled", false)
	if err := r.h.Decommission(r.node, before); err != nil {
		r.t.Fatal(err)
	}
}

func (r *oldRig) enable() {
	r.db.Model(&model.Node{}).Where("id = ?", r.node.Id).Update("enabled", true)
}

// ---- 1. 清账标记和报告的先后不拿两个时钟读数比 ----

// 打标记那台主机的钟比现在拉报告的这台快一小时:本机停用再启用老副机时钟快了一小时、之后被 NTP 拨回;或者标记是原主机打的,
// 随整库数据 / 副本到了钟慢一小时的新主机。以前比的是打标记时和拉报告时的 time.Now():这一小时里拉到的报告全被当成清账之前
// 就在路上的旧报告,按增量计、标记一直留着;等钟走过标记那一刻,再拉到的那份又被当成清账后重开的一本整本从 0 计 ——
// 重开那本一共 1600 计成 2100。这里用一个新起的主机进程拉报告(钟比标记慢),清账后重开的这本应从 0 计、只计一次。
func TestClearMarkOrderNotByWallClock(t *testing.T) {
	r := newOldRig(t, "alice")
	var u model.User
	r.db.Where("name = ?", "alice").First(&u)
	r.db.Model(&u).Updates(map[string]interface{}{"up": 1000, "down": 1000})
	ahead := time.Now().Add(time.Hour).UnixNano()
	r.db.Create(&model.TrafficCursor{NodeId: r.node.Id, UserName: "alice", Up: 1000, Down: 1000, UserId: u.Id, UserCreatedAt: u.CreatedAt,
		StampUp: 1000, StampDown: 1000, Cleared: true, ClearedAt: ahead})
	r.a.use("alice", 1200) // 老副机清账之后重开的那本
	r.h.RunOnce()
	if got := r.usage("alice"); got != 2200 {
		t.Fatalf("清账之后拉到的报告是重开的那本,应从 0 计 1200(共 2200),实际 %d", got)
	}
	r.a.use("alice", 300)
	r.h.RunOnce()
	r.a.use("alice", 100)
	r.h.RunOnce()
	if got := r.usage("alice"); got != 2600 {
		t.Fatalf("之后按增量,重开的那本 1600 只计一次(共 2600),实际 %d", got)
	}
}

// ---- 2. 老副机确认了却没清成:不打标记,不把整本账再计一遍 ----

// 0.7.16 的副机应用快照时清账本那条 DELETE 不看错误:库忙(SQLITE_BUSY)没删成也照样确认。停用推空用户表时它没清成,
// 主机却给这台的游标全打了清账标记;重新启用后第一份报告带着整本旧账,被当成清账后重开的一本从 0 计 —— 这台的历史
// 整本再计一遍(应计 1050,实计 2050)。确认之后紧接着拉一份报告核对:快照里没有的名字计数还不小于游标的就是没清,不打标记。
func TestOldAgentFailedClearOnDisableNotRecounted(t *testing.T) {
	r := newOldRig(t, "alice")
	r.a.use("alice", 1000)
	r.h.RunOnce()
	r.a.with(func(a *oldAgent) { a.deleteFails = true })
	r.disable()
	var once sync.Once
	r.a.with(func(a *oldAgent) {
		a.afterApply = func(snap Snapshot) {
			if len(snap.Users) > 0 { // 重新启用推回的完整快照:应用完、下一次拉取之前又用了 50
				once.Do(func() { r.a.use("alice", 50) })
			}
		}
	})
	r.enable()
	r.h.RunOnce()
	if got := r.usage("alice"); got != 1050 {
		t.Fatalf("老副机没清成,账本接着记:只计多出来的 50(共 1050),实际 %d", got)
	}
	r.a.use("alice", 10)
	r.h.RunOnce()
	if got := r.usage("alice"); got != 1060 {
		t.Fatalf("之后按增量,应是 1060,实际 %d", got)
	}
}

// 改名推过去、老副机清账的 DELETE 没删成:原名那本还在,紧接着拉到的报告里它不小于游标,不能当成清账后重开的一本再计一遍
// (以前服务器维度又多记 1000)。下一次推送它清成了,这时再打标记;原名以后给了新用户,新用户的那本从 0 计。
func TestOldAgentFailedClearOnRenameNotRecounted(t *testing.T) {
	r := newOldRig(t, "alice")
	r.a.use("alice", 1000)
	r.h.RunOnce()
	r.a.with(func(a *oldAgent) { a.deleteFails = true })
	r.rename("alice", "bob")
	r.h.RunOnce()
	if got := r.nodeDown(); got != 1000 {
		t.Fatalf("没清成的原名那本不能再计一遍:服务器维度应仍是 1000,实际 %d", got)
	}
	if c := r.cursor("alice"); c.Cleared {
		t.Fatalf("老副机没清成,不该打清账标记: %+v", c)
	}
	r.a.with(func(a *oldAgent) { a.deleteFails = false })
	r.db.Create(&model.User{Name: "carol", Enabled: true, CreatedAt: 300, Credentials: []byte(`{}`)})
	r.h.RunOnce() // 名单变了、再推一次:这次清成了
	if c := r.cursor("alice"); !c.Cleared {
		t.Fatalf("这次清成了,应打上清账标记: %+v", c)
	}
	r.db.Create(&model.User{Name: "alice", Enabled: true, CreatedAt: 400, Credentials: []byte(`{}`)})
	r.h.RunOnce()
	r.a.use("alice", 300)
	r.h.RunOnce()
	if got, node := r.usage("alice"), r.nodeDown(); got != 300 || node != 1300 {
		t.Fatalf("原名给了新用户,新的那本从 0 计:新 alice 应 300、服务器维度 1300,实际 %d、%d", got, node)
	}
}

// ---- 5. 名单没变、游标也没新添,推送时不再整份读游标 ----

// 推给老副机的每份快照都要看一眼有没有要打清账标记的:以前每次都把这台还没打标记的游标名单整个读出来(稳定时差不多是全部
// 用户,两万行上下),再把整份快照的用户名建成一张表。快照里的用户名单没变、这台的游标也没新添或去掉标记的,不再读。
func TestOldAgentClearMarkScanSkippedWhenUnchanged(t *testing.T) {
	r := newOldRig(t, "alice", "bob")
	r.a.use("alice", 100)
	r.a.use("bob", 100)
	r.h.RunOnce()
	if err := r.h.PushNow(r.node); err != nil { // 游标是刚建的:这一次照常看一眼
		t.Fatal(err)
	}
	var scans atomic.Int32
	count := func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "traffic_cursors") {
			scans.Add(1)
		}
	}
	if err := r.db.Callback().Query().After("gorm:query").Register("test:cursor_scans", count); err != nil {
		t.Fatal(err)
	}
	if err := r.db.Callback().Row().After("gorm:row").Register("test:cursor_scans_row", count); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := r.h.PushNow(r.node); err != nil {
			t.Fatal(err)
		}
	}
	if n := scans.Load(); n != 0 {
		t.Fatalf("用户名单没变、游标没新添:推送时不该再读游标,读了 %d 次", n)
	}
	r.rename("alice", "carol") // 名单变了照常看:原名打上标记
	if err := r.h.PushNow(r.node); err != nil {
		t.Fatal(err)
	}
	if c := r.cursor("alice"); !c.Cleared {
		t.Fatalf("改名推过去、老副机清掉了原名那本:应打上清账标记: %+v", c)
	}
}

// 名单没变,但并账给这台新建了一个快照里没有的名字的游标(已删用户在途的流量):下一次推送照样看、给它打上标记,
// 不能因为名单没变就跳过。
func TestOldAgentClearMarkAfterNewCursor(t *testing.T) {
	r := newOldRig(t, "alice")
	r.a.use("alice", 100)
	r.h.RunOnce()
	if err := r.h.PushNow(r.node); err != nil {
		t.Fatal(err)
	}
	r.a.use("ghost", 70) // 主机上没有这个用户:已删用户在途的流量
	r.h.RunOnce()
	if c := r.cursor("ghost"); c.Up != 70 || c.Cleared {
		t.Fatalf("并账应给 ghost 建游标: %+v", c)
	}
	if err := r.h.PushNow(r.node); err != nil { // 老副机应用完清掉了 ghost 那本
		t.Fatal(err)
	}
	if c := r.cursor("ghost"); !c.Cleared {
		t.Fatalf("快照里没有 ghost、老副机清掉了它的账本:应打上清账标记: %+v", c)
	}
}

// ---- 6. 等确认超时、看报告确认应用完的,同样打清账标记 ----

// 推送等确认超时、之后看报告确认它应用完了:老副机照样清掉了快照里没有的名字的账本。以前这条路不打清账标记,要等 10 分钟
// 一次的定期重推;这期间同一个名字又出现(改名后又改回),重开的那本第一份比游标小,被当成账本回退对齐不计
// (应计 1300,实计 1000,还误报回退)。
func TestOldAgentClearConfirmedByReportMarks(t *testing.T) {
	defer shortApplyTimeout(500 * time.Millisecond)()
	r := newOldRig(t, "alice")
	r.a.use("alice", 1000)
	r.h.RunOnce()
	r.a.with(func(a *oldAgent) { a.slow = 1500 * time.Millisecond })
	r.rename("alice", "bob")
	r.h.RunOnce() // 推送等确认超时:它其实在应用,已经清掉了 alice 那本
	time.Sleep(2 * time.Second)
	r.h.RunOnce() // 退避期内不重推;报告显示它已经在这个修订上、没有待重载:看报告确认
	r.h.mu.Lock()
	pushed := r.h.pushed[r.node.Id]
	r.h.mu.Unlock()
	if pushed == "" {
		t.Fatal("报告显示应用完了,应记为已推送")
	}
	if c := r.cursor("alice"); !c.Cleared {
		t.Fatalf("看报告确认应用完的快照里没有 alice,老副机清掉了她那本:应打上清账标记: %+v", c)
	}
	r.rename("bob", "alice") // 改回原名
	r.h.RunOnce()
	r.a.use("alice", 300)
	r.h.RunOnce()
	if got := r.usage("alice"); got != 1300 {
		t.Fatalf("改回原名后重开的那本应从 0 计:应是 1300,实际 %d", got)
	}
	if st := r.h.Statuses()[r.node.Id]; st.LedgerRegressUsers != 0 {
		t.Fatalf("清账后重开不是账本回退,不该告警: %+v", st)
	}
}
