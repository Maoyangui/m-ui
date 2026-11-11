package usersess

import (
	"testing"
	"time"
)

// quicLike 仿 QUIC 会话:closer 只发起关闭,连接要过一会儿(它自己的事件循环排上)才真正断开,done 这时才关。
func quicLike(r *Registry, lag time.Duration) (*Session, <-chan struct{}) {
	done := make(chan struct{})
	s := r.begin(func() {
		go func() {
			time.Sleep(lag)
			close(done)
		}()
	}, done, nil)
	return s, done
}

func isShut(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// 换表 / 踢线 / 入站关闭返回时,被关的连接必须已经真正断开 —— 不能只是发了个信号。
// 以前 QUIC 会话发完 CloseWithError 的信号就返回,机器一忙,老连接在"已经断开"之后还能收发
// (端到端测试:踢线 / 移除 / 换密码之后的探测字节被老连接回显回来,报"连接还活着")。
func TestBatchCloseWaitsUntilShut(t *testing.T) {
	const lag = 30 * time.Millisecond
	r := New(creds("alice", "pa", "bob", "pb"))
	set := NewSet()
	set.Add(r)

	a, aDone := quicLike(r, lag)
	r.Admit(a, "alice")
	if n := set.CloseUsers([]string{"alice"}); n != 1 || !isShut(aDone) {
		t.Fatalf("Set.CloseUsers 返回时连接应已断开: n=%d 断开=%v", n, isShut(aDone))
	}

	b, bDone := quicLike(r, lag)
	r.Admit(b, "bob")
	next := creds("alice", "pa") // 移除 bob
	r.Prepare(next)
	if _, _, n := r.Commit(next); n != 1 || !isShut(bDone) {
		t.Fatalf("Commit 返回时被移除用户的连接应已断开: n=%d 断开=%v", n, isShut(bDone))
	}

	c, cDone := quicLike(r, lag)
	if !r.Admit(c, "alice") {
		t.Fatal("踢线之后新建的会话应放行")
	}
	if n := r.CloseUsers([]string{"alice"}); n != 1 || !isShut(cDone) {
		t.Fatalf("CloseUsers 返回时连接应已断开: n=%d 断开=%v", n, isShut(cDone))
	}

	d, dDone := quicLike(r, lag)
	r.Admit(d, "alice")
	if n := r.CloseAll(); n != 1 || !isShut(dDone) {
		t.Fatalf("CloseAll 返回时连接应已断开: n=%d 断开=%v", n, isShut(dDone))
	}
}

// 关掉的会话要等连接结束后另起协程才注销(End),在那之前还挂在登记表里:
// 紧接着的下一次踢线 / 换表不能把它再算一条(以前"移除应关 1 条,实际 2",报给面板的断开数也跟着多)。
func TestBatchCloseCountsOnlyNewlyShut(t *testing.T) {
	r := New(creds("alice", "pa"))
	var c1, c2 closeCounter
	old := r.Begin(c1.closer(), nil)
	r.Admit(old, "alice")
	if n := r.CloseUsers([]string{"alice"}); n != 1 {
		t.Fatalf("踢线应关 1 条,实际 %d", n)
	}
	// old 还没 End:再踢一次,没有新关掉的
	if n := r.CloseUsers([]string{"alice"}); n != 0 {
		t.Fatalf("已关掉、还没注销的会话不该再算一次,实际 %d", n)
	}
	fresh := r.Begin(c2.closer(), nil)
	if !r.Admit(fresh, "alice") {
		t.Fatal("踢线之后新建的会话应放行")
	}
	next := creds() // 移除 alice
	r.Prepare(next)
	if _, _, n := r.Commit(next); n != 1 {
		t.Fatalf("移除 alice 应只算她新的那 1 条,实际 %d", n)
	}
	if c1.n.Load() != 1 || c2.n.Load() != 1 {
		t.Fatalf("每条会话的 closer 应只调一次,实际 %d / %d", c1.n.Load(), c2.n.Load())
	}
}

// 关不掉的连接(事件循环卡死)不能让换表 / 踢线一直挂着:最多等 closeWaitLimit。
func TestBatchCloseWaitIsBounded(t *testing.T) {
	saved := closeWaitLimit
	closeWaitLimit = 50 * time.Millisecond
	defer func() { closeWaitLimit = saved }()
	r := New(creds("alice", "pa"))
	stuck := r.begin(func() {}, make(chan struct{}), nil) // 永远不会断开
	r.Admit(stuck, "alice")
	start := time.Now()
	if n := r.CloseUsers([]string{"alice"}); n != 1 {
		t.Fatalf("踢线应关 1 条,实际 %d", n)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > 2*time.Second {
		t.Fatalf("应等到上限(50ms)再返回,实际等了 %v", d)
	}
}
