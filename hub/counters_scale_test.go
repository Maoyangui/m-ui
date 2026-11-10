package hub

import (
	"encoding/json"
	"errors"
	"fmt"
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

// manyCounters 建 n 个用户(主机侧),返回副机账本里他们各自的计数(上行 1 MB、下行 2 MB)。
func manyCounters(t *testing.T, db *gorm.DB, n int) []model.AgentCounter {
	t.Helper()
	us := make([]model.User, 0, n)
	cs := make([]model.AgentCounter, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("u%06d", i)
		us = append(us, model.User{Name: name, Enabled: true})
		cs = append(cs, model.AgentCounter{UserName: name, Up: 1 << 20, Down: 2 << 20})
	}
	if err := db.CreateInBatches(&us, 500).Error; err != nil {
		t.Fatal(err)
	}
	return cs
}

// holdWriteLock 起一笔写事务一直占着写锁(模拟另一笔长事务),返回放锁的函数。
func holdWriteLock(t *testing.T, db *gorm.DB) (release func()) {
	t.Helper()
	held, done := make(chan struct{}), make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("UPDATE settings SET value = value WHERE key = 'held'").Error; err != nil {
				t.Error(err)
			}
			close(held)
			<-done
			return nil
		})
	}()
	<-held
	var once sync.Once
	return func() { once.Do(func() { close(done); <-finished }) }
}

func usersTotal(t *testing.T, db *gorm.DB) (up, down int64) {
	t.Helper()
	if err := db.Model(&model.User{}).Select("COALESCE(SUM(up),0)").Scan(&up).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.User{}).Select("COALESCE(SUM(down),0)").Scan(&down).Error; err != nil {
		t.Fatal(err)
	}
	return
}

// 副机断电(synchronous=NORMAL 丢了最后几笔提交)或虚机快照回滚,账本比上次小一点(审计 I4):
// 以前一律当成回绕,游标归零后把整段历史再记一遍(100 GB 的用户多记 400 GB)。同一个用户的计数变小按账本回退处理:
// 游标对齐到现在的值,这一次不计,之后照常按增量计。
func TestApplyCountersRegressionDoesNotRecount(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "n2", Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true})
	const gb, mb = int64(1) << 30, int64(1) << 20
	report := func(up, down, at int64) {
		t.Helper()
		if _, err := ApplyCounters(db, 2, "n2", "e1", []model.AgentCounter{{UserName: "alice", Up: up, Down: down}}, at, 60, 1); err != nil {
			t.Fatal(err)
		}
	}
	usage := func() int64 {
		var u model.User
		db.First(&u, "name = ?", "alice")
		return u.Up + u.Down
	}
	report(100*gb, 300*gb, 1000) // 这本账第一次出现:只建基线
	report(100*gb+10*mb, 300*gb+10*mb, 1005)
	if got := usage(); got != 20*mb {
		t.Fatalf("基线之后应只计增量 20 MB,实际 %d", got)
	}
	report(100*gb+5*mb, 300*gb+5*mb, 1010) // 副机断电重启:账本回到 5 秒前
	if got := usage(); got != 20*mb {
		t.Fatalf("账本回退不该把整段历史再记一遍:应仍是 20 MB,实际 %.1f GB", float64(got)/float64(gb))
	}
	report(100*gb+6*mb, 300*gb+6*mb, 1015) // 之后照常按增量
	if got := usage(); got != 22*mb {
		t.Fatalf("回退之后应从对齐后的游标接着计(+2 MB),实际 %d MB", got/mb)
	}
}

// deleteNodeRows 照服务器页「删除」的事务删掉副机及它的游标、账本纪元。
func deleteNodeRows(t *testing.T, db *gorm.DB, id uint) {
	t.Helper()
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&model.Node{}, id).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", id).Delete(&model.TrafficCursor{}).Error; err != nil {
			return err
		}
		return tx.Where("key = ?", LedgerEpochSetting(id)).Delete(&model.Setting{}).Error
	}); err != nil {
		t.Fatal(err)
	}
}

// 删除副机时它正好有一轮报告在途(审计 I4):删库事务清掉游标和纪元之后,在途那一份又把它们写回已删的 id;
// 同一台机器重新加回(新 id)时被认成"同一本账记在另一台副机上",流量永久不计。已删副机的报告要整份丢弃,
// 重新加回的机器先建基线、之后照常计。
func TestApplyCountersDropsReportForDeletedNode(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "n2", Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true})
	ctr := func(v int64) []model.AgentCounter { return []model.AgentCounter{{UserName: "alice", Up: v, Down: v}} }
	if _, err := ApplyCounters(db, 2, "n2", "machineA", ctr(100), 1000, 60, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyCounters(db, 2, "n2", "machineA", ctr(200), 1005, 60, 1); err != nil {
		t.Fatal(err)
	}
	deleteNodeRows(t, db, 2)
	// 在途那一轮的结果此刻才落库
	if _, err := ApplyCounters(db, 2, "n2", "machineA", ctr(300), 1010, 60, 1); err != nil {
		t.Fatalf("已删副机的报告应安静地丢掉: %v", err)
	}
	var cursors, epochs int64
	db.Model(&model.TrafficCursor{}).Where("node_id = ?", 2).Count(&cursors)
	db.Model(&model.Setting{}).Where("key = ?", LedgerEpochSetting(2)).Count(&epochs)
	if cursors != 0 || epochs != 0 {
		t.Fatalf("已删副机不该留下游标(%d)和账本纪元(%d)", cursors, epochs)
	}
	var u model.User
	db.First(&u, "name = ?", "alice")
	if u.Up != 100 {
		t.Fatalf("已删副机的在途报告不该再记账,alice 上行应仍是 100,实际 %d", u.Up)
	}

	// 同一台机器重新加回来(新 id 3):第一份只建基线,之后照常计
	db.Create(&model.Node{Name: "n3", Enabled: true})
	for i, v := range []int64{400, 500} {
		if _, err := ApplyCounters(db, 3, "n3", "machineA", ctr(v), 1015+int64(i)*5, 60, 1); err != nil {
			var dup *DuplicateLedgerError
			t.Fatalf("重新加回的副机第 %d 份报告不该被当成重复账本(dup=%v): %v", i+1, errors.As(err, &dup), err)
		}
	}
	db.First(&u, "name = ?", "alice")
	if u.Up != 200 || u.Down != 200 {
		t.Fatalf("重新加回后基线之后的 100 应计入,alice 应是 200/200,实际 %d/%d", u.Up, u.Down)
	}
}

// 老版本留下的孤儿纪元(指向已经不存在的副机)不能挡住同一台机器重新加回。
func TestGhostLedgerEpochDoesNotBlockReadd(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "n2", Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true})
	db.Create(&model.Setting{Key: LedgerEpochSetting(7), Value: "machineA"}) // 副机 #7 早已删除
	ctr := func(v int64) []model.AgentCounter { return []model.AgentCounter{{UserName: "alice", Up: v, Down: v}} }
	for i, v := range []int64{100, 150} {
		if _, err := ApplyCounters(db, 2, "n2", "machineA", ctr(v), 1000+int64(i)*5, 60, 1); err != nil {
			t.Fatalf("孤儿纪元不该让这台的流量不计: %v", err)
		}
	}
	var u model.User
	db.First(&u, "name = ?", "alice")
	if u.Up != 50 {
		t.Fatalf("基线之后的 50 应计入,实际 %d", u.Up)
	}
}

// 没有任何配置变化的一轮 tick(审计 I4):以前也要开一笔 BEGIN IMMEDIATE 去读推送序号,
// 别的写事务占锁超过 10 秒时这一轮"构造同步快照失败",推送、拉报告、设备租约整轮跳过。
func TestIdleRefreshTakesNoWriteLock(t *testing.T) {
	db := openDB(t, "x.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	defer h.Stop()
	first, err := h.refreshSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	release := holdWriteLock(t, db)
	defer release()
	t0 := time.Now()
	snap, err := h.refreshSnapshot()
	d := time.Since(t0)
	release()
	if err != nil || d > 2*time.Second {
		t.Fatalf("修订号没变的刷新等写锁等了 %v: %v", d, err)
	}
	if snap.Revision != first.Revision || snap.Sequence != first.Sequence {
		t.Fatalf("修订号没变,序号也不该变: %s/%d → %s/%d", first.Revision, first.Sequence, snap.Revision, snap.Sequence)
	}
}

// 并账排队(别的副机的长事务把落库占着)时,这台副机的闸不能一直被在途那轮拿着(审计 I4):
// 停用 / 删除时的下线推送以前要等满 25 秒才放弃,空用户表根本没发出去,那台副机继续按旧用户表为所有人服务。
func TestDecommissionNotBlockedByMergeQueue(t *testing.T) {
	db := openDB(t, "x.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})
	var reports, emptyPushes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			if len(snap.Users) == 0 {
				atomic.AddInt32(&emptyPushes, 1)
			}
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			atomic.AddInt32(&reports, 1)
			json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true})
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	defer srv.Close()
	node := model.Node{Name: "n1", ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true}
	db.Create(&node)
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	defer h.Stop()
	before, err := BuildSnapshot(db, h.d.Setting)
	if err != nil {
		t.Fatal(err)
	}
	h.dbMu.Lock() // 模拟别的副机的并账把落库占住
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			h.dbMu.Unlock()
		}
	}
	defer unlock()
	h.tick()
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&reports) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", false) // 管理员停用它
	t0 := time.Now()
	err = h.Decommission(node, before)
	d := time.Since(t0)
	unlock()
	h.settle()
	if err != nil || d > 3*time.Second {
		t.Fatalf("并账排队不该拖住下线推送: 用了 %v, err=%v", d, err)
	}
	if got := atomic.LoadInt32(&emptyPushes); got != 1 {
		t.Fatalf("副机应收到一份空用户表,实际 %d 份", got)
	}
}

// slowApplyDelay 假副机处理一次推送要多久:比主机等应用确认的时限(测试里调成 1 秒)长一点。
const slowApplyDelay = 2 * time.Second

// shortApplyTimeout 测试里把等应用确认的时限调短,返回恢复函数。
func shortApplyTimeout(d time.Duration) (restore func()) {
	base, per := applyTimeoutBase, applyTimeoutPerMB
	applyTimeoutBase, applyTimeoutPerMB = d, 0
	return func() { applyTimeoutBase, applyTimeoutPerMB = base, per }
}

// 副机处理一份大快照(整表替换 + 渲染 + 热更新)超过主机的请求超时(审计 I4):副机其实应用成功了,
// 但主机记成"推送失败"且不退避,下一轮又整份重推 —— 副机永远在重做同一份。超时之后看报告:
// 副机已经在这个修订上、没有待重载,就记为已推送;还没好就退避,不再立刻重推。
func TestSlowApplyIsNotRepushedForever(t *testing.T) {
	defer shortApplyTimeout(time.Second)()
	db := openDB(t, "x.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})
	var applies int32
	var mu, applyMu sync.Mutex // applyMu:真副机的 agentApplyMu,一次只应用一份
	nodeRev, pending := "", false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			applyMu.Lock()
			defer applyMu.Unlock()
			atomic.AddInt32(&applies, 1)
			mu.Lock()
			nodeRev, pending = snap.Revision, true // 落库就写修订号、记待重载
			mu.Unlock()
			time.Sleep(slowApplyDelay) // 渲染 + 热更新;老副机不看主机断没断开,照样做完
			mu.Lock()
			pending = false
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			mu.Lock()
			rep := Report{Version: "t", CoreRunning: true, Revision: nodeRev, ReloadPending: pending}
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
	deadline := time.Now().Add(4 * slowApplyDelay)
	for time.Now().Before(deadline) {
		h.tick()
		h.settle()
		h.mu.Lock()
		done := h.pushed[2] != ""
		h.mu.Unlock()
		if done {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.mu.Lock()
	pushed := h.pushed[2]
	h.mu.Unlock()
	if got := atomic.LoadInt32(&applies); got != 1 {
		t.Fatalf("同一份快照应只整份应用 1 次,实际 %d 次", got)
	}
	if pushed == "" {
		t.Fatal("副机报告已经在这一修订上、没有待重载,应记为已推送")
	}
}
