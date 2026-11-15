package ha

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
)

// withoutHistory 去掉"历史数据没带过来"的告警(只看身份迁移本身的告警的测试用:那些测试里备用没收过历史数据)。
func withoutHistory(ws []PlanItem) []PlanItem {
	var out []PlanItem
	for _, w := range ws {
		if !strings.HasPrefix(w.Code, "history") {
			out = append(out, w)
		}
	}
	return out
}

// historyFor 主机生成一份历史数据、封给备用(kind 换成别的可以造"不是历史数据"的信封)。
func historyFor(t *testing.T, m, c *Service, seq uint64, kind string) []byte {
	t.Helper()
	dir := t.TempDir()
	zp, meta, err := m.buildHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(zp)
	k, err := c.standbyKey()
	if err != nil {
		t.Fatal(err)
	}
	dataKey := NewDataKey()
	wrap, _ := SealTo(dataKey, k.Pub)
	sign, err := m.signer()
	if err != nil {
		t.Fatal(err)
	}
	hdr := EnvHeader{Kind: kind, Epoch: m.Epoch(), Seq: seq, CreatedAt: meta.CreatedAt, MasterId: meta.MasterId, MasterName: meta.MasterName,
		Version: meta.Version, MaxChangeId: meta.MaxChangeId, DBSize: meta.DBSize, Wrap: wrap, To: Fingerprint(k.PubString())}
	f, _ := os.Open(zp)
	defer f.Close()
	var buf bytes.Buffer
	if err := WriteEnvelope(&buf, hdr, dataKey, f, st.Size(), sign); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 历史数据只有流量时序、订阅日志、审计日志三张表(审计日志全带,不像副本只带 7 天);副本里照旧不带前两张。
func TestHistoryHasOnlyItsTables(t *testing.T) {
	m := testService(t, false, map[string]string{KeyEpoch: "1", KeyMode: ModeConfirm})
	seedMaster(t, m)
	dir := t.TempDir()
	_, meta, err := m.buildHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Counts["stats"] != 1 || meta.Counts["sub_logs"] != 1 || meta.Counts["changes"] < 2 || meta.Counts["users"] != 0 || meta.Counts["settings"] != 0 {
		t.Fatalf("只带三张表,审计日志连 30 天前的一起: %+v", meta.Counts)
	}
	db, err := openScratch(filepath.Join(dir, "history.db"))
	if err == nil {
		var n int64
		db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('users', 'settings', 'nodes', 'admins')").Scan(&n)
		closeDB(db)
		if n != 0 {
			t.Fatal("别的表连结构都不建")
		}
	}
	_, rmeta, err := m.buildReplica(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rmeta.Counts["stats"] != 0 || rmeta.Counts["sub_logs"] != 0 {
		t.Fatalf("副本照旧不带流量时序、订阅日志: %+v", rmeta.Counts)
	}
	// 内容没变指纹不变,变了就变
	_, meta2, _ := m.buildHistory(t.TempDir())
	m.d.DB.Create(&model.Stats{DateTime: time.Now().Unix() - 60, Resource: "line", Tag: "VL", Traffic: 9})
	_, meta3, _ := m.buildHistory(t.TempDir())
	if meta2.Sum != meta.Sum || meta3.Sum == meta.Sum {
		t.Fatal("内容指纹:没变的一样、变了的不一样")
	}
}

// 备用收历史数据:先有这一任的副本;验签、核对纪元与主机、收件人、序号只增;不是历史数据的拒;
// 报告里带上手里那份;不再是备用时连同副本一起删掉。
func TestHistoryReceiveRules(t *testing.T) {
	m, c := standbyPair(t)
	if _, err := c.ReceiveHistory(bytes.NewReader(historyFor(t, m, c, 5, KindHistory))); err == nil || !strings.Contains(err.Error(), "先收副本") {
		t.Fatalf("还没有这一任的副本:不收: %v", err)
	}
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 10))); err != nil {
		t.Fatal(err)
	}
	if seq, err := c.ReceiveHistory(bytes.NewReader(historyFor(t, m, c, 20, KindHistory))); err != nil || seq != 20 {
		t.Fatalf("收下: %d %v", seq, err)
	}
	if _, err := c.ReceiveHistory(bytes.NewReader(historyFor(t, m, c, 20, KindHistory))); err == nil {
		t.Fatal("序号不比手里的新:拒")
	}
	if _, err := c.ReceiveHistory(bytes.NewReader(historyFor(t, m, c, 30, KindReplica))); err == nil || !strings.Contains(err.Error(), "不是历史数据") {
		t.Fatalf("副本当历史数据发来:拒: %v", err)
	}
	other := testService(t, false, map[string]string{KeyEpoch: "1", KeyMode: ModeConfirm})
	seedMaster(t, other)
	if _, err := c.ReceiveHistory(bytes.NewReader(historyFor(t, other, c, 40, KindHistory))); err == nil {
		t.Fatal("别人签的(不是钉住的主机公钥):拒")
	}
	m.set(map[string]string{KeyEpoch: "2"})
	if _, err := c.ReceiveHistory(bytes.NewReader(historyFor(t, m, c, 50, KindHistory))); err == nil || !strings.Contains(err.Error(), "纪元") {
		t.Fatalf("纪元对不上:拒: %v", err)
	}
	m.set(map[string]string{KeyEpoch: "1"})
	hi, ok := c.historyInfo()
	if !ok || hi.Seq != 20 || hi.MasterPub == "" || hi.DBSize <= 0 {
		t.Fatalf("手里还是序号 20 那份: %+v %v", hi, ok)
	}
	rep := c.Report()
	if rep.History == nil || rep.History.Seq != 20 || rep.HistoryBytes <= 0 {
		t.Fatalf("报告里带上: %+v", rep.History)
	}
	if err := c.DropReplica(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.historyInfo(); ok {
		t.Fatal("不再是备用:历史数据和副本一起删掉")
	}
}

// 收新副本时磁盘紧:先删可有可无的历史数据腾地方,副本照收;腾了也不够才拒。
func TestReplicaEvictsHistoryWhenDiskTight(t *testing.T) {
	m, c := standbyPair(t)
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 10))); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReceiveHistory(bytes.NewReader(historyFor(t, m, c, 11, KindHistory))); err != nil {
		t.Fatal(err)
	}
	// 和 ReceiveReplica 同样算:接任要多少(这时还没有上一份副本)
	needOf := func(r []byte) int64 {
		env, err := ReceiveEnvelope(bytes.NewReader(r), io.Discard, MaxReplica)
		if err != nil {
			t.Fatal(err)
		}
		ri, _ := c.replicaInfo()
		db := env.Header.Size
		if ri.DBSize > 0 && ri.Size > 0 {
			db = max(db, scaleSize(ri.DBSize, env.Header.Size, ri.Size))
		}
		return TakeoverNeed(env.Header.Size, db)
	}
	old := standbyDiskFree
	defer func() { standbyDiskFree = old }()
	hb := c.historyBytes()
	// 腾了也不够:拒,历史数据也不白删
	r2 := replicaFor(t, m, c, 12)
	standbyDiskFree = func(*Service) int64 { return needOf(r2) - hb - 1 }
	if _, err := c.ReceiveReplica(bytes.NewReader(r2)); err == nil {
		t.Fatal("腾了也不够:拒")
	}
	if _, ok := c.historyInfo(); !ok {
		t.Fatal("腾了也不够:历史数据不删")
	}
	// 差的那点历史数据腾得出来:删掉它,副本照收
	r3 := replicaFor(t, m, c, 13)
	standbyDiskFree = func(*Service) int64 { return needOf(r3) - 1 }
	if _, err := c.ReceiveReplica(bytes.NewReader(r3)); err != nil {
		t.Fatalf("删掉历史数据腾地方,副本照收: %v", err)
	}
	if _, ok := c.historyInfo(); ok {
		t.Fatal("历史数据让位了")
	}
	if ri, _ := c.replicaInfo(); ri.Seq != 13 {
		t.Fatalf("新副本收下: %d", ri.Seq)
	}
}

// 接任:历史数据并进新库 —— 流量图、订阅日志、30 天前的审计日志都在;副本之后删掉的用户不留孤儿行,
// 删了又建的同名用户不接上前一个人的历史;成为主机的提示写上截至何时、没有"没带过来"的告警。
func TestHistoryCarriedOnTakeover(t *testing.T) {
	p := newP2(t, 1, ModeConfirm)
	m, c := p.m, p.cands[0]
	now := time.Now().Unix()
	db := m.d.DB
	db.Create(&model.User{Name: "h-alice", Enabled: true, CreatedAt: now - 10*86400})
	db.Create(&model.User{Name: "h-bob", Enabled: true, CreatedAt: now - 10*86400})
	db.Create(&model.User{Name: "h-carol", Enabled: true, CreatedAt: now - 10*86400})
	hour := now - now%3600
	db.Create(&model.Stats{DateTime: hour - 3*86400, Resource: "user", Tag: "h-alice", Direction: true, Traffic: 111})
	db.Create(&model.Stats{DateTime: hour - 3600, Resource: "user", Tag: "h-bob", Traffic: 222})
	db.Create(&model.Stats{DateTime: hour - 7200, Resource: "user", Tag: "h-carol", Traffic: 444})
	db.Create(&model.Stats{DateTime: hour - 7200, Resource: "line", Tag: "VL", Traffic: 333})
	for i := 0; i < 3; i++ {
		db.Create(&model.SubLog{Ts: now - int64(100+i), User: "h-carol", Ip: "198.51.100.7", Ua: "hist-ua", Status: 200})
	}
	old := model.Change{DateTime: now - 30*86400, Actor: "admin", Key: "hist", Action: "old"}
	db.Create(&old)
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	if err := m.HistoryNow(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.historyInfo(); !ok {
		t.Fatal("备用收下了历史数据")
	}
	// 历史数据之后:删掉 bob;alice 删了又建(新号)。副本是这之后的
	db.Where("name = ?", "h-bob").Delete(&model.User{})
	db.Where("name = ?", "h-alice").Delete(&model.User{})
	db.Create(&model.User{Name: "h-alice", Enabled: true, CreatedAt: now})
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	if err := c.Takeover(TakeoverOpts{Force: true, By: "cli"}); err != nil {
		t.Fatal(err)
	}
	ndb := swapIn(t, c)
	sum := func(tag string) (n int64) {
		ndb.Raw("SELECT COALESCE(SUM(traffic), 0) FROM stats WHERE tag = ?", tag).Scan(&n)
		return
	}
	if sum("VL") != 333 || sum("h-carol") != 444 {
		t.Fatalf("线路、还在的用户的流量时序带过来: VL=%d carol=%d", sum("VL"), sum("h-carol"))
	}
	if sum("h-bob") != 0 || sum("h-alice") != 0 {
		t.Fatalf("删掉的用户不留孤儿行、新建的同名用户不接前一个人的: bob=%d alice=%d", sum("h-bob"), sum("h-alice"))
	}
	var logs int64
	ndb.Raw("SELECT COUNT(*) FROM sub_logs WHERE ua = 'hist-ua'").Scan(&logs)
	if logs != 3 {
		t.Fatalf("订阅日志带过来: %d", logs)
	}
	var oldN int64
	ndb.Raw("SELECT COUNT(*) FROM changes WHERE id = ? AND action = 'old'", old.Id).Scan(&oldN)
	if oldN != 1 {
		t.Fatal("30 天前的审计日志带过来(副本只带 7 天)")
	}
	var dup int64
	ndb.Raw("SELECT COUNT(*) - COUNT(DISTINCT id) FROM changes").Scan(&dup)
	if dup != 0 {
		t.Fatal("审计日志不重复")
	}
	var raw string
	ndb.Raw("SELECT value FROM settings WHERE key = ?", KeyBecame).Scan(&raw)
	var b Became
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	if b.History == nil || b.History.At == 0 || b.History.SubLogs < 3 || b.History.Changes == 0 {
		t.Fatalf("记下并进来的历史数据: %+v", b.History)
	}
	for _, w := range b.Identity.Warnings {
		if strings.HasPrefix(w.Code, "history") {
			t.Fatalf("带过来了就不告警: %+v", w)
		}
	}
	if txt := c.becameText(b, "198.51.100.2"); !strings.Contains(txt, "流量图、订阅日志") {
		t.Fatalf("通知里说一声带过来了:\n%s", txt)
	}
}

// 没有历史数据(刚当上备用)、不是这一任的:照常接任,成为主机的告警里说明流量图、订阅日志没带过来。
func TestHistoryMissingStillTakesOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		fake func(t *testing.T, c *Service)
		code string
	}{
		{"没收到过", nil, idwHistNone},
		{"不是这一任的", func(t *testing.T, c *Service) {
			hi, _ := c.historyInfo()
			hi.Epoch += 7
			b, _ := json.Marshal(hi)
			if err := os.WriteFile(filepath.Join(c.Dir(), fileHistInfo), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}, idwHistOther},
		{"坏了", func(t *testing.T, c *Service) {
			p := filepath.Join(c.Dir(), fileHist)
			b, _ := os.ReadFile(p)
			b[len(b)/2] ^= 0xff
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}, idwHistFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newP2(t, 1, ModeConfirm)
			m, c := p.m, p.cands[0]
			if tc.fake != nil {
				if err := m.HistoryNow(); err != nil {
					t.Fatal(err)
				}
				tc.fake(t, c)
			}
			if err := c.Takeover(TakeoverOpts{Force: true, By: "cli"}); err != nil {
				t.Fatalf("照常接任: %v", err)
			}
			ndb := swapIn(t, c)
			var raw string
			ndb.Raw("SELECT value FROM settings WHERE key = ?", KeyBecame).Scan(&raw)
			var b Became
			_ = json.Unmarshal([]byte(raw), &b)
			found := false
			for _, w := range b.Identity.Warnings {
				if w.Code == tc.code {
					found = true
				}
			}
			if !found || b.History != nil {
				t.Fatalf("告警说明没带过来(%s): %+v %+v", tc.code, b.Identity.Warnings, b.History)
			}
			if txt := identityWarnText(item(tc.code, "t", "x", "err", "x", "need", "1", "free", "0")); !strings.Contains(txt, "已用流量") {
				t.Fatalf("告警写明用户的已用流量不受影响: %s", txt)
			}
		})
	}
}

// 节奏:先收下这一任的副本才给;默认每小时一份,库大放宽(最多 6 小时);发失败隔 30 分钟再试;
// 主机重启后不记得发过,按它报的手里那份算。
func TestHistoryCadence(t *testing.T) {
	m := testService(t, false, map[string]string{KeyEpoch: "3", KeyMode: ModeConfirm})
	r := m.rep
	now := time.Now()
	infos := []StandbyInfo{{Id: 7}}
	due := func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.historyDueLocked(infos, []uint{7}, 3, now)) == 1
	}
	if due() {
		t.Fatal("还没收下这一任的副本:不给")
	}
	r.acks[7] = &ack{epoch: 3, seq: 1}
	if !due() {
		t.Fatal("收下副本、没给过:给")
	}
	r.noteHist(7, 3, 9, HistoryMeta{Sum: "a", CreatedAt: now.Unix()}, 100, nil)
	r.hist[7].sentAt = now.Add(-59 * time.Minute).Unix()
	if due() {
		t.Fatal("不到一小时:不给")
	}
	r.hist[7].sentAt = now.Add(-61 * time.Minute).Unix()
	if !due() {
		t.Fatal("过了一小时:给")
	}
	r.histSize = 100 << 20 // 100 MB:4 小时一份
	if due() {
		t.Fatal("库大:放宽")
	}
	r.histSize = 10 << 30
	if r.historyEveryLocked() != historyEveryMax {
		t.Fatal("最多 6 小时")
	}
	r.histSize = 0
	r.noteHist(7, 3, 0, HistoryMeta{}, 0, os.ErrDeadlineExceeded)
	r.hist[7].errAt = now.Add(-10 * time.Minute).Unix()
	if due() {
		t.Fatal("刚发失败:隔 30 分钟再试")
	}
	r.hist[7].errAt = now.Add(-31 * time.Minute).Unix()
	if !due() {
		t.Fatal("失败过了 30 分钟:再试")
	}
	// 主机重启:不记得发过,它报手里有一份 10 分钟前的
	delete(r.hist, 7)
	infos[0].History = &hub.HAHistory{Epoch: 3, Seq: 5, At: now.Add(-10 * time.Minute).Unix()}
	if due() {
		t.Fatal("它手里的还新:不给")
	}
	infos[0].History.Epoch = 2
	if !due() {
		t.Fatal("它手里的是上一任的:给")
	}
}

// 内容没变、它手里就是上一份:不重发;流量一变就发新的。
func TestHistoryUnchangedNotResent(t *testing.T) {
	p := newP2(t, 1, ModeConfirm)
	m, c := p.m, p.cands[0]
	if err := m.HistoryNow(); err != nil {
		t.Fatal(err)
	}
	h1, _ := c.historyInfo()
	p.h.RunOnce() // 报告刷新:主机看到它手里的序号
	if err := m.HistoryNow(); err != nil {
		t.Fatal(err)
	}
	if h2, _ := c.historyInfo(); h2.Seq != h1.Seq {
		t.Fatal("内容没变:不重发")
	}
	m.d.DB.Create(&model.Stats{DateTime: time.Now().Unix() - 30, Resource: "line", Tag: "VL", Traffic: 1})
	if err := m.HistoryNow(); err != nil {
		t.Fatal(err)
	}
	if h3, _ := c.historyInfo(); h3.Seq <= h1.Seq {
		t.Fatal("内容变了:发新的")
	}
	var v []StandbyInfo
	v = m.rep.Standbys()
	if len(v) == 0 || v[0].HistAck == nil || v[0].HistAck.AsOf == 0 {
		t.Fatalf("面板上看得到历史数据截至何时: %+v", v)
	}
}
