package usersess

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
)

// 记一条会话被关了几次的 closer。
type closeCounter struct{ n atomic.Int32 }

func (c *closeCounter) closer() func() { return func() { c.n.Add(1) } }

func creds(pairs ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return m
}

// 表完全没变、只换顺序:一个会话都不关。这是最高优先级的性质 —— 每次主机推快照都会热更新一遍。
func TestCommitUnchangedTableClosesNothing(t *testing.T) {
	r := New(creds("alice", "pa", "bob", "pb"))
	var ca, cb closeCounter
	a := r.Begin(ca.closer(), nil)
	b := r.Begin(cb.closer(), nil)
	if !r.Admit(a, "alice") || !r.Admit(b, "bob") {
		t.Fatal("初始用户应放行")
	}
	for i := 0; i < 3; i++ {
		next := creds("bob", "pb", "alice", "pa") // 顺序反过来
		r.Prepare(next)
		removed, changed, closed := r.Commit(next)
		if len(removed)+len(changed)+closed != 0 {
			t.Fatalf("表没变却关了会话: removed=%v changed=%v closed=%d", removed, changed, closed)
		}
	}
	if ca.n.Load()+cb.n.Load() != 0 {
		t.Fatal("表没变不该关任何会话")
	}
	if !r.Admit(a, "alice") {
		t.Fatal("换表后老会话应仍可用")
	}
}

// 移除用户:只关他的会话;新增用户:不关任何会话;换密码:关他的会话。
func TestCommitRemovedAndChanged(t *testing.T) {
	r := New(creds("alice", "pa", "bob", "pb"))
	var ca, cb closeCounter
	a := r.Begin(ca.closer(), nil)
	b := r.Begin(cb.closer(), nil)
	r.Admit(a, "alice")
	r.Admit(b, "bob")

	next := creds("bob", "pb", "carol", "pc") // 删 alice、加 carol
	r.Prepare(next)
	removed, changed, closed := r.Commit(next)
	if len(removed) != 1 || removed[0] != "alice" || len(changed) != 0 || closed != 1 {
		t.Fatalf("应只移除 alice 并关她一条会话: removed=%v changed=%v closed=%d", removed, changed, closed)
	}
	if ca.n.Load() != 1 || cb.n.Load() != 0 {
		t.Fatalf("alice 应被关 1 次、bob 0 次,实际 %d / %d", ca.n.Load(), cb.n.Load())
	}
	if r.Admit(a, "alice") {
		t.Fatal("被移除的用户不该再放行")
	}
	// carol 刚加进来,Prepare 之后就能通过
	c := r.Begin(nil, nil)
	if !r.Admit(c, "carol") {
		t.Fatal("新增用户应放行")
	}

	next2 := creds("bob", "pb2", "carol", "pc") // bob 换密码
	r.Prepare(next2)
	removed, changed, closed = r.Commit(next2)
	if len(removed) != 0 || len(changed) != 1 || changed[0] != "bob" || closed != 1 || cb.n.Load() != 1 {
		t.Fatalf("bob 换密码应只关他: removed=%v changed=%v closed=%d cb=%d", removed, changed, closed, cb.n.Load())
	}
	// bob 用新密码重连:新会话在撤销点之后,放行
	b2 := r.Begin(nil, nil)
	if !r.Admit(b2, "bob") {
		t.Fatal("换密码后的新会话应放行")
	}
}

// 两段式:Prepare 之后、Commit 之前,新名字已经能通过;Commit 之前没被 Prepare 的名字不放行。
func TestPrepareAdmitsNewNamesBeforeCommit(t *testing.T) {
	r := New(creds("alice", "pa"))
	s := r.Begin(nil, nil)
	if r.Admit(s, "dave") {
		t.Fatal("还没进表的名字不该放行")
	}
	r.Prepare(creds("alice", "pa", "dave", "pd"))
	if !r.Admit(s, "dave") {
		t.Fatal("Prepare 之后新名字应放行")
	}
}

// 踢线:关已绑定的会话;撤销前建立、还没开流的会话之后开第一条流也被拒;撤销后建立的放行。
func TestCloseUsersRevokesEarlierSessions(t *testing.T) {
	r := New(creds("alice", "pa", "alice#share", "ps"))
	var c1, c2, c3 closeCounter
	bound := r.Begin(c1.closer(), nil)
	r.Admit(bound, "alice")
	idle := r.Begin(c2.closer(), nil) // 建立了但还没开流
	if n := r.CloseUsers([]string{"alice"}); n != 1 || c1.n.Load() != 1 {
		t.Fatalf("应只关已绑定的那条: n=%d c1=%d", n, c1.n.Load())
	}
	if r.Admit(idle, "alice") {
		t.Fatal("撤销点之前建立的会话开第一条流应被拒")
	}
	after := r.Begin(c3.closer(), nil)
	if !r.Admit(after, "alice") {
		t.Fatal("撤销点之后建立的会话应放行")
	}
	// 共享凭据的名字不受影响(精确匹配)
	sh := r.Begin(nil, nil)
	if !r.Admit(sh, "alice#share") {
		t.Fatal("没被点名的共享凭据不该受影响")
	}
	if c2.n.Load() != 0 || c3.n.Load() != 0 {
		t.Fatal("没绑定的和撤销后的会话不该被关")
	}
}

// 已结束的会话不能再被绑定回来(晚到的流),也不能污染按地址查找。
func TestEndedSessionCannotBeRebound(t *testing.T) {
	r := New(creds("alice", "pa"))
	ap := netip.MustParseAddrPort("1.2.3.4:5000")
	s := r.Begin(nil, func() netip.AddrPort { return ap })
	r.End(s)
	if r.Admit(s, "alice") {
		t.Fatal("已结束的会话不该被绑定")
	}
	if r.Bound("alice") != 0 || r.Len() != 0 {
		t.Fatal("已结束的会话不该留在登记表里")
	}
	if r.FindByRemote("alice", ap) != nil {
		t.Fatal("已结束的会话不该按地址找到")
	}
}

// 登记表关门之后 Begin 的会话立刻被关,Admit 一律拒。
func TestClosedRegistry(t *testing.T) {
	r := New(creds("alice", "pa"))
	var c closeCounter
	s := r.Begin(c.closer(), nil)
	r.Admit(s, "alice")
	if n := r.CloseAll(); n != 1 || c.n.Load() != 1 {
		t.Fatalf("CloseAll 应关掉全部: n=%d c=%d", n, c.n.Load())
	}
	var late closeCounter
	l := r.Begin(late.closer(), nil)
	if !l.Closed() || late.n.Load() != 1 {
		t.Fatal("关门之后登记的会话应立刻被关")
	}
	if r.Admit(l, "alice") {
		t.Fatal("关门之后 Admit 应拒")
	}
}

// 双栈监听下 IPv4 客户端的地址是 ::ffff:a.b.c.d,和 Unwrap 过的地址要认成同一个。
func TestFindByRemoteUnmapsV4InV6(t *testing.T) {
	r := New(creds("alice", "pa"))
	mapped := netip.MustParseAddrPort("[::ffff:1.2.3.4]:5000")
	s := r.Begin(nil, func() netip.AddrPort { return mapped })
	if got := r.FindByRemote("alice", netip.MustParseAddrPort("1.2.3.4:5000")); got != s {
		t.Fatal("v4-mapped 地址应命中同一条会话")
	}
	r.Admit(s, "alice")
	if got := r.FindByRemote("bob", netip.MustParseAddrPort("1.2.3.4:5000")); got != nil {
		t.Fatal("已绑定到别人的会话不该按地址给出去")
	}
	// 连接迁移:对端地址变了,按当前地址在已绑定的会话里找
	moved := netip.MustParseAddrPort("5.6.7.8:1")
	s2 := r.Begin(nil, func() netip.AddrPort { return moved })
	r.Admit(s2, "alice")
	if got := r.FindByRemote("alice", moved); got != s2 {
		t.Fatal("按当前地址应找到迁移后的会话")
	}
}

// 关闭是幂等的,重复关不会把 closer 调两次。
func TestCloseIdempotent(t *testing.T) {
	r := New(creds("alice", "pa"))
	var c closeCounter
	s := r.Begin(c.closer(), nil)
	s.Close()
	s.Close()
	if c.n.Load() != 1 {
		t.Fatalf("closer 应只调一次,实际 %d", c.n.Load())
	}
	var nilSess *Session
	nilSess.Close() // nil 安全
}

// 撤销点会被修剪:比所有存活会话都早的记录不再保留。
func TestChangedAtPruned(t *testing.T) {
	r := New(creds("alice", "pa"))
	for i := 0; i < 50; i++ {
		r.CloseUsers([]string{"alice"})
	}
	r.mu.Lock()
	n := len(r.changedAt)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("没有存活会话时撤销点应全部修剪掉,实际还剩 %d", n)
	}
}

// 并发:Begin / Admit / Commit / CloseUsers / End 同时跑不许有竞态(用 -race 跑)。
func TestConcurrentAccess(t *testing.T) {
	r := New(creds("alice", "pa", "bob", "pb"))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := []string{"alice", "bob"}[i%2]
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := r.Begin(nil, func() netip.AddrPort { return netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), uint16(1000+i)) })
				r.Admit(s, name)
				r.FindByRemote(name, netip.MustParseAddrPort("10.0.0.1:1000"))
				r.End(s)
			}
		}(i)
	}
	for i := 0; i < 200; i++ {
		next := creds("alice", "pa", "bob", "pb")
		if i%3 == 0 {
			next["alice"] = "changed" // 时不时换一下密码
		}
		r.Prepare(next)
		r.Commit(next)
		r.CloseUsers([]string{"bob"})
	}
	close(stop)
	wg.Wait()
}

// Set:踢线只扫在册的入站;Remove 幂等;nil 安全。
func TestSet(t *testing.T) {
	set := NewSet()
	r1 := New(creds("alice", "pa"))
	r2 := New(creds("alice", "pa"))
	var c1, c2 closeCounter
	s1 := r1.Begin(c1.closer(), nil)
	s2 := r2.Begin(c2.closer(), nil)
	r1.Admit(s1, "alice")
	r2.Admit(s2, "alice")
	set.Add(r1)
	set.Add(r2)
	set.Remove(r2)
	set.Remove(r2)
	if n := set.CloseUsers([]string{"alice"}); n != 1 || c1.n.Load() != 1 || c2.n.Load() != 0 {
		t.Fatalf("只该关在册入站的会话: n=%d c1=%d c2=%d", n, c1.n.Load(), c2.n.Load())
	}
	var nilSet *Set
	if nilSet.CloseUsers([]string{"alice"}) != 0 || nilSet.Len() != 0 {
		t.Fatal("nil Set 应安全")
	}
}
