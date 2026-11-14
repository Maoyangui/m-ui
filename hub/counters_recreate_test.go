package hub

import (
	"testing"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// recreateRig 一台主机 + 一台副机(n2,id 2)的记账链路:副机按主机快照换用户表、清账本、记流量、上报,主机并账。
type recreateRig struct {
	t           *testing.T
	host, agent *gorm.DB
}

func newRecreateRig(t *testing.T) *recreateRig {
	r := &recreateRig{t: t, host: openDB(t, "m.db").DB, agent: openDB(t, "a.db").DB}
	r.host.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	r.host.Create(&model.Node{Name: "n2", Enabled: true})
	return r
}

// newUser 主机上建一个 alice(建号时间各不相同)。
func (r *recreateRig) newUser(createdAt int64) model.User {
	u := model.User{Name: "alice", Enabled: true, CreatedAt: createdAt}
	if err := r.host.Create(&u).Error; err != nil {
		r.t.Fatal(err)
	}
	return u
}

// sync 副机应用主机快照:用户表整表换成主机那份(id、建号时间原样),再按 agent/apply 的做法清账本。
func (r *recreateRig) sync() {
	r.t.Helper()
	r.syncUsersOnly()
	if err := model.PruneAgentCounters(r.agent, true); err != nil {
		r.t.Fatal(err)
	}
}

// syncUsersOnly 只换了用户表、还没来得及清账本(记账和应用快照交错)。
func (r *recreateRig) syncUsersOnly() {
	r.t.Helper()
	if err := r.agent.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.User{}).Error; err != nil {
		r.t.Fatal(err)
	}
	var us []model.User
	r.host.Find(&us)
	if len(us) > 0 {
		if err := r.agent.Create(&us).Error; err != nil {
			r.t.Fatal(err)
		}
	}
}

// use 副机这一轮记了 alice 上下行各 n 字节。
func (r *recreateRig) use(n int64) {
	r.t.Helper()
	if err := model.AddAgentCounters(r.agent, []model.AgentCounter{{UserName: "alice", Up: n, Down: n}}, 100); err != nil {
		r.t.Fatal(err)
	}
}

// report 副机把整本账报上来,主机并账。
func (r *recreateRig) report(at int64) MergeResult {
	r.t.Helper()
	var cs []model.AgentCounter
	if err := r.agent.Find(&cs).Error; err != nil {
		r.t.Fatal(err)
	}
	res, err := MergeCounters(r.host, 2, "n2", "", cs, at, 60, 1)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func (r *recreateRig) usage(id uint) int64 {
	r.t.Helper()
	var u model.User
	if err := r.host.First(&u, id).Error; err != nil {
		r.t.Fatal(err)
	}
	return u.Down
}

func (r *recreateRig) nodeDown() int64 {
	var v int64
	r.host.Model(&model.Stats{}).Where("resource = ? AND tag = ? AND direction = ?", "node", "n2", false).
		Select("COALESCE(SUM(traffic), 0)").Scan(&v)
	return v
}

// Azure 真机 run3:同名用户删了又建,副机应用了删用户的快照、账本清了,新用户在这台机器上从 0 记,
// 主机收到的第一份报告里计数已经追上甚至超过旧用户留下的游标。以前按"计数没变小 = 同一本账"只计差值,
// 相等时直接当"没变化"跳过 —— 旧用户在这台机器上用过多少,新用户就有多少不计费(下载 60 MB 面板记 0)。
func TestRecreatedUserFirstReportReachesOldCursor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first int64
	}{{"首份等于旧游标", 1000}, {"首份超过旧游标", 1600}, {"首份小于旧游标", 300}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecreateRig(t)
			old := r.newUser(100)
			r.sync()
			r.use(1000)
			r.report(1000)
			if got := r.usage(old.Id); got != 1000 {
				t.Fatalf("旧用户应记 1000,实际 %d", got)
			}
			r.host.Delete(&model.User{}, old.Id)
			r.sync() // 副机应用了删掉 alice 的快照:账本清了
			fresh := r.newUser(200)
			r.sync()
			r.use(tc.first)
			r.report(1005)
			if got := r.usage(fresh.Id); got != tc.first {
				t.Fatalf("删了又建的同名用户在这台机器上用了 %d,应全部计费,实际记 %d", tc.first, got)
			}
			r.use(100)
			r.report(1010)
			r.report(1015) // 同一份重复报:不重复计
			if got := r.usage(fresh.Id); got != tc.first+100 {
				t.Fatalf("之后按增量计,应是 %d,实际 %d", tc.first+100, got)
			}
			if got := r.nodeDown(); got != 1000+tc.first+100 {
				t.Fatalf("服务器维度应是两个用户之和 %d,实际 %d", 1000+tc.first+100, got)
			}
		})
	}
}

// 删了又建发生在同一轮同步里(副机没见过"没有 alice"的那份快照):旧用户那本账在应用快照时换掉,
// 新用户从 0 记;记账和应用快照交错(用户表换了、账本还没清)时,记账自己按账本主人换本,不把新流量接在旧账上。
func TestRecreatedUserWithinOneSyncRound(t *testing.T) {
	r := newRecreateRig(t)
	old := r.newUser(100)
	r.sync()
	r.use(1000)
	r.report(1000)
	r.host.Delete(&model.User{}, old.Id)
	fresh := r.newUser(200)
	r.syncUsersOnly()
	r.use(1500) // 新 alice 的流量,账本还是旧 alice 的那本
	var c model.AgentCounter
	r.agent.First(&c, "user_name = ?", "alice")
	if c.Down != 1500 || c.UserId != fresh.Id || c.UserCreatedAt != 200 {
		t.Fatalf("账本主人换了应换一本从 0 记:%+v", c)
	}
	if err := model.PruneAgentCounters(r.agent, true); err != nil {
		t.Fatal(err)
	}
	r.report(1005)
	if got := r.usage(fresh.Id); got != 1500 {
		t.Fatalf("新用户在这台机器上用了 1500,应全部计费,实际 %d", got)
	}
	// 同一轮里删了又建、副机先应用了快照才记账:旧账本按主人对不上清掉
	r.host.Delete(&model.User{}, fresh.Id)
	third := r.newUser(300)
	r.sync()
	var n int64
	r.agent.Model(&model.AgentCounter{}).Count(&n)
	if n != 0 {
		t.Fatalf("主人已经不在的账本应在应用快照时清掉,还剩 %d 本", n)
	}
	r.use(1500) // 与游标相等
	r.report(1010)
	if got := r.usage(third.Id); got != 1500 {
		t.Fatalf("第三个 alice 用了 1500,应全部计费,实际 %d", got)
	}
}

// 主机上删了又建之后,副机还没收到快照、报来的是旧用户那本账的在途流量:不算到新用户头上(服务器维度照记)。
func TestInflightOldLedgerNotBilledToRecreatedUser(t *testing.T) {
	r := newRecreateRig(t)
	old := r.newUser(100)
	r.sync()
	r.use(1000)
	r.report(1000)
	r.host.Delete(&model.User{}, old.Id)
	fresh := r.newUser(200)
	r.use(50) // 旧 alice 的尾巴
	r.report(1005)
	if got := r.usage(fresh.Id); got != 0 {
		t.Fatalf("旧用户的在途流量不该记到新用户头上,实际记了 %d", got)
	}
	if got := r.nodeDown(); got != 1050 {
		t.Fatalf("服务器维度照记,应是 1050,实际 %d", got)
	}
	r.sync()
	r.use(1200)
	r.report(1010)
	if got := r.usage(fresh.Id); got != 1200 {
		t.Fatalf("新用户在这台机器上用了 1200,应全部计费,实际 %d", got)
	}
}

// 兼容:升级前的账本(没记主人,UserId 为 0)照老办法并 —— 计数没变小按增量;记账时也不给老账本认主人
// (它可能是同一轮里删掉的前一个同名用户的,认了主机会当成新账本从 0 计,把前人的用量记到新用户头上)。
// 升级前留下的游标(不知道是谁的)遇到记了主人的账本:不一定是另一本 —— 升级是一台一台做的,先升副机时同一本账
// 也会碰上老主机写的游标(见 TestMixedVersionAgentUpgradedFirst),只能按计数判断:没变小按增量,变小从 0 计。
func TestAgentLedgerIdentityUpgradeCompat(t *testing.T) {
	r := newRecreateRig(t)
	u := r.newUser(100)
	r.sync()
	r.agent.Create(&model.AgentCounter{UserName: "alice", Up: 1000, Down: 1000}) // 升级前的账本
	r.host.Create(&model.TrafficCursor{NodeId: 2, UserName: "alice", Up: 900, Down: 900})
	r.use(10)
	var c model.AgentCounter
	r.agent.First(&c, "user_name = ?", "alice")
	if c.Down != 1010 || c.UserId != 0 {
		t.Fatalf("老账本应接着记、不认主人:%+v", c)
	}
	r.report(1000)
	if got := r.usage(u.Id); got != 110 {
		t.Fatalf("老账本按增量计,应是 110,实际 %d", got)
	}
	// 另两个用户:升级前删掉的同名用户留下了游标(不知道是谁的),升级后新开的账本记了主人。
	// 首份比游标小:是新开的一本,从 0 计;不比游标小:分不出是不是同一本,按同一本只计增量 ——
	// 少计这一段只在过渡期发生一次,当成另一本从 0 计却会在先升副机时把整本账再计一遍
	bob := model.User{Name: "bob", Enabled: true, CreatedAt: 500}
	carol := model.User{Name: "carol", Enabled: true, CreatedAt: 600}
	r.host.Create(&bob)
	r.host.Create(&carol)
	r.host.Create(&model.TrafficCursor{NodeId: 2, UserName: "bob", Up: 700, Down: 700})
	r.host.Create(&model.TrafficCursor{NodeId: 2, UserName: "carol", Up: 700, Down: 700})
	r.sync()
	if err := model.AddAgentCounters(r.agent, []model.AgentCounter{{UserName: "bob", Up: 800, Down: 800}, {UserName: "carol", Up: 300, Down: 300}}, 100); err != nil {
		t.Fatal(err)
	}
	res := r.report(1005)
	if b, c := r.usageOf("bob"), r.usageOf("carol"); b != 100 || c != 300 || res.Restarted != 1 || res.Recreated != 0 {
		t.Fatalf("不知道是哪一本的游标按计数判断:bob 按增量 100、carol 从 0 计 300,实际 %d、%d(%+v)", b, c, res)
	}
	// 游标记下了主人和代号:之后按代号认账本
	if err := model.AddAgentCounters(r.agent, []model.AgentCounter{{UserName: "bob", Up: 10, Down: 10}}, 100); err != nil {
		t.Fatal(err)
	}
	r.report(1007)
	if b := r.usageOf("bob"); b != 110 {
		t.Fatalf("同一本按增量,bob 应是 110,实际 %d", b)
	}
	// 老版本副机(上报不带主人):行为不变
	if _, err := MergeCounters(r.host, 2, "n2", "", []model.AgentCounter{{UserName: "bob", Up: 850, Down: 850}}, 1010, 60, 1); err != nil {
		t.Fatal(err)
	}
	if b := r.usageOf("bob"); b != 150 {
		t.Fatalf("老副机的上报按增量计,应是 150,实际 %d", b)
	}
}

// 同一个用户(主人对得上)的账本计数变小仍是账本回退:游标对齐、不计,不当成新账本把整段重算。
func TestAgentLedgerRegressSameOwner(t *testing.T) {
	r := newRecreateRig(t)
	u := r.newUser(100)
	r.sync()
	r.use(1000)
	r.report(1000)
	r.agent.Model(&model.AgentCounter{}).Where("user_name = ?", "alice").Updates(map[string]interface{}{"up": 900, "down": 900})
	if res := r.report(1005); res.Regressed != 1 {
		t.Fatalf("应按账本回退处理: %+v", res)
	}
	r.use(50)
	r.report(1010)
	if got := r.usage(u.Id); got != 1050 {
		t.Fatalf("回退那段不计、之后按增量,应是 1050,实际 %d", got)
	}
}

// 记账时副机上已经没有这个用户(刚删,数据面还没重载完):这段尾巴记成"主人不在"的账本,不算到任何用户头上
// (之后同名的新用户也不会接着它记),服务器维度照记;下次应用快照清掉。
func TestOrphanAgentLedgerNotBilled(t *testing.T) {
	r := newRecreateRig(t)
	// 主机上还有别的用户:一个用户都不剩的空用户表(停用副机)不清账本,见 TestAgentLedgerSurvivesDecommission
	r.host.Create(&model.User{Name: "bob", Enabled: true, CreatedAt: 50})
	old := r.newUser(100)
	r.sync()
	r.use(1000)
	r.report(1000)
	r.host.Delete(&model.User{}, old.Id)
	r.sync()
	r.use(20) // 被删用户的尾巴
	var c model.AgentCounter
	r.agent.First(&c, "user_name = ?", "alice")
	if c.Down != 20 || c.UserId != 0 || c.UserCreatedAt != model.OwnerAbsent {
		t.Fatalf("本机没有这个用户时应记成主人不在的账本: %+v", c)
	}
	fresh := r.newUser(200)
	r.report(1005)
	if got := r.usage(fresh.Id); got != 0 {
		t.Fatalf("主人不在的账本不该记到同名新用户头上,实际 %d", got)
	}
	if got := r.nodeDown(); got != 1020 {
		t.Fatalf("服务器维度照记,应是 1020,实际 %d", got)
	}
	r.syncUsersOnly()
	r.use(1000) // 新用户的流量:换一本从 0 记,不接在尾巴上
	r.agent.First(&c, "user_name = ?", "alice")
	if c.Down != 1000 || c.UserId != fresh.Id {
		t.Fatalf("新用户应换一本从 0 记: %+v", c)
	}
	r.report(1010)
	if got := r.usage(fresh.Id); got != 1000 {
		t.Fatalf("新用户用了 1000,应全部计费,实际 %d", got)
	}
}

// decommission 停用副机:主机推一份空用户表,副机整表换成空的、照常清账本(见 agent/apply)。
func (r *recreateRig) decommission() {
	r.t.Helper()
	if err := r.agent.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.User{}).Error; err != nil {
		r.t.Fatal(err)
	}
	if err := model.PruneAgentCounters(r.agent, true); err != nil {
		r.t.Fatal(err)
	}
}

// 副机的账本被整本清掉后同一个主人从 0 重开一本(改名后再改回、老版本副机停用时清了账本):主人和游标一样,
// 但不是游标记的那一本。以前同一主人计数比游标小当成账本回退(对齐不计,还误报"账本回退"),
// 追上游标只计差值、相等时整份当没变化跳过 —— 每个用户漏掉 min(首份, 旧游标)。
func TestAgentLedgerReopenedSameOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first int64
	}{{"首份小于旧游标", 300}, {"首份等于旧游标", 1000}, {"首份超过旧游标", 1600}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecreateRig(t)
			u := r.newUser(100)
			r.sync()
			r.use(1000)
			r.report(1000)
			// 账本整本清掉(比如老版本副机停用时按空用户表清了账本),之后同一个用户重开一本
			if err := r.agent.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.AgentCounter{}).Error; err != nil {
				t.Fatal(err)
			}
			r.use(tc.first)
			res := r.report(1005)
			if res.Regressed != 0 {
				t.Fatalf("重开的账本不是账本回退,不该告警: %+v", res)
			}
			if got := r.usage(u.Id); got != 1000+tc.first {
				t.Fatalf("重开的账本应从 0 计,应是 %d,实际 %d", 1000+tc.first, got)
			}
			r.use(100)
			r.report(1010)
			r.report(1015)
			if got := r.usage(u.Id); got != 1000+tc.first+100 {
				t.Fatalf("之后按增量计,应是 %d,实际 %d", 1000+tc.first+100, got)
			}
		})
	}
}

// 改名后再改回:改名时旧名字那本账清掉,改回后同一个主人用回原名、从 0 重开一本;主机上原名的游标还是改名前那本的。
func TestAgentLedgerRenamedBack(t *testing.T) {
	r := newRecreateRig(t)
	u := r.newUser(100)
	r.sync()
	r.use(1000)
	r.report(1000)
	r.host.Model(&model.User{}).Where("id = ?", u.Id).Update("name", "bob")
	r.sync() // alice 那本清掉
	if err := model.AddAgentCounters(r.agent, []model.AgentCounter{{UserName: "bob", Up: 300, Down: 300}}, 100); err != nil {
		t.Fatal(err)
	}
	r.report(1005)
	r.host.Model(&model.User{}).Where("id = ?", u.Id).Update("name", "alice")
	r.sync() // bob 那本清掉
	r.use(200)
	res := r.report(1010)
	if res.Regressed != 0 {
		t.Fatalf("改回原名后重开的账本不是账本回退,不该告警: %+v", res)
	}
	if got := r.usage(u.Id); got != 1500 {
		t.Fatalf("改名前 1000 + 改名后 300 + 改回后 200,应是 1500,实际 %d", got)
	}
}

// 停用再启用:停用推的是空用户表,副机不再为任何人服务;账本不跟着清 —— 上次主机拉取之后还没报上去的那段
// (停用期间主机不再拉取)在重新启用后照常报上来计费,重新启用后接着同一本记,不误报"账本回退"。
func TestAgentLedgerSurvivesDecommission(t *testing.T) {
	r := newRecreateRig(t)
	u := r.newUser(100)
	r.sync()
	r.use(1000)
	r.report(1000)
	r.use(50) // 还没报上去,副机就被停用了
	r.decommission()
	r.sync() // 重新启用,推回完整快照
	r.use(100)
	res := r.report(1005)
	if res.Regressed != 0 {
		t.Fatalf("停用再启用不是账本回退,不该告警: %+v", res)
	}
	if got := r.usage(u.Id); got != 1150 {
		t.Fatalf("停用前没报上去的 50 和启用后的 100 都应计费,应是 1150,实际 %d", got)
	}
	// 停用期间主机删掉了这个用户:重新启用时那本照常清掉
	r.decommission()
	r.host.Delete(&model.User{}, u.Id)
	r.newUser(200)
	r.sync()
	var n int64
	r.agent.Model(&model.AgentCounter{}).Count(&n)
	if n != 0 {
		t.Fatalf("停用期间删掉的用户,重新启用时账本应清掉,还剩 %d 本", n)
	}
}
