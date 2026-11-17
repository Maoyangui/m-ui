package model_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

func quotaDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	return db
}

func quotaOf(t *testing.T, db *gorm.DB, id uint) model.NodeQuota {
	t.Helper()
	var q model.NodeQuota
	if err := db.Where("node_id = ?", id).Limit(1).Find(&q).Error; err != nil {
		t.Fatal(err)
	}
	return q
}

// 本机数据面的累计:打开库时就建好一条全零的记录(带这本账的代号),之后只增不减、代号不变。
// 记录要在第一笔流量之前就有:主机头一次见到一本账只当基线、不计,等有了流量才建的话第一笔就丢了(实机上发现的)。
func TestLocalTotalAccumulates(t *testing.T) {
	db := quotaDB(t)
	first, err := model.ReadLocalTotal(db)
	if err != nil || first.Up != 0 || first.Down != 0 || first.LedgerId == 0 {
		t.Fatalf("打开库之后应当已经有一条全零、带代号的记录: %+v %v", first, err)
	}
	got, err := model.AddLocalTotal(db, 0, 0, 10)
	if err != nil || got != first {
		t.Fatalf("没有流量的一轮不改它: %+v %v", got, err)
	}
	if err := model.EnsureLocalTotal(db, 99); err != nil { // 再调一次(重启):不换代号、不清零
		t.Fatal(err)
	}
	a, err := model.AddLocalTotal(db, 100, 900, 11)
	if err != nil || a.Up != 100 || a.Down != 900 || a.LedgerId != first.LedgerId {
		t.Fatalf("第一笔(代号应当还是 %d): %+v %v", first.LedgerId, a, err)
	}
	if err := model.EnsureLocalTotal(db, 100); err != nil {
		t.Fatal(err)
	}
	if again, _ := model.ReadLocalTotal(db); again != a {
		t.Fatalf("有了数之后再调 EnsureLocalTotal 不该动它: %+v,应为 %+v", again, a)
	}
	// 库里那一行没了(比如从没有这张表的旧备份还原之后):照样能记,另起一本
	db.Exec("DELETE FROM local_totals")
	if z, _ := model.ReadLocalTotal(db); z != (model.NodeTotal{}) {
		t.Fatalf("没有记录时读出来是零值: %+v", z)
	}
	if n, err := model.AddLocalTotal(db, 7, 8, 12); err != nil || n.Up != 7 || n.Down != 8 || n.LedgerId == 0 || n.LedgerId == first.LedgerId {
		t.Fatalf("没有记录时第一笔自己建一条、另起一本: %+v %v", n, err)
	}
	db.Exec("DELETE FROM local_totals")
	db.Create(&model.LocalTotal{Id: 1, Up: a.Up, Down: a.Down, LedgerId: a.LedgerId})
	b, _ := model.AddLocalTotal(db, 5, -7, 12) // 负数当 0
	if b.Up != 105 || b.Down != 900 || b.LedgerId != a.LedgerId {
		t.Fatalf("累加后: %+v(代号应不变 %d)", b, a.LedgerId)
	}
	c, _ := model.ReadLocalTotal(db)
	if c != b {
		t.Fatalf("读回 %+v,应为 %+v", c, b)
	}
	var rows int64
	db.Model(&model.LocalTotal{}).Count(&rows)
	if rows != 1 {
		t.Fatalf("只该有一行,实际 %d", rows)
	}
}

// 主机把一台机器的累计并进它的本期用量:只计增量;第一次见到、换了一本、变小了都只对齐不计。
func TestMergeNodeTotal(t *testing.T) {
	db := quotaDB(t)
	db.Create(&model.Node{Name: "tw", Enabled: true})
	var n model.Node
	db.Where("name = ?", "tw").First(&n)
	merge := func(up, down, ledger int64, now int64) int64 {
		t.Helper()
		added, err := model.MergeNodeTotal(db, n.Id, model.NodeTotal{Up: up, Down: down, LedgerId: ledger}, now)
		if err != nil {
			t.Fatal(err)
		}
		return added
	}
	// 一台跑了很久的机器第一次报累计:只建基线。把 500 GB 当成本期用量会当场把它判成用完
	if got := merge(300<<30, 200<<30, 7, 100); got != 0 {
		t.Fatalf("第一次见到只建基线,却计了 %d", got)
	}
	q := quotaOf(t, db, n.Id)
	if q.Used() != 0 || !q.Tracked || q.PeriodStart != 100 || q.CurLedger != 7 {
		t.Fatalf("基线: %+v", q)
	}
	// 之后按增量
	if got := merge(300<<30+1000, 200<<30+4000, 7, 110); got != 5000 {
		t.Fatalf("增量应为 5000,实际 %d", got)
	}
	if q = quotaOf(t, db, n.Id); q.Up != 1000 || q.Down != 4000 {
		t.Fatalf("本期用量: 上 %d 下 %d", q.Up, q.Down)
	}
	// 一模一样的再来一份(每 5 秒一份报告):不计,也不写库
	before := quotaOf(t, db, n.Id)
	if got := merge(300<<30+1000, 200<<30+4000, 7, 999); got != 0 {
		t.Fatalf("重复的报告计了 %d", got)
	}
	if after := quotaOf(t, db, n.Id); after.CurAt != before.CurAt {
		t.Fatal("没变化的报告不该写库")
	}
	// 同一本账却变小了(那台的库回到了以前):对齐,不计;之后从新的位置接着算
	if got := merge(1000, 2000, 7, 120); got != 0 {
		t.Fatalf("账本回退不该计,实际 %d", got)
	}
	if got := merge(1500, 2600, 7, 130); got != 1100 {
		t.Fatalf("回退之后的增量应为 1100,实际 %d", got)
	}
	// 换了一本(重装、库被换掉):只建基线,不把新的一本整本当增量
	if got := merge(900000, 900000, 8, 140); got != 0 {
		t.Fatalf("换了一本只建基线,却计了 %d", got)
	}
	if got := merge(900100, 900000, 8, 150); got != 100 {
		t.Fatalf("新的一本上的增量应为 100,实际 %d", got)
	}
	if q = quotaOf(t, db, n.Id); q.Up != 1000+500+100 || q.Down != 4000+600 {
		t.Fatalf("合计: 上 %d 下 %d", q.Up, q.Down)
	}
	// 老版本不报(代号 0)、负数、已经删掉的服务器:什么都不做,也不留下记录
	for _, tc := range []struct {
		id uint
		t  model.NodeTotal
	}{{n.Id, model.NodeTotal{Up: 1 << 40, Down: 1}}, {n.Id, model.NodeTotal{Up: -1, Down: 5, LedgerId: 8}}, {9999, model.NodeTotal{Up: 5, Down: 5, LedgerId: 3}}, {0, model.NodeTotal{Up: 5, Down: 5, LedgerId: 3}}} {
		if added, err := model.MergeNodeTotal(db, tc.id, tc.t, 200); err != nil || added != 0 {
			t.Fatalf("%+v 不该计: %d %v", tc, added, err)
		}
	}
	var rows int64
	db.Model(&model.NodeQuota{}).Count(&rows)
	if rows != 1 {
		t.Fatalf("不该给不存在的服务器留记录:共 %d 行", rows)
	}
	if q2 := quotaOf(t, db, n.Id); q2.Up != q.Up || q2.Down != q.Down {
		t.Fatal("无效的报告动了用量")
	}
}

// 下一次重置的时间:按天数从现在数;按月是下一个「每月 D 日 0 点」,那个月没有这一天就用月底,当天 0 点整算已经过了。
func TestNextQuotaReset(t *testing.T) {
	sh := time.FixedZone("CST", 8*3600)
	at := func(y int, m time.Month, d, h int) int64 { return time.Date(y, m, d, h, 0, 0, 0, sh).Unix() }
	now := at(2026, 11, 18, 15)
	if got := model.NextQuotaReset(model.QuotaResetDays, 30, 0, now, sh); got != now+30*86400 {
		t.Fatalf("按 30 天: %d", got)
	}
	for _, tc := range []struct {
		name string
		now  int64
		day  int
		want int64
	}{
		{"这个月还没到", at(2026, 11, 18, 15), 25, at(2026, 11, 25, 0)},
		{"这个月已经过了", at(2026, 11, 18, 15), 5, at(2026, 12, 5, 0)},
		{"就是今天,0 点已过", at(2026, 11, 18, 0), 18, at(2026, 12, 18, 0)},
		{"跨年", at(2026, 12, 20, 9), 1, at(2027, 1, 1, 0)},
		{"31 日遇上 11 月", at(2026, 11, 18, 15), 31, at(2026, 11, 30, 0)},
		{"31 日遇上平年 2 月", at(2027, 2, 1, 9), 31, at(2027, 2, 28, 0)},
		{"29 日遇上闰年 2 月", at(2028, 2, 1, 9), 29, at(2028, 2, 29, 0)},
		{"月底那天已过 → 下个月的月底 / 那一天", at(2027, 2, 28, 1), 31, at(2027, 3, 31, 0)},
	} {
		if got := model.NextQuotaReset(model.QuotaResetMonth, 0, tc.day, tc.now, sh); got != tc.want {
			t.Errorf("%s: 得到 %s,应为 %s", tc.name, time.Unix(got, 0).In(sh), time.Unix(tc.want, 0).In(sh))
		}
	}
	// 时区:同一时刻在不同的时区里「这个月的 1 日」不是同一刻
	utc := model.NextQuotaReset(model.QuotaResetMonth, 0, 1, at(2026, 11, 30, 23), time.UTC)
	if want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Unix(); utc != want {
		t.Fatalf("UTC 下应为 12-01 00:00 UTC,得到 %s", time.Unix(utc, 0).UTC())
	}
	for _, bad := range []int64{
		model.NextQuotaReset(model.QuotaResetNone, 30, 1, now, sh), model.NextQuotaReset(model.QuotaResetDays, 0, 0, now, sh),
		model.NextQuotaReset(model.QuotaResetMonth, 0, 0, now, sh), model.NextQuotaReset(model.QuotaResetMonth, 0, 32, now, sh), model.NextQuotaReset("weekly", 7, 1, now, sh),
	} {
		if bad != 0 {
			t.Fatalf("不重置 / 参数不对应返回 0,得到 %d", bad)
		}
	}
}

// 状态:到期优先;流量到了额度才算用完;没设就是正常。到点重置:本期并进合计、清零、排下一次。
func TestQuotaStateAndRoll(t *testing.T) {
	q := model.NodeQuota{Volume: 1000, Up: 400, Down: 599}
	if q.StateNow(50) != model.QuotaOK {
		t.Fatal("差 1 字节不算用完")
	}
	q.Down = 600
	if q.StateNow(50) != model.QuotaTraffic {
		t.Fatal("到了额度算用完")
	}
	q.Expiry = 60
	if q.StateNow(59) != model.QuotaTraffic || q.StateNow(60) != model.QuotaExpired {
		t.Fatal("到期优先,到点那一刻就算到期")
	}
	if (model.NodeQuota{Up: 1 << 50}).StateNow(1) != model.QuotaOK || (model.NodeQuota{}).Configured() {
		t.Fatal("没设额度、没设到期就是正常")
	}

	db := quotaDB(t)
	db.Create(&model.NodeQuota{NodeId: 3, Volume: 1000, Up: 700, Down: 800, TotalUp: 10, TotalDown: 20, ResetMode: model.QuotaResetDays, ResetDays: 7, NextReset: 500, PeriodStart: 1})
	q = quotaOf(t, db, 3)
	if model.RollQuota(q, 499, time.UTC) != nil {
		t.Fatal("没到时间不重置")
	}
	roll := model.RollQuota(q, 500, time.UTC)
	if roll == nil {
		t.Fatal("到点应重置")
	}
	if err := db.Model(&model.NodeQuota{}).Where("node_id = ?", 3).Updates(roll).Error; err != nil {
		t.Fatal(err)
	}
	if q = quotaOf(t, db, 3); q.Used() != 0 || q.TotalUp != 710 || q.TotalDown != 820 || q.PeriodStart != 500 || q.NextReset != 500+7*86400 {
		t.Fatalf("重置后: %+v", q)
	}
	if model.RollQuota(model.NodeQuota{NextReset: 1}, 500, time.UTC) != nil || model.RollQuota(model.NodeQuota{ResetMode: model.QuotaResetDays, ResetDays: 7}, 500, time.UTC) != nil {
		t.Fatal("不重置的、还没排时间的不动")
	}
}

// Serving:启用着、而且没有被停止服务。
func TestNodeServing(t *testing.T) {
	for _, tc := range []struct {
		n    model.Node
		want bool
	}{{model.Node{Enabled: true}, true}, {model.Node{Enabled: false}, false}, {model.Node{Enabled: true, Suspended: true}, false}, {model.Node{Suspended: true}, false}} {
		if tc.n.Serving() != tc.want {
			t.Errorf("%+v 应为 %v", tc.n, tc.want)
		}
	}
}
