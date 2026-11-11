package ha

import (
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"

	"gorm.io/gorm"
)

// ---- 换主机前后的流量:接任 / 设为主机 / 原主机变为副机,每一段都要进新主机的库,而且只进一次 ----

// openPromoted 打开组好、还没换上的新库(.promote)。
func openPromoted(t *testing.T, s *Service) *gorm.DB {
	t.Helper()
	db, err := database.Open(promoteFile(s.d.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	return db
}

// swapIn 模拟 s 重启:关掉当前库,换上待换的新库(ApplyPending),打开换上的库。
func swapIn(t *testing.T, s *Service) *gorm.DB {
	t.Helper()
	if err := database.Close(s.d.DB); err != nil {
		t.Fatal(err)
	}
	if ok, err := ApplyPendingAt(s.d.DBPath, time.Now()); !ok || err != nil {
		t.Fatalf("应换上新库: %v %v", ok, err)
	}
	db, err := database.Open(s.d.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	return db
}

func downOf(t *testing.T, db *gorm.DB, name string) int64 {
	t.Helper()
	var u model.User
	if err := db.Where("name = ?", name).First(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u.Down
}

// useAgent 副机记账(和 jobs 的副机路径一样)。
func useAgent(t *testing.T, db *gorm.DB, name string, n int64) {
	t.Helper()
	if err := model.AddAgentCounters(db, []model.AgentCounter{{UserName: name, Up: n, Down: n}}, 100); err != nil {
		t.Fatal(err)
	}
}

// useMaster 主机记账(和 jobs 的主机路径一样):直接加到用户用量上,同一笔事务里交接途中另记一本账。
func useMaster(t *testing.T, db *gorm.DB, name string, n int64) {
	t.Helper()
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.User{}).Where("name = ?", name).
			Updates(map[string]interface{}{"up": gorm.Expr("up + ?", n), "down": gorm.Expr("down + ?", n)}).Error; err != nil {
			return err
		}
		return model.AddSelfLedger(tx, []model.AgentCounter{{UserName: name, Up: n, Down: n}}, 100)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// copyUsers 副机应用主机快照里的用户表(id、建号时间原样;假副机 miniAgent 不落库,这里补上)。
func copyUsers(t *testing.T, from, to *gorm.DB) {
	t.Helper()
	var us []model.User
	if err := from.Find(&us).Error; err != nil {
		t.Fatal(err)
	}
	if err := to.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.User{}).Error; err != nil {
		t.Fatal(err)
	}
	if len(us) > 0 {
		if err := to.Create(&us).Error; err != nil {
			t.Fatal(err)
		}
	}
}

// reporting 副机 node 已经在给主机报账:用户表同步了,纪元记在主机上(第一份空报告只建基线)。
func reporting(t *testing.T, m, node *Service, id uint, name, epoch string) {
	t.Helper()
	copyUsers(t, m.d.DB, node.d.DB)
	if err := node.set(map[string]string{hub.LedgerEpochKey: epoch}); err != nil {
		t.Fatal(err)
	}
	mergeFrom(t, m.d.DB, id, name, node.d.DB)
}

// mergeFrom 主机 db 并副机 from 现在的整本账(纪元、清空重开的声明都照副机的报告)。
func mergeFrom(t *testing.T, db *gorm.DB, nodeId uint, name string, from *gorm.DB) hub.MergeResult {
	t.Helper()
	l, err := hub.LocalLedger(from)
	if err != nil {
		t.Fatal(err)
	}
	res, err := hub.MergeReport(db, nodeId, name, l, time.Now().Unix(), 60, 1)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// 接任之后原主机回来、变为副机:它当主机时本机流量直接记进用户用量,新主机库里既没有它的纪元也没有它的游标。
// 以前它换一本新纪元的账,新主机把第一份当成新的一本只建基线:从变为副机到第一次被拉取(原主机那一行要等确认它认了
// 新纪元才重新启用,API 是 http 的要等管理员手动启用)的流量全丢(应计 700,实计 0)。
// 现在变为副机时清空账本、声明从 0 开始,新主机组库时给原主机记上"等它清空重开的那本",第一份从 0 计。
func TestFormerMasterTakeoverFirstReportCounted(t *testing.T) {
	p := newP2(t, 1, ModeConfirm)
	m, c := p.m, p.cands[0]
	mid := m.SelfId()
	if err := c.Takeover(TakeoverOpts{Force: true, By: "cli"}); err != nil {
		t.Fatal(err)
	}
	ndb := openPromoted(t, c)
	before := downOf(t, ndb, "alice")
	// 原主机库里的残行(早年当副机时的账本)、交接途中另记账的开关:变为副机时都清掉,新账本从 0 开始
	m.d.DB.Create(&model.AgentCounter{UserName: "ghost", Up: 9, Down: 9})
	if err := m.set(map[string]string{KeySelfLedger: "e-stale"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Demote(Demoted{Epoch: 2, MasterId: c.SelfId(), OldEpoch: 1, At: p.clk.now().Unix(), Reason: "takenOver"}); err != nil {
		t.Fatal(err)
	}
	if !m.IsNode() || m.setting(KeySelfLedger) != "" {
		t.Fatalf("应已变为副机、不再另记账: node=%v selfLedger=%q", m.IsNode(), m.setting(KeySelfLedger))
	}
	useAgent(t, m.d.DB, "alice", 700)
	mergeFrom(t, ndb, mid, "HK", m.d.DB)
	if got := downOf(t, ndb, "alice") - before; got != 700 {
		t.Fatalf("原主机变为副机后记的 700 应全部计费,实际 %d", got)
	}
	useAgent(t, m.d.DB, "alice", 70)
	mergeFrom(t, ndb, mid, "HK", m.d.DB)
	mergeFrom(t, ndb, mid, "HK", m.d.DB) // 重复的报告不重复计
	if got := downOf(t, ndb, "alice") - before; got != 770 {
		t.Fatalf("之后按增量,应是 770,实际 %d", got)
	}
	var server int64
	ndb.Model(&model.Stats{}).Where("resource = ? AND tag = ? AND direction = ?", "node", "HK", false).
		Select("COALESCE(SUM(traffic), 0)").Scan(&server)
	if server != 770 {
		t.Fatalf("变为副机前的残行不该报上来,服务器维度应是 770,实际 %d", server)
	}
}

// 接任时组好新库之后、重启换库之前,备用自己还在记账(写进旧库的账本):以前随旧库换下去丢了(应计 125,实计 120)。
// 换库时按新库里本机的游标把旧库账本里多出来的并进去;组库时并过的不重计。
func TestTakeoverLateOwnTrafficMerged(t *testing.T) {
	p := newP2(t, 1, ModeConfirm)
	m, c := p.m, p.cands[0]
	cid := c.SelfId()
	reporting(t, m, c, cid, "TW1", "e-standby")
	useAgent(t, c.d.DB, "alice", 100)
	mergeFrom(t, m.d.DB, cid, "TW1", c.d.DB) // 主机并过这台的 100
	if err := m.ReplicateNow(); err != nil { // 副本里带着游标
		t.Fatal(err)
	}
	useAgent(t, c.d.DB, "alice", 20) // 还没报上去:组库时并进去
	if err := c.Takeover(TakeoverOpts{Force: true, By: "cli"}); err != nil {
		t.Fatal(err)
	}
	useAgent(t, c.d.DB, "alice", 5) // 组好新库之后、重启之前
	ndb := swapIn(t, c)
	if got := downOf(t, ndb, "alice"); got != 125 {
		t.Fatalf("组库前后备用自己记的都应计费、不重计,应是 125,实际 %d", got)
	}
}

// 设为主机的交接窗口:整库数据导出之后原主机照常当主机(冻结只挡管理类写操作),本机流量记进它自己的用户用量,
// 目标暂存之后自己记的流量写进它旧库的账本,两段都进不了新主机的库(应计 3100,实计 3000);原主机变为副机后
// 第一份报告又只建基线。现在:交接途中原主机另记一本账,整库数据里带着这本账的纪元与同一时刻的计数(当游标),
// 原主机变为副机后接着这本账报,新主机按增量回收;目标换库时把旧库账本里组库之后多出来的并进去。
func TestSetMasterHandoverTrafficCounted(t *testing.T) {
	cl := readyCluster(t, 2)
	m, tgt := cl.m, cl.cands[0]
	tid, mid := cl.ids[0], m.SelfId()
	reporting(t, m, tgt, tid, "TW1", "e-target")
	useAgent(t, tgt.d.DB, "alice", 100)
	mergeFrom(t, m.d.DB, tid, "TW1", tgt.d.DB) // 主机并过目标的 100
	base := downOf(t, m.d.DB, "alice")
	useAgent(t, tgt.d.DB, "alice", 20) // 目标还没报上去的:组库时并进去
	wrapAgent(t, cl, 0, agentHook{before: func(action string) {
		switch action {
		case "ha/prepare":
			useMaster(t, m.d.DB, "alice", 11) // 冻结之后、导出整库之前:照常记进用户用量,随整库数据过去
		case "ha/handover":
			useMaster(t, m.d.DB, "alice", 60) // 整库数据已经导出:只能靠另记的那本账
		case "ha/commit":
			useAgent(t, tgt.d.DB, "alice", 40) // 目标暂存之后自己记的
			useMaster(t, m.d.DB, "alice", 7)
		}
	}})
	if _, err := m.SetMaster(tid, "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	if st := waitSwitchDone(t, m); st.Stage != StageDone || !m.IsNode() {
		t.Fatalf("设为主机应完成、原主机变为副机: %+v", st)
	}
	useAgent(t, m.d.DB, "alice", 3) // 原主机变为副机之后记的
	ndb := swapIn(t, tgt)
	mergeFrom(t, ndb, mid, "HK", m.d.DB)
	mergeFrom(t, ndb, mid, "HK", m.d.DB) // 重复的报告不重复计
	want := base + 11 + 20 + 40 + 60 + 7 + 3
	if got := downOf(t, ndb, "alice"); got != want {
		t.Fatalf("交接前后每一段都应计费、只计一次:应是 %d,实际 %d", want, got)
	}
	useAgent(t, m.d.DB, "alice", 1)
	mergeFrom(t, ndb, mid, "HK", m.d.DB)
	if got := downOf(t, ndb, "alice"); got != want+1 {
		t.Fatalf("之后按增量,应是 %d,实际 %d", want+1, got)
	}
}

// 设为主机没做成(本机照旧当主机):交接途中本机流量一直照常记在用户用量上,另记的那本账不用、清掉,不重计。
func TestSetMasterFailedDropsSelfLedger(t *testing.T) {
	cl := readyCluster(t, 2)
	m := cl.m
	base := downOf(t, m.d.DB, "alice")
	wrapAgent(t, cl, 0, agentHook{before: func(action string) {
		if action == "ha/handover" {
			useMaster(t, m.d.DB, "alice", 60)
			cl.clk.add(switchFreezeMax) // 推数据推了很久:来不及交接
		}
	}})
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	if st := waitSwitchDone(t, m); st.Stage != StageFailed || m.IsNode() {
		t.Fatalf("应失败收尾、照旧是主机: %+v", st)
	}
	var n int64
	m.d.DB.Model(&model.AgentCounter{}).Count(&n)
	if got := downOf(t, m.d.DB, "alice"); got != base+60 || n != 0 || m.setting(KeySelfLedger) != "" {
		t.Fatalf("用户用量照常记(应 %d,实际 %d),另记的账本清掉(剩 %d 本,开关 %q)", base+60, got, n, m.setting(KeySelfLedger))
	}
}
