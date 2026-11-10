package ha

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// testClock 可拨的钟。
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func fastReplicas(t *testing.T) {
	t.Helper()
	w, g := replicaWarmup, replicaMinGap
	replicaWarmup, replicaMinGap = 0, 0
	t.Cleanup(func() { replicaWarmup, replicaMinGap = w, g })
}

type haCluster struct {
	m      *Service
	h      interface{ RunOnce() }
	cands  []*Service
	agents []*miniAgent
	ids    []uint
	notes  *[]string
	clk    *testClock
}

// newCluster 一台开着容灾的主机(纪元 1、手动、候选 2 台)+ n 台副机。
func newCluster(t *testing.T, n int) *haCluster {
	t.Helper()
	fastReplicas(t)
	clk := &testClock{t: time.Now()}
	m := testService(t, false, map[string]string{KeyEpoch: "1", KeyMode: ModeConfirm, KeyStandbys: "2"})
	m.d.Now = clk.now
	var notes []string
	var mu sync.Mutex
	m.d.Notify = func(_, text string) {
		mu.Lock()
		notes = append(notes, text)
		mu.Unlock()
	}
	m.d.DB.Create(&model.Node{Name: "HK", IsLocal: true, Enabled: true, Sort: 1})
	m.d.DB.Create(&model.User{Name: "alice", Enabled: true, Up: 1})
	cl := &haCluster{m: m, notes: &notes, clk: clk}
	for i := 0; i < n; i++ {
		c := testService(t, true, map[string]string{})
		a := newMiniAgent(t, c, "tok")
		row := model.Node{Name: "TW" + string(rune('1'+i)), ApiUrl: a.url(), Token: "tok", Insecure: true, Enabled: true, Sort: 2 + i}
		m.d.DB.Create(&row)
		c.d.DB.Create(&model.Node{Id: row.Id, Name: row.Name, IsLocal: true, Enabled: true})
		cl.cands, cl.agents, cl.ids = append(cl.cands, c), append(cl.agents, a), append(cl.ids, row.Id)
	}
	cl.h = testHub(t, m)
	return cl
}

// 副本只发给按顺序的前 N 台合格候选;内容没变不重发、只推"截至";排除一台后名单顺延,被移出的删掉手里的副本;
// 候选公钥变了不再给它发、告警一次,重新信任后恢复。
func TestReplicatorCandidates(t *testing.T) {
	cl := newCluster(t, 3)
	m := cl.m
	cl.h.RunOnce() // 推容灾状态(副机切到纪元 1)、拉报告(拿到候选公钥)
	for i, c := range cl.cands {
		if c.Epoch() != 1 {
			t.Fatalf("副机 %d 应已切到纪元 1", i)
		}
	}
	if err := m.ReplicateNow(); err != nil {
		t.Fatalf("应发给前两台: %v", err)
	}
	r1, ok1 := cl.cands[0].replicaInfo()
	r2, ok2 := cl.cands[1].replicaInfo()
	_, ok3 := cl.cands[2].replicaInfo()
	if !ok1 || !ok2 || ok3 || r1.Seq == 0 || r1.Seq != r2.Seq {
		t.Fatalf("副本只给前两台候选,同一份: %v %v %v %+v %+v", ok1, ok2, ok3, r1, r2)
	}
	st, _ := cl.cands[0].NodeState()
	if st.rank() != 1 || len(st.Standbys) != 2 {
		t.Fatalf("候选要知道自己的名次: %+v", st)
	}
	// 内容没变:不重发,下一轮容灾状态把"截至"推过去
	cl.h.RunOnce()
	cl.clk.add(10 * time.Minute)
	before := cl.agents[0].count("ha/replica")
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	if cl.agents[0].count("ha/replica") != before {
		t.Fatal("内容没变不该重发副本")
	}
	cl.h.RunOnce()
	if ri, _ := cl.cands[0].replicaInfo(); ri.Seq != r1.Seq || ri.AsOf < cl.clk.now().Unix() {
		t.Fatalf("内容没变时候选那份的「截至」要更新: %+v (now %d)", ri, cl.clk.now().Unix())
	}
	// 内容变了:发新的一份
	m.d.DB.Model(&model.User{}).Where("name = ?", "alice").Update("up", 99)
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	if ri, _ := cl.cands[0].replicaInfo(); ri.Seq <= r1.Seq {
		t.Fatal("内容变了要发新的一份")
	}
	// 第一台挪到列表最后:备用顺延到第三台;第一台在下一轮状态里得知不再是备用,删掉副本
	m.d.DB.Model(&model.Node{}).Where("id = ?", cl.ids[0]).Update("sort", 99)
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	cl.h.RunOnce()
	if _, ok := cl.cands[0].replicaInfo(); ok {
		t.Fatal("不再是备用的要删掉手里的副本")
	}
	if _, ok := cl.cands[2].replicaInfo(); !ok {
		t.Fatal("名单顺延后第三台要收到副本")
	}
	if got := m.rep.standbyIds(); len(got) != 2 || got[0] != cl.ids[1] || got[1] != cl.ids[2] {
		t.Fatalf("备用按服务器顺序选: %v", got)
	}
	// 第二台的候选公钥变了(重装):不再发、告警一次;重新信任后恢复
	os.Remove(filepath.Join(cl.cands[1].Dir(), fileKey))
	cl.h.RunOnce()
	_ = m.ReplicateNow()
	var changed bool
	for _, c := range m.View(false).Standbys {
		if c.Id == cl.ids[1] {
			changed = c.Rank == 0 && strings.Contains(strings.Join(c.Reasons, ","), reasonPubChanged)
		}
	}
	if !changed {
		t.Fatalf("公钥变了要移出名单: %+v", m.View(false).Standbys)
	}
	_ = m.ReplicateNow()
	alerts := 0
	for _, n := range *cl.notes {
		if strings.Contains(n, "备用公钥变了") {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("公钥变了告警一次,实际 %d", alerts)
	}
	if err := m.Repin(cl.ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	if ri, ok := cl.cands[1].replicaInfo(); !ok || ri.Seq == 0 {
		t.Fatal("重新信任后恢复发副本")
	}
}

// 刚启动的主机(状态还没拉齐)用上次记下的名单:第一轮容灾状态不会让候选把副本删掉。
func TestReplicatorWarmupKeepsList(t *testing.T) {
	cl := newCluster(t, 1)
	cl.h.RunOnce()
	if err := cl.m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	// 模拟主机重启:新的容灾服务、新的 hub,状态都是空的
	m2 := New(Deps{DB: cl.m.d.DB, DBPath: cl.m.d.DBPath, Version: cl.m.d.Version})
	replicaWarmup = time.Hour
	h2 := testHub(t, m2)
	h2.RunOnce()
	if _, ok := cl.cands[0].replicaInfo(); !ok {
		t.Fatal("主机刚重启时第一轮容灾状态不能让候选删掉副本")
	}
	if got := m2.rep.standbyIds(); len(got) != 1 || got[0] != cl.ids[0] {
		t.Fatalf("刚启动时用上次记下的名单: %v", got)
	}
}
