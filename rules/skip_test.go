package rules

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 「忽略部分线路」:用量累计、状态带上忽略的线路、突发只算没被忽略的线路(用户 11-23 提的需求)。

func meter(boot string, rows ...model.LineMeterRow) model.LineMeter {
	return model.LineMeter{Boot: boot, Rows: rows}
}

func row(user string, line uint, raw, billed int64) model.LineMeterRow {
	return model.LineMeterRow{User: user, Line: line, Raw: raw, Billed: billed}
}

// 头一回见到一台只当基线;同一个 Boot 按差值加;换了 Boot 整份算新的;倍率只乘 Raw;Sum 不算忽略的线路。
func TestLineUsageFeed(t *testing.T) {
	u := &LineUsage{}
	u.Feed(1, 1, meter("a", row("alice", 7, 100, 0)))
	if u.Sum("alice", nil) != 0 {
		t.Fatal("功能没开时不收")
	}
	u.SetOn(true)
	u.Feed(1, 2, meter("a", row("alice", 7, 100, 0), row("alice", 8, 50, 0)))
	if n := u.Sum("alice", nil); n != 0 {
		t.Fatalf("头一回见到这台只当基线: %d", n)
	}
	u.Feed(1, 2, meter("a", row("alice", 7, 130, 0), row("alice", 8, 50, 40)))
	if n := u.Sum("alice", nil); n != 30*2+40 {
		t.Fatalf("同一次启动按差值加,Raw 乘服务器倍率、Billed 原样: %d", n)
	}
	if n := u.Sum("alice", map[uint]bool{7: true}); n != 40 {
		t.Fatalf("忽略 7 号线路后只剩 8 号的: %d", n)
	}
	// 这台重启:累计从 0 重来,这一份整份是新的
	u.Feed(1, 2, meter("b", row("alice", 7, 10, 0)))
	if n := u.Sum("alice", nil); n != 100+20 {
		t.Fatalf("换了 Boot 整份算新的: %d", n)
	}
	// 同一次启动里某一项变小:是那一项闲置太久被副机清掉、又从 0 记的,整份算新的
	u.Feed(1, 2, meter("b", row("alice", 7, 5, 0)))
	if n := u.Sum("alice", nil); n != 130 {
		t.Fatalf("同一次启动里变小的一项按从 0 重记算: %d", n)
	}
	// 同一次启动里第一次出现的组合:从这台启动以来的整份都是上一份之后跑的
	u.Feed(1, 2, meter("b", row("alice", 7, 5, 0), row("bob", 7, 9, 0)))
	if n := u.Sum("bob", nil); n != 18 || u.Sum("alice", nil) != 130 {
		t.Fatalf("新出现的组合整份算、没变的不加: bob %d alice %d", n, u.Sum("alice", nil))
	}
	// 另一台:各记各的基线
	u.Feed(2, 1, meter("x", row("alice", 7, 1000, 0)))
	if n := u.Sum("alice", nil); n != 130 {
		t.Fatalf("另一台头一回见只当基线: %d", n)
	}
	// 关掉清空;再打开又从基线开始
	u.SetOn(false)
	u.SetOn(true)
	u.Feed(1, 2, meter("b", row("alice", 7, 500, 0)))
	if n := u.Sum("alice", nil); n != 0 {
		t.Fatalf("关掉再打开后清空、重新当基线: %d", n)
	}
}

// 太久没涨的累计清掉(突发窗口最长 24 小时):25 小时没动的那一项没了,刚涨过的留着。
func TestLineUsagePrune(t *testing.T) {
	clock := int64(1_000_000)
	u := &LineUsage{now: func() int64 { return clock }}
	u.SetOn(true)
	u.Feed(1, 1, meter("a"))
	u.Feed(1, 1, meter("a", row("alice", 7, 100, 0), row("bob", 7, 50, 0)))
	clock += 20 * 3600
	u.Feed(1, 1, meter("a", row("alice", 7, 100, 0), row("bob", 7, 60, 0)))
	clock += 6 * 3600
	u.Prune()
	if a, b := u.Sum("alice", nil), u.Sum("bob", nil); a != 0 || b != 60 {
		t.Fatalf("alice 26 小时没涨应清掉,bob 6 小时前还涨过留着: alice %d bob %d", a, b)
	}
}

func lineIDs(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	return string(raw)
}

// 时段规则:状态抄上规则忽略的线路(规整成从小到大、去掉已经删了的线路);改规则的忽略线路,状态跟着改;
// 一条都不剩(线路全删了)就当没勾。
func TestScheduleStateCarriesSkipLines(t *testing.T) {
	e, db := newEngine(t)
	db.Create(&model.User{Name: "alice", Enabled: true})
	db.Create(&model.Line{Name: "便宜", Protocol: "anytls", Port: 1001})
	db.Create(&model.Line{Name: "正常", Protocol: "anytls", Port: 1002})
	db.Create(&model.Line{Name: "又一条", Protocol: "anytls", Port: 1003})
	db.Create(&model.Rule{Name: "全天", Enabled: true, Kind: KindSchedule, AllUsers: true, Start: "00:00", End: "00:00",
		DownMbps: 5, TightenOnly: true, SkipLineIds: []byte(`[3,1,1,99]`)})
	t0 := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	if _, err := e.Tick(t0); err != nil {
		t.Fatal(err)
	}
	st := statesOf(t, db)
	if len(st) != 1 || lineIDs(t, st[0].SkipLineIds) != `[1,3]` {
		t.Fatalf("状态应带上规整后的忽略线路 [1,3]: %+v", st)
	}
	db.Model(&model.Rule{}).Where("id = 1").Update("skip_line_ids", []byte(`[2]`))
	if changed, err := e.Tick(t0.Add(10 * time.Second)); err != nil || !changed {
		t.Fatalf("改了忽略的线路,状态要跟着改: %v %v", changed, err)
	}
	if st = statesOf(t, db); lineIDs(t, st[0].SkipLineIds) != `[2]` {
		t.Fatalf("状态的忽略线路没跟上: %s", st[0].SkipLineIds)
	}
	db.Delete(&model.Line{}, 2)
	if _, err := e.Tick(t0.Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if st = statesOf(t, db); len(st[0].SkipLineIds) != 0 {
		t.Fatalf("忽略的线路全删了,状态就不再带: %s", st[0].SkipLineIds)
	}
}

// 突发规则勾了忽略线路:用量只算没被忽略的线路(用户表里的总用量不看);在忽略的线路上跑多少都不触发,
// 在别的线路上跑到阈值才触发,状态带上忽略的线路;改了忽略的线路窗口从头算(把跑得多的那条取消忽略不会立刻误触发)。
func TestBurstSkipLinesCountsOnlyOtherLines(t *testing.T) {
	e, db := newEngine(t)
	u := &LineUsage{}
	e.Usage = u
	var local model.LineMeter
	e.LocalMeter = func() (uint, float64, model.LineMeter, bool) { return 1, 1, local, true }
	db.Create(&model.User{Name: "alice", Enabled: true})
	db.Create(&model.Line{Name: "便宜", Protocol: "anytls", Port: 1001})
	db.Create(&model.Line{Name: "正常", Protocol: "anytls", Port: 1002})
	db.Create(&model.Rule{Name: "突发", Enabled: true, Kind: KindBurst, AllUsers: true,
		WindowMin: 10, ThresholdBytes: gb, PenaltyMin: 20, DownMbps: 5, TightenOnly: true, SkipLineIds: []byte(`[1]`)})
	t0 := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	tick := func(sec int64) bool {
		changed, err := e.Tick(t0.Add(time.Duration(sec) * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	local = meter("boot1")
	tick(0) // 打开功能、主机自己这台当基线
	// 在忽略的线路上跑了 5 GB,用户表里的总用量也涨了 5 GB:不该触发
	local = meter("boot1", row("alice", 1, 5*gb, 0))
	setUsage(db, 1, 5*gb)
	if tick(10) {
		t.Fatal("忽略的线路上的流量不该算进用量")
	}
	// 再在正常线路上跑 1.2 GB:触发,状态带上忽略的线路
	local = meter("boot1", row("alice", 1, 5*gb, 0), row("alice", 2, gb+gb/5, 0))
	setUsage(db, 1, 6*gb+gb/5)
	if !tick(20) {
		t.Fatal("没被忽略的线路上 10 分钟 1.2 GB 应触发")
	}
	st := statesOf(t, db)
	if len(st) != 1 || string(st[0].SkipLineIds) != `[1]` || st[0].Until == 0 {
		t.Fatalf("状态不对: %+v", st)
	}
	// 解除后把忽略的线路改成另一条(便宜那条不再忽略):口径换了,窗口从头算,前面便宜线路上那 5 GB 不能一下子算进来
	db.Where("1 = 1").Delete(&model.LimitState{})
	db.Model(&model.Rule{}).Where("id = 1").Update("skip_line_ids", []byte(`[2]`))
	if tick(30) {
		t.Fatal("改了忽略的线路后窗口从头算,不该拿旧样本误触发")
	}
	local = meter("boot1", row("alice", 1, 5*gb+gb/10, 0), row("alice", 2, 3*gb, 0))
	if tick(40) {
		t.Fatal("新口径下只跑了 0.1 GB,不该触发")
	}
}

// 没勾忽略线路的突发规则照旧按用户表里的总用量算(和以前一样),也不打开「用户 × 线路」的计数。
func TestBurstWithoutSkipUnchanged(t *testing.T) {
	e, db := newEngine(t)
	u := &LineUsage{}
	e.Usage = u
	fed := 0
	e.LocalMeter = func() (uint, float64, model.LineMeter, bool) { fed++; return 1, 1, model.LineMeter{}, true }
	db.Create(&model.User{Name: "alice", Enabled: true})
	db.Create(&model.Rule{Name: "突发", Enabled: true, Kind: KindBurst, AllUsers: true,
		WindowMin: 10, ThresholdBytes: gb, PenaltyMin: 20, DownMbps: 5, TightenOnly: true})
	t0 := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	e.Tick(t0)
	setUsage(db, 1, gb+gb/5)
	if changed, _ := e.Tick(t0.Add(10 * time.Second)); !changed {
		t.Fatal("没勾忽略线路:按总用量 1.2 GB 触发")
	}
	if fed != 0 {
		t.Fatal("没有规则用得着时不取「用户 × 线路」的计数")
	}
	if st := statesOf(t, db); len(st[0].SkipLineIds) != 0 {
		t.Fatalf("没勾忽略线路的状态不带: %s", st[0].SkipLineIds)
	}
}

// 「要不要记用户 × 线路」:只看启用的突发规则勾的、还在的线路;时段规则、停用的规则、线路已删的不算。
func TestLineMeterWanted(t *testing.T) {
	_, db := newEngine(t)
	want := func() bool {
		on, err := LineMeterWanted(db)
		if err != nil {
			t.Fatal(err)
		}
		return on
	}
	db.Create(&model.Line{Name: "便宜", Protocol: "anytls", Port: 1001})
	db.Create(&model.Rule{Name: "时段", Enabled: true, Kind: KindSchedule, AllUsers: true, Start: "00:00", End: "00:00", DownMbps: 5, SkipLineIds: []byte(`[1]`)})
	if want() {
		t.Fatal("时段规则不用记")
	}
	db.Create(&model.Rule{Name: "突发", Enabled: true, Kind: KindBurst, AllUsers: true, WindowMin: 10, ThresholdBytes: gb, PenaltyMin: 20, DownMbps: 5, SkipLineIds: []byte(`[9]`)})
	if want() {
		t.Fatal("勾的线路已经不在了,不用记")
	}
	db.Model(&model.Rule{}).Where("name = ?", "突发").Update("skip_line_ids", []byte(`[1]`))
	if !want() {
		t.Fatal("启用的突发规则勾了还在的线路:要记")
	}
	db.Model(&model.Rule{}).Where("name = ?", "突发").Update("enabled", false)
	if want() {
		t.Fatal("规则停用了不用记")
	}
}

// 某条线路上的限速只算管得着它的状态。
func TestEffectiveOnLine(t *testing.T) {
	states := []model.LimitState{
		{DownMbps: 5, TightenOnly: true, SkipLineIds: []byte(`[1]`)},
		{DownMbps: 20, TightenOnly: true},
	}
	if _, d := Effective(100, 100, states); d != 5 {
		t.Fatalf("别的线路取最严: %d", d)
	}
	if _, d := EffectiveOnLine(100, 100, states, 1); d != 20 {
		t.Fatalf("1 号线路上忽略第一条,只剩 20: %d", d)
	}
	if _, d := EffectiveOnLine(100, 100, states[:1], 1); d != 100 {
		t.Fatalf("1 号线路上没有管得着的状态,回到用户自己的: %d", d)
	}
}
