package ha

import (
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// ---- 交接收尾没清成的另记账本:定时收尾只清它看见的那一本,不碰新开的 ----

func selfLedgerDown(t *testing.T, s *Service, name string) int64 {
	t.Helper()
	var c model.AgentCounter
	s.d.DB.Where("user_name = ?", name).Limit(1).Find(&c)
	return c.Down
}

// 上一次设为主机失败、收尾时清另记账本又出了错,开关(haSelfLedger)留着。定时收尾刚看过"没在设为主机",管理员正好又发起了
// 一次设为主机:冻结、另起一本账、开始记本机流量。以前收尾接着读到开关有值,就把开关连同账本一起清掉 —— 清的是新开的这本,
// 导出的整库数据里没有它,原主机从导出到变为副机之间的流量就丢了。
func TestLeftoverSelfLedgerCleanupSparesStartingSwitch(t *testing.T) {
	s := testService(t, false, map[string]string{KeySelfLedger: "e-left", KeyFreezeUntil: "1"})
	s.d.DB.Create(&model.User{Name: "alice", Enabled: true})
	s.d.DB.Create(&model.AgentCounter{UserName: "alice", Up: 5, Down: 5}) // 上次留下的
	var once sync.Once
	s.SetNow(func() time.Time {
		once.Do(func() {
			// 收尾已经看过"没在设为主机":这时管理员发起了设为主机,另起一本账,本机流量开始同时记进这本账
			s.sw.mu.Lock()
			s.sw.running = true
			s.sw.mu.Unlock()
			if err := s.startSelfLedger(); err != nil {
				t.Error(err)
			}
			useMaster(t, s.d.DB, "alice", 40)
		})
		return time.Now()
	})
	s.sw.tick()
	if v := setting(s.d.DB, KeySelfLedger); v == "" || v == "e-left" {
		t.Fatalf("正在进行的设为主机另起的那本账,开关不能被收尾清掉: %q", v)
	}
	if got := selfLedgerDown(t, s, "alice"); got != 40 {
		t.Fatalf("新开的这本账记着 40,不能被收尾清掉,实际 %d", got)
	}
}

// 收尾看见的是上次留下的那本(e-left),还没来得及清,管理员又发起了设为主机、另起了新的一本:收尾只能清它看见的那一本。
// 没人动过的照常清掉;本机已经变为副机的不清(账本是副机自己的账)。
func TestLeftoverSelfLedgerDropOnlySeenEpoch(t *testing.T) {
	s := testService(t, false, map[string]string{KeySelfLedger: "e-left"})
	s.d.DB.Create(&model.User{Name: "alice", Enabled: true})
	s.d.DB.Create(&model.AgentCounter{UserName: "alice", Up: 5, Down: 5})
	e := s.selfLedgerLeftover()
	if e != "e-left" {
		t.Fatalf("应看见上次留下的开关: %q", e)
	}
	if err := s.startSelfLedger(); err != nil { // 收尾还没清,新的设为主机另起了一本账
		t.Fatal(err)
	}
	useMaster(t, s.d.DB, "alice", 40)
	s.dropSelfLedger(e)
	if v := setting(s.d.DB, KeySelfLedger); v == "" || v == "e-left" {
		t.Fatalf("新开的这本账的开关不能被清掉: %q", v)
	}
	if got := selfLedgerDown(t, s, "alice"); got != 40 {
		t.Fatalf("新开的这本账记着 40,不能被清掉,实际 %d", got)
	}

	// 没人动过的留下来的那本:照常清掉(开关、账本一起)
	e = s.selfLedgerLeftover()
	s.dropSelfLedger(e)
	var n int64
	s.d.DB.Model(&model.AgentCounter{}).Count(&n)
	if v := setting(s.d.DB, KeySelfLedger); v != "" || n != 0 {
		t.Fatalf("留下来的那本应连同开关一起清掉: %q,还剩 %d 本", v, n)
	}

	// 已经变为副机:账本是副机自己的账,不清
	node := testService(t, false, map[string]string{KeySelfLedger: "e-node"})
	node.d.DB.Create(&model.AgentCounter{UserName: "alice", Up: 7, Down: 7})
	node.d.DB.Create(&model.Setting{Key: "nodeMode", Value: "true"})
	if got := node.selfLedgerLeftover(); got != "" {
		t.Fatalf("副机没有要收尾的另记账本: %q", got)
	}
	node.dropSelfLedger("e-node")
	if got := selfLedgerDown(t, node, "alice"); got != 7 {
		t.Fatalf("变为副机之后账本是副机自己的账,不能清,实际 %d", got)
	}
}
