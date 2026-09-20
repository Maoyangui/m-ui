// Package usersess 「用户 → 会话」登记表:让停用、删除、换凭据、踢线能关掉**整条**会话,而不只是里面的一条流。
//
// 为什么非要有它:hysteria2、TUIC、anytls 都是一条会话只鉴权一次,之后会话里开多少条流都不再看用户表。
// 面板停用一个用户只是把他从用户表里删掉、再把追踪器里的流关掉 —— 外层会话还活着,客户端立刻开新流,
// 照样放行、照样记账(2026-09-19 生产实证:用户被停用后又用了十几分钟)。
//
// 每个入站一张登记表:会话建立时登记(Begin),第一条流进来时按用户名绑定(Admit),会话结束时注销(End)。
// 换用户表走两段式(Prepare → 库换表 → Commit):Commit 按**名字**比对库实际用于认证的凭据,只关被移除的和凭据变了的;
// 用户表没变、只是顺序变了,一个会话都不关 —— 这一条错了就是全员掉线,因为每次主机推快照都会热更新一遍。
// 踢线(CloseUsers)按显式名字列表关,并给这些名字打一个撤销点:撤销之前建立、还没开过流的会话,开第一条流时也会被拒。
//
// 锁的约定:Registry.mu 是叶子锁,持有它时不调任何外部函数;真正关闭会话一律在锁外做。
// Set.mu → Registry.mu 是唯一允许的嵌套方向,Set 遍历时先复制再解锁。
package usersess

import (
	"net/netip"
	"sync"
	"sync/atomic"
)

// Session 一条会话(hy2 / TUIC 是一条 QUIC 连接,anytls 是一条 TLS 连接)。
type Session struct {
	reg    *Registry
	start  uint64                // 登记时的序号(与换表、撤销共用同一个单调计数器)
	user   string                // 第一条流进来时绑定,之后不再改;归 reg.mu 管
	closer func()                // 关整条会话;只会被调一次
	remote func() netip.AddrPort // 当前对端地址(QUIC 连接迁移后会变);TCP 会话为 nil
	addr   netip.AddrPort        // 登记时的对端地址(已 Unmap),按它建索引;无效 = 不索引
	closed atomic.Bool
	ended  bool // 已注销;归 reg.mu 管
}

// Close 关掉整条会话(幂等)。closer 由登记方给,通常是 conn.CloseWithError / rawConn.Close。
func (s *Session) Close() {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return
	}
	if s.closer != nil {
		s.closer()
	}
}

// Closed 会话是否已被关掉(或登记时登记表已关闭)。
func (s *Session) Closed() bool { return s != nil && s.closed.Load() }

// User 绑定的用户名(空 = 还没开过流)。
func (s *Session) User() string {
	if s == nil {
		return ""
	}
	s.reg.mu.Lock()
	defer s.reg.mu.Unlock()
	return s.user
}

// Registry 一个入站的登记表。
type Registry struct {
	mu        sync.Mutex
	seq       uint64
	closed    bool              // 入站已关:之后 Begin 的会话立刻关掉,Admit 一律拒
	creds     map[string]string // 名字 → 凭据指纹(库实际用于认证的那部分);键集合就是"当前有效用户"
	changedAt map[string]uint64 // 名字 → 最近一次被撤销 / 换凭据时的序号
	sessions  map[*Session]struct{}
	byUser    map[string]map[*Session]struct{}
	byAddr    map[netip.AddrPort]*Session
	hookOK    atomic.Bool // QUIC 入站:监听扩展点确实被调用了(否则登记表里永远没有会话)
}

// New 用初始用户表建登记表。creds 是 名字 → 指纹。
func New(creds map[string]string) *Registry {
	r := &Registry{
		creds:     make(map[string]string, len(creds)),
		changedAt: map[string]uint64{},
		sessions:  map[*Session]struct{}{},
		byUser:    map[string]map[*Session]struct{}{},
		byAddr:    map[netip.AddrPort]*Session{},
	}
	for k, v := range creds {
		r.creds[k] = v
	}
	return r
}

// Begin 登记一条刚建立的会话。closer 关整条会话;remote 取当前对端地址(TCP 会话传 nil)。
// 登记表已关闭(入站在关)时直接把它关掉并返回一个已关闭的会话:调用方看 Closed() 决定要不要继续。
func (r *Registry) Begin(closer func(), remote func() netip.AddrPort) *Session {
	s := &Session{reg: r, closer: closer, remote: remote}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		s.Close()
		return s
	}
	r.seq++
	s.start = r.seq
	r.sessions[s] = struct{}{}
	if remote != nil {
		if ap := normalize(remote()); ap.IsValid() {
			s.addr = ap
			r.byAddr[ap] = s
		}
	}
	r.mu.Unlock()
	return s
}

// End 会话结束:从登记表摘掉。之后再对它 Admit 一律拒(晚到的流不能把死会话插回来)。
func (r *Registry) End(s *Session) {
	if s == nil {
		return
	}
	r.mu.Lock()
	s.ended = true
	delete(r.sessions, s)
	if s.user != "" {
		if m := r.byUser[s.user]; m != nil {
			delete(m, s)
			if len(m) == 0 {
				delete(r.byUser, s.user)
			}
		}
	}
	if s.addr.IsValid() && r.byAddr[s.addr] == s {
		delete(r.byAddr, s.addr)
	}
	r.mu.Unlock()
}

// Admit 一条流带着用户名进来了:这条会话还能不能用。
//
// 拒绝只有几种明确情况:登记表已关;名字不在当前用户表里;会话已结束或已关;
// 会话是在这个名字最近一次被撤销 / 换凭据**之前**建立的(那份会话用的是旧凭据)。
// 空用户名(无鉴权的流)放行;拿不到会话(s 为 nil)只做名字检查。
// 首次绑定在锁内完成,和 Commit / CloseUsers 不会交错漏掉。
func (r *Registry) Admit(s *Session, name string) bool {
	if name == "" {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if _, ok := r.creds[name]; !ok {
		return false
	}
	if s == nil {
		return true
	}
	if s.ended || s.closed.Load() {
		return false
	}
	if s.user == "" {
		if s.start <= r.changedAt[name] {
			return false
		}
		s.user = name
		m := r.byUser[name]
		if m == nil {
			m = map[*Session]struct{}{}
			r.byUser[name] = m
		}
		m[s] = struct{}{}
	}
	return true
}

// FindByRemote 按对端地址找会话(UDP 流拿不到 QUIC 流的上下文,只能按地址认)。
// 只在"未绑定的会话"和"已绑定到这个名字的会话"里找;找不到返回 nil,调用方退回只查名字。
func (r *Registry) FindByRemote(name string, ap netip.AddrPort) *Session {
	ap = normalize(ap)
	if !ap.IsValid() {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.byAddr[ap]; s != nil && !s.ended && !s.closed.Load() && (s.user == "" || s.user == name) {
		return s
	}
	// 连接迁移之后对端地址变了:在绑定到这个名字的会话里按当前地址再找一遍
	for s := range r.byUser[name] {
		if s.remote != nil && !s.ended && !s.closed.Load() && normalize(s.remote()) == ap {
			return s
		}
	}
	return nil
}

// Prepare 换表第一步:把新名字先放进表(保留已有名字的旧指纹,留给 Commit 比对)。
// 库换表的那一瞬间新用户已经能通过 Admit,不会被误拒。
func (r *Registry) Prepare(creds map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, fp := range creds {
		if _, ok := r.creds[name]; !ok {
			r.creds[name] = fp
		}
	}
}

// Commit 换表第二步:按名字比对指纹,关掉被移除的和凭据变了的用户的会话,换上新表。
// 返回被移除、凭据变了的名字,以及关掉的会话数。表没变时三者都为空。
func (r *Registry) Commit(creds map[string]string) (removed, changed []string, closed int) {
	var victims []*Session
	r.mu.Lock()
	r.seq++
	c := r.seq
	for name, old := range r.creds {
		fp, ok := creds[name]
		switch {
		case !ok:
			removed = append(removed, name)
		case fp != old:
			changed = append(changed, name)
		default:
			continue
		}
		r.changedAt[name] = c
		for s := range r.byUser[name] {
			victims = append(victims, s)
		}
	}
	next := make(map[string]string, len(creds))
	for k, v := range creds {
		next[k] = v
	}
	r.creds = next
	r.pruneLocked()
	r.mu.Unlock()
	for _, s := range victims {
		s.Close()
	}
	return removed, changed, len(victims)
}

// CloseUsers 踢线:关掉这些名字已绑定的会话,并打上撤销点(撤销前建立、还没开流的会话之后也进不来)。
// 名字精确匹配:踢本人传 [X, X#share],只踢共享传 [X#share]。返回关掉的会话数。
func (r *Registry) CloseUsers(names []string) int {
	var victims []*Session
	r.mu.Lock()
	r.seq++
	c := r.seq
	for _, name := range names {
		if name == "" {
			continue
		}
		_, known := r.creds[name]
		if !known && len(r.byUser[name]) == 0 {
			continue
		}
		r.changedAt[name] = c
		for s := range r.byUser[name] {
			victims = append(victims, s)
		}
	}
	r.pruneLocked()
	r.mu.Unlock()
	for _, s := range victims {
		s.Close()
	}
	return len(victims)
}

// CloseAll 入站关闭(anytls):关掉全部会话,之后登记的会话立刻被关。返回关掉的会话数。
func (r *Registry) CloseAll() int {
	var victims []*Session
	r.mu.Lock()
	r.closed = true
	for s := range r.sessions {
		victims = append(victims, s)
	}
	r.mu.Unlock()
	for _, s := range victims {
		s.Close()
	}
	return len(victims)
}

// Clear 入站关闭(hy2 / TUIC):QUIC 连接由传输层自己销毁,这里只把表清空并关门。
func (r *Registry) Clear() {
	r.mu.Lock()
	r.closed = true
	r.sessions = map[*Session]struct{}{}
	r.byUser = map[string]map[*Session]struct{}{}
	r.byAddr = map[netip.AddrPort]*Session{}
	r.mu.Unlock()
}

// Len 当前登记的会话数(含还没绑定用户的)。
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Bound 某个名字已绑定的会话数。
func (r *Registry) Bound(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byUser[name])
}

// HookOK QUIC 入站的监听扩展点是否真的被调用了(TCP 入站恒为假,不适用)。
func (r *Registry) HookOK() bool { return r.hookOK.Load() }

// pruneLocked 修剪撤销点:比所有存活会话都早的撤销点没人会再撞上,删掉,免得无限增长。
func (r *Registry) pruneLocked() {
	if len(r.changedAt) == 0 {
		return
	}
	var oldest uint64
	for s := range r.sessions {
		if oldest == 0 || s.start < oldest {
			oldest = s.start
		}
	}
	for name, at := range r.changedAt {
		if oldest == 0 || at < oldest {
			delete(r.changedAt, name)
		}
	}
}

// normalize 双栈监听(::)下 IPv4 客户端的地址是 ::ffff:a.b.c.d,和 sing-box 交给我们的 Unwrap 过的地址对不上;统一去掉映射。
func normalize(ap netip.AddrPort) netip.AddrPort {
	if !ap.IsValid() {
		return ap
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}
