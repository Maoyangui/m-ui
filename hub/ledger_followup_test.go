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

	"gorm.io/gorm"
)

// ---- 原主机变为副机:新主机等着它清空重开的那本账,第一份从 0 计 ----

// freshAgent 副机这边:账本纪元记成 epoch;fresh 为真时同时声明这本账是换纪元那一刻清空重开的(原主机变为副机时的做法)。
func (r *recreateRig) freshAgent(epoch string, fresh bool) {
	r.t.Helper()
	kv := map[string]string{LedgerEpochKey: epoch, LedgerFreshKey: ""}
	if fresh {
		kv[LedgerFreshKey] = epoch
	}
	for k, v := range kv {
		if err := upsertSetting(r.agent, k, v); err != nil {
			r.t.Fatal(err)
		}
	}
}

// mergeLocal 主机并副机现在的整本账(纪元、清空重开的声明都照副机报告里的)。
func (r *recreateRig) mergeLocal(at int64) (MergeResult, error) {
	r.t.Helper()
	l, err := LocalLedger(r.agent)
	if err != nil {
		r.t.Fatal(err)
	}
	return MergeReport(r.host, 2, "n2", l, at, 60, 1)
}

// 原主机当主机时本机流量直接记进用户用量,库里没有它自己的游标和纪元;变为副机时换一本新纪元的账。
// 以前新主机把第一份当成新的一本只建基线:从变为副机到第一次被拉取之间的流量(接任时要等原主机那一行重新启用,
// API 是 http 的要等管理员手动启用)一个字节都不计。新主机接任 / 被设为主机时给它记上 LedgerAwaitFresh,
// 原主机清空账本、声明从 0 开始,第一份就从 0 计;库里早年它当副机时留下的游标不算数。
func TestFormerMasterFreshLedgerCounted(t *testing.T) {
	r := newRecreateRig(t)
	u := r.newUser(100)
	r.sync()
	// 很早以前这台当副机时留下的游标(计数比现在这本大得多)
	r.host.Create(&model.TrafficCursor{NodeId: 2, UserName: "alice", Up: 5000, Down: 5000, UserId: u.Id, UserCreatedAt: 100,
		LedgerId: 42, StampUp: 5000, StampDown: 5000})
	if err := upsertSetting(r.host, LedgerEpochSetting(2), LedgerAwaitFresh); err != nil {
		t.Fatal(err)
	}
	r.freshAgent("e-former", true)
	r.use(700)
	if _, err := r.mergeLocal(1000); err != nil {
		t.Fatal(err)
	}
	if got := r.usage(u.Id); got != 700 {
		t.Fatalf("原主机变为副机后的第一份应从 0 计 700,实际 %d", got)
	}
	if v := settingValue(r.host, LedgerEpochSetting(2)); v != "e-former" {
		t.Fatalf("并过之后记下它的纪元,实际 %q", v)
	}
	r.use(70)
	r.mergeLocal(1005)
	r.mergeLocal(1010) // 重复的报告不重复计
	if got := r.usage(u.Id); got != 770 {
		t.Fatalf("之后按增量,应是 770,实际 %d", got)
	}
}

// 新主机等着的是"清空重开"的那本:报告没声明从 0 开始(老版本的原主机),或者纪元已经记在另一台上,都照老办法。
// 没有占位的副机(新加的、删了再加的)哪怕声明了也只建基线:它的账本可能早被别的主机计过。
func TestFormerMasterFreshLedgerGuards(t *testing.T) {
	for _, tc := range []struct {
		name  string
		await bool // 主机记着 LedgerAwaitFresh
		fresh bool // 副机声明清空重开
		want  int64
	}{
		{name: "没声明从 0 开始:只建基线", await: true, fresh: false, want: 0},
		{name: "主机没等它(删了再加):只建基线", await: false, fresh: true, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecreateRig(t)
			u := r.newUser(100)
			r.sync()
			if tc.await {
				upsertSetting(r.host, LedgerEpochSetting(2), LedgerAwaitFresh)
			}
			r.freshAgent("e-x", tc.fresh)
			r.use(700)
			if _, err := r.mergeLocal(1000); err != nil {
				t.Fatal(err)
			}
			if got := r.usage(u.Id); got != tc.want {
				t.Fatalf("应计 %d,实际 %d", tc.want, got)
			}
			r.use(30)
			r.mergeLocal(1005)
			if got := r.usage(u.Id); got != tc.want+30 {
				t.Fatalf("基线之后按增量,应是 %d,实际 %d", tc.want+30, got)
			}
		})
	}
	// 同一纪元已经记在另一台还在的副机上:不计入
	r := newRecreateRig(t)
	u := r.newUser(100)
	r.sync()
	r.host.Create(&model.Node{Name: "n3", Enabled: true})
	upsertSetting(r.host, LedgerEpochSetting(3), "e-dup")
	upsertSetting(r.host, LedgerEpochSetting(2), LedgerAwaitFresh)
	r.freshAgent("e-dup", true)
	r.use(700)
	var dup *DuplicateLedgerError
	if _, err := r.mergeLocal(1000); !errors.As(err, &dup) || r.usage(u.Id) != 0 {
		t.Fatalf("纪元记在另一台上应报重复账本、不计入: %v, %d", err, r.usage(u.Id))
	}
}

func settingValue(db *gorm.DB, key string) string {
	var v string
	db.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v
}

// ---- 副机库回滚跨过不止一次重开 ----

// 同一个人的账本在虚机快照之后重开了两次(L0 → L1 → L2),副机库回滚到快照、报回 L0:以前游标只记得上一本(L1),
// L0 被当成又重开的一本从 0 计,快照那本在快照时的累计量重计一遍(应计 1600,实计 2600)。
func TestAgentLedgerRollbackAcrossTwoReopens(t *testing.T) {
	r := newRecreateRig(t)
	r.newUser(100)
	r.sync()
	r.use(1000)
	r.report(1000)
	snap := r.ledger() // 虚机快照:L0
	total := int64(1000)
	for i, at := range []int64{1005, 1010, 1015, 1020} { // 之后重开四次(游标最多记得这么多本)
		r.wipeLedger()
		r.use(300)
		total += 300
		if res := r.report(at); res.Reopened != 1 {
			t.Fatalf("第 %d 次重开从 0 计: %+v", i+1, res)
		}
	}
	if got := r.usageOf("alice"); got != total {
		t.Fatalf("应是 %d,实际 %d", total, got)
	}
	r.wipeLedger()
	if err := r.agent.Create(&snap).Error; err != nil {
		t.Fatal(err)
	}
	res := r.report(1025)
	if got := r.usageOf("alice"); got != total || res.Regressed != 1 || res.Reopened != 0 {
		t.Fatalf("回到更早的那一本是账本回退,应对齐不计、告警,应是 %d,实际 %d(%+v)", total, got, res)
	}
	r.use(50)
	r.report(1030)
	if got := r.usageOf("alice"); got != total+50 {
		t.Fatalf("之后在这一本上照常按增量,应是 %d,实际 %d", total+50, got)
	}
}

// ---- 改名:副机上还没报上去的那段 ----

// 改名的快照推过去时,副机上次拉取之后记在原名下的流量还没报。以前副机应用快照时按"名字不在用户表"把原名那本删了,
// 这段永远不计(应计 1030,实计 1000);主机和副机失联期间改名更糟(用户照常连副机,没报的全丢:应计 6000,实计 1000)。
// 主人还在(只是改了名)的账本留着、照常上报,主机按主人计到改名后的用户头上。
func TestRenameKeepsUnreportedLedger(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unreported int64
	}{{"同一轮里没报的", 30}, {"失联期间改名", 5000}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecreateRig(t)
			u := r.newUser(100)
			r.sync()
			r.use(1000)
			r.report(1000)
			r.use(tc.unreported) // 还没报上去
			r.host.Model(&model.User{}).Where("id = ?", u.Id).Update("name", "bob")
			r.sync() // 推送在前、拉报告在后:先应用了改名的快照
			r.report(1005)
			if got := r.usage(u.Id); got != 1000+tc.unreported {
				t.Fatalf("改名前没报上去的 %d 应计到这个用户头上,应是 %d,实际 %d", tc.unreported, 1000+tc.unreported, got)
			}
			if err := model.AddAgentCounters(r.agent, []model.AgentCounter{{UserName: "bob", Up: 50, Down: 50}}, 100); err != nil {
				t.Fatal(err)
			}
			r.report(1010)
			r.report(1015)
			if got := r.usage(u.Id); got != 1000+tc.unreported+50 {
				t.Fatalf("改名后按新名字照常计,应是 %d,实际 %d", 1000+tc.unreported+50, got)
			}
			// 用户删掉后,两本账都清掉
			r.host.Delete(&model.User{}, u.Id)
			r.host.Create(&model.User{Name: "carol", Enabled: true, CreatedAt: 300})
			r.sync()
			var n int64
			r.agent.Model(&model.AgentCounter{}).Count(&n)
			if n != 0 {
				t.Fatalf("主人删掉后它的账本(原名、新名)都应清掉,还剩 %d 本", n)
			}
		})
	}
}

// 原名已经给了另一个人(改名后又有人用了原名):报来的原名那本主人还是改了名的那个人,按主人计,不算到新的同名用户头上;
// 主人已经删掉的照旧只记服务器维度。
func TestLedgerBilledToOwnerAfterRename(t *testing.T) {
	r := newRecreateRig(t)
	u := r.newUser(100)
	r.sync()
	r.use(1000)
	r.report(1000)
	r.use(40) // 没报上去
	r.host.Model(&model.User{}).Where("id = ?", u.Id).Update("name", "bob")
	other := r.newUser(200) // 新的 alice
	r.report(1005)          // 副机还没收到快照:报的是原来那个人的 alice
	if a, b := r.usage(u.Id), r.usage(other.Id); a != 1040 || b != 0 {
		t.Fatalf("原名那本的主人改名成了 bob:应计到 bob(1040),新 alice 不计;实际 bob %d、新 alice %d", a, b)
	}
	if got := r.nodeDown(); got != 1040 {
		t.Fatalf("服务器维度应是 1040,实际 %d", got)
	}
	var tags []string
	r.host.Model(&model.Stats{}).Where("resource = ? AND direction = ?", "user", false).Distinct().Order("tag").Pluck("tag", &tags)
	if strings.Join(tags, ",") != "alice,bob" { // 改名前那 1000 记在 alice 名下(当时就叫 alice);改名后这 40 记在 bob 名下
		t.Fatalf("改名后并进来的流量的时序应记在 bob 名下: %v", tags)
	}
	// 副机应用了快照:原名已经给了新 alice,前一个人那本清掉(老版本主机只认名字,留着报上去会算到新 alice 头上)
	r.sync()
	var c model.AgentCounter
	r.agent.Where("user_name = ?", "alice").Limit(1).Find(&c)
	if c.UserId == u.Id {
		t.Fatalf("原名给了别人后,前一个人那本应清掉: %+v", c)
	}
	r.use(25) // 新 alice 的流量:换一本从 0 记
	r.report(1010)
	if a, b := r.usage(u.Id), r.usage(other.Id); a != 1040 || b != 25 {
		t.Fatalf("新 alice 从 0 计:bob 应 1040、新 alice 应 25,实际 %d、%d", a, b)
	}
}

// ---- 老副机清账之后:按拉取时刻分清报告是清账前还是清账后的 ----

// oldNode 照 0.7.16 真代码的老副机替身:应用快照时把快照里没有的名字的账本清掉(停用推的空用户表就整本清掉),
// 确认时不回 ledgerKept;报告只有名字和计数。afterApply 在应用完、回确认之前调用(模拟用户在下一次拉取前已经用了多少)。
type oldNode struct {
	mu         sync.Mutex
	ledger     map[string]int64
	afterApply func(snap Snapshot)
	srv        *httptest.Server
}

func newOldNode(t *testing.T) *oldNode {
	n := &oldNode{ledger: map[string]int64{}}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			n.mu.Lock()
			keep := map[string]bool{}
			for _, u := range snap.Users {
				keep[u.Name] = true
			}
			for name := range n.ledger {
				if !keep[name] {
					delete(n.ledger, name)
				}
			}
			f := n.afterApply
			n.mu.Unlock()
			if f != nil {
				f(snap)
			}
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			n.mu.Lock()
			rep := Report{Version: "0.7.16", CoreRunning: true}
			for name, v := range n.ledger {
				rep.Counters = append(rep.Counters, model.AgentCounter{UserName: name, Up: v, Down: v})
			}
			n.mu.Unlock()
			json.NewEncoder(w).Encode(rep)
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *oldNode) use(name string, v int64) {
	n.mu.Lock()
	n.ledger[name] += v
	n.mu.Unlock()
}

// 本版主机配老副机:停用再启用、改名后又改回,老副机清掉的账本从 0 重开。以前清账标记只在计数变小时才认 ——
// 怕在路上的旧报告 —— 重开后第一份不小于旧游标时只计差值(停用再启用首份 1000:应计 2000 实计 1000;首份 1200:
// 应计 2200 实计 1200)。现在标记记下打标记的时刻,拉取晚于它的报告就是清账之后的那本,不管大小都从 0 计。
// 走真实的 hub 一轮:推送(确认后打标记)→ 拉报告 → 并账。
func TestOldAgentClearedLedgerCountedFromZero(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first int64
	}{{"首份等于旧游标", 1000}, {"首份超过旧游标", 1200}, {"首份小于旧游标", 300}} {
		t.Run(tc.name, func(t *testing.T) {
			db := openDB(t, "m.db").DB
			db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
			old := newOldNode(t)
			node := model.Node{Name: "n1", ApiUrl: old.srv.URL + "/app/", Token: "t", Enabled: true}
			db.Create(&node)
			u := model.User{Name: "alice", Enabled: true, CreatedAt: 100, Credentials: []byte(`{}`)}
			db.Create(&u)
			h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
			defer h.Stop()
			usage := func() int64 {
				var x model.User
				db.First(&x, u.Id)
				return x.Down
			}
			old.use("alice", 1000)
			h.RunOnce()
			if got := usage(); got != 1000 { // 老副机不报纪元,没有游标:按老办法从 0 计
				t.Fatalf("第一份应计 1000,实际 %d", got)
			}
			const base = 1000
			before, err := BuildSnapshot(db, h.d.Setting)
			if err != nil {
				t.Fatal(err)
			}
			db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", false)
			if err := h.Decommission(node, before); err != nil {
				t.Fatal(err)
			}
			// 重新启用:推回完整快照,老副机应用完、主机下一次拉取之前,alice 已经用了 first
			old.mu.Lock()
			old.afterApply = func(snap Snapshot) {
				if len(snap.Users) > 0 {
					old.mu.Lock()
					old.afterApply = nil
					old.ledger["alice"] += tc.first
					old.mu.Unlock()
				}
			}
			old.mu.Unlock()
			db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", true)
			h.RunOnce()
			if got := usage(); got != base+tc.first {
				t.Fatalf("清账后重开的一本应从 0 计:应是 %d,实际 %d", base+tc.first, got)
			}
			old.use("alice", 10)
			h.RunOnce()
			if got := usage(); got != base+tc.first+10 {
				t.Fatalf("之后按增量,应是 %d,实际 %d", base+tc.first+10, got)
			}
		})
	}
}

// 在路上的旧报告:拉取早于清账标记(老副机确认之前就拉到的那份,并账排在后面),里面还是清账前那本 ——
// 照常按增量、不消耗标记;之后拉到的清账后的那本照样从 0 计。
func TestOldAgentInflightReportBeforeClear(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "n2", Enabled: true})
	u := model.User{Name: "alice", Enabled: true, CreatedAt: 100}
	db.Create(&u)
	merge := func(fetched time.Time, n int64) MergeResult {
		t.Helper()
		res, err := MergeReport(db, 2, "n2", LedgerReport{Counters: []model.AgentCounter{{UserName: "alice", Up: n, Down: n}},
			FetchedAt: fetched.UnixNano()}, 1000, 60, 1)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	usage := func() int64 {
		var x model.User
		db.First(&x, u.Id)
		return x.Down
	}
	merge(time.Now(), 1000)
	inflight := time.Now() // 这一份在清账之前就拉到了
	time.Sleep(2 * time.Millisecond)
	if err := markLedgerCleared(db, 2, nil); err != nil { // 老副机确认了空用户表
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if res := merge(inflight, 1050); usage() != 1050 || res.Reopened != 0 {
		t.Fatalf("清账前拉到的旧报告按增量,应是 1050,实际 %d(%+v)", usage(), res)
	}
	var cur model.TrafficCursor
	db.Where("node_id = ? AND user_name = ?", 2, "alice").First(&cur)
	if !cur.Cleared {
		t.Fatalf("清账前的报告不消耗清账标记: %+v", cur)
	}
	if res := merge(time.Now(), 1050); usage() != 2100 || res.Reopened != 1 {
		t.Fatalf("清账后拉到的一本从 0 计(哪怕和游标一样大),应是 2100,实际 %d(%+v)", usage(), res)
	}
	merge(time.Now(), 1060)
	if usage() != 2110 {
		t.Fatalf("之后按增量,应是 2110,实际 %d", usage())
	}
}

// 清账标记按推过去的那份快照打:快照里还有这个名字(推送之后、确认之前主机上才删的),老副机没清它的账本,不能打标记 ——
// 打错了,清账后的报告从 0 计就会把这本账整本再计一遍。
func TestMarkLedgerClearedFollowsPushedSnapshot(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	old := newOldNode(t)
	node := model.Node{Name: "n1", ApiUrl: old.srv.URL + "/app/", Token: "t", Enabled: true}
	db.Create(&node)
	alice := model.User{Name: "alice", Enabled: true, CreatedAt: 100, Credentials: []byte(`{}`)}
	bob := model.User{Name: "bob", Enabled: true, CreatedAt: 200, Credentials: []byte(`{}`)}
	db.Create(&alice)
	db.Create(&bob)
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	defer h.Stop()
	old.use("alice", 100)
	old.use("bob", 100)
	h.RunOnce()
	snap, err := BuildSnapshot(db, h.d.Setting) // 快照里有 alice、bob
	if err != nil {
		t.Fatal(err)
	}
	db.Delete(&model.User{}, alice.Id) // 推送之前主机上删了 alice:这份快照里还有她
	if err := h.push(node, snap); err != nil {
		t.Fatal(err)
	}
	cleared := func(name string) bool {
		var cur model.TrafficCursor
		db.Where("node_id = ? AND user_name = ?", node.Id, name).First(&cur)
		return cur.Cleared
	}
	if cleared("alice") || cleared("bob") {
		t.Fatal("推过去的快照里还有 alice、bob,老副机没清她们的账本,不能打清账标记")
	}
	snap2, err := BuildSnapshot(db, h.d.Setting) // 没有 alice 了
	if err != nil {
		t.Fatal(err)
	}
	if err := h.push(node, snap2); err != nil {
		t.Fatal(err)
	}
	if !cleared("alice") || cleared("bob") {
		t.Fatal("推过去的快照里没有 alice:她的游标应打上清账标记,bob 不打")
	}
}
