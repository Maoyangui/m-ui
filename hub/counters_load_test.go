//go:build !race

// 这几条按真实规模造数(两万个计数器、六台副机),量的是写锁被占多久、别的写者等多久。
// 竞争检测(-race)下纯 Go 的 SQLite 慢几十倍,一块就要十几秒,时长断言没有意义,也会把 CI 的 race 步骤拖到十几分钟;
// 普通 go test 照跑(CI 的 test 步骤),只在 -race 那一步跳过。

package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// 副机每 5 秒报一次全部计数器,绝大多数没变化(审计 I4)。以前每一份都在一笔 BEGIN IMMEDIATE 里逐个查游标:
// 别的写事务占着锁时干等 10 秒然后 database is locked。没变化的计数器不该碰写锁。
func TestApplyCountersUnchangedTakesNoWriteLock(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "n2", Enabled: true})
	cs := manyCounters(t, db, 20000)
	now := time.Now().Unix()
	if _, err := ApplyCounters(db, 2, "n2", "", cs, now, 60, 1); err != nil {
		t.Fatal(err)
	}
	release := holdWriteLock(t, db)
	defer release()
	t0 := time.Now()
	merged, err := ApplyCounters(db, 2, "n2", "", cs, now+5, 60, 1)
	d := time.Since(t0)
	release()
	t.Logf("两万个没变化的计数器,写锁被占着时并账用了 %v", d)
	if err != nil || merged != 0 {
		t.Fatalf("没变化的报告不该失败、也不该记账: merged=%d err=%v", merged, err)
	}
	if d > 2*time.Second {
		t.Fatalf("两万个没变化的计数器等写锁等了 %v:不该去拿写锁", d)
	}
}

// 两万个计数器全有增量:以前一笔事务逐行处理、占着写锁好几秒,同时进来的面板写入、统计任务一直排不上。
// 现在按块提交、块之间让出写锁:并发的短写者最多等一小会儿,也不会报 database is locked;合计用量一分不差。
func TestApplyCountersChunksBoundLockTime(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "n2", Enabled: true})
	const n = 20000
	cs := manyCounters(t, db, n)
	now := time.Now().Unix()
	if _, err := ApplyCounters(db, 2, "n2", "", cs, now, 60, 1); err != nil {
		t.Fatal(err)
	}
	for i := range cs {
		cs[i].Up += 1000
		cs[i].Down += 2000
	}
	var worst time.Duration
	var fails int
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() { // 面板 / 统计任务:每 50 毫秒一笔短写
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			t0 := time.Now()
			if err := db.Transaction(func(tx *gorm.DB) error {
				return tx.Exec("UPDATE settings SET value = value WHERE key = 'x'").Error
			}); err != nil {
				fails++
			}
			if d := time.Since(t0); d > worst {
				worst = d
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	t0 := time.Now()
	merged, err := ApplyCounters(db, 2, "n2", "", cs, now+5, 60, 1)
	took := time.Since(t0)
	close(stop)
	<-writerDone
	t.Logf("两万个有增量的计数器并账用了 %v;并发短写者最长等 %v,失败 %d 次", took, worst, fails)
	if err != nil || merged != n {
		t.Fatalf("应并入 %d 个用户,实际 %d,err=%v", n, merged, err)
	}
	if fails > 0 || worst > time.Second {
		t.Fatalf("并账期间并发的短写者最长等了 %v、失败 %d 次:写锁不该被一口气占住", worst, fails)
	}
	up, down := usersTotal(t, db)
	if up != n*((1<<20)+1000) || down != n*((2<<20)+2000) {
		t.Fatalf("合计用量不对: %d/%d", up, down)
	}
}

// 某一块中途失败(这里用触发器让第二块里的一个用户写不进去):已提交的块不回滚,失败的那块整块回滚;
// 同一份报告再并一次,不重复记、不漏记。
func TestApplyCountersIdempotentAcrossChunkFailure(t *testing.T) {
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "n2", Enabled: true})
	const n = 2500
	cs := manyCounters(t, db, n)
	bad := cs[counterChunk+10].UserName // 第二块里的一个
	if err := db.Exec("CREATE TRIGGER users_broken BEFORE UPDATE ON users WHEN NEW.name = '" + bad + "' BEGIN SELECT RAISE(ABORT, 'disk full'); END").Error; err != nil {
		t.Fatal(err)
	}
	merged, err := ApplyCounters(db, 2, "n2", "", cs, 1000, 60, 1)
	if err == nil || merged != counterChunk {
		t.Fatalf("第二块应失败、第一块已并入: merged=%d err=%v", merged, err)
	}
	if up, _ := usersTotal(t, db); up != counterChunk*(1<<20) {
		t.Fatalf("只有第一块记了账: 上行合计 %d", up)
	}
	if err := db.Exec("DROP TRIGGER users_broken").Error; err != nil {
		t.Fatal(err)
	}
	merged, err = ApplyCounters(db, 2, "n2", "", cs, 1005, 60, 1)
	if err != nil || merged != n-counterChunk {
		t.Fatalf("重试应只并没并过的 %d 个: merged=%d err=%v", n-counterChunk, merged, err)
	}
	up, down := usersTotal(t, db)
	if up != n*(1<<20) || down != n*(2<<20) {
		t.Fatalf("重试后合计应一分不差: %d/%d", up, down)
	}
	if merged, err := ApplyCounters(db, 2, "n2", "", cs, 1010, 60, 1); err != nil || merged != 0 {
		t.Fatalf("同一份报告第三次不该再记账: merged=%d err=%v", merged, err)
	}
}

// 多台副机的报告背靠背并账(审计 I4):以前各台经 dbMu 串行、每台一笔大事务,单笔都不到 10 秒,
// 但两笔之间只有微秒级空档,别的写者的忙等抢不到,累计等满 10 秒照样 database is locked。
func TestMergeDoesNotStarveOtherWriters(t *testing.T) {
	const nodes, per = 6, 6000
	db := openDB(t, "m.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	manyCounters(t, db, per)
	var round int64 // 每份报告每个用户涨 1000
	report := func(w http.ResponseWriter) {
		v := atomic.LoadInt64(&round) * 1000
		cs := make([]model.AgentCounter, per)
		for i := range cs {
			cs[i] = model.AgentCounter{UserName: fmt.Sprintf("u%06d", i), Up: v, Down: v}
		}
		json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true, Counters: cs})
	}
	for k := 0; k < nodes; k++ {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/agent/apply"):
				var snap Snapshot
				_ = json.NewDecoder(r.Body).Decode(&snap)
				json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
			case strings.HasSuffix(r.URL.Path, "/agent/report"):
				report(w)
			default:
				w.Write([]byte(`{"ok":"1"}`))
			}
		}))
		defer srv.Close()
		db.Create(&model.Node{Name: fmt.Sprintf("n%d", k+2), ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true})
	}
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	defer h.Stop()
	atomic.StoreInt64(&round, 1)
	h.tick() // 第一份:各台都从 0 计
	h.settle()

	var worst time.Duration
	var fails int
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			t0 := time.Now()
			if err := db.Transaction(func(tx *gorm.DB) error {
				return tx.Exec("UPDATE settings SET value = value WHERE key = 'x'").Error
			}); err != nil {
				fails++
			}
			if d := time.Since(t0); d > worst {
				worst = d
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	atomic.StoreInt64(&round, 2)
	h.tick() // 第二份:各台每个用户都有增量
	h.settle()
	close(stop)
	<-writerDone
	t.Logf("%d 台副机各 %d 个有增量的计数器并账:并发短写者最长等 %v,失败 %d 次", nodes, per, worst, fails)
	if fails > 0 || worst > 1500*time.Millisecond {
		t.Fatalf("%d 台副机各 %d 个有增量的计数器并账时,并发短写者最长等了 %v、失败 %d 次", nodes, per, worst, fails)
	}
	up, _ := usersTotal(t, db)
	if want := int64(nodes) * per * 2000; up != want {
		t.Fatalf("合计上行应是 %d,实际 %d", want, up)
	}
}
