package core

import (
	"net"
	"net/netip"
	"sort"
	"time"

	N "github.com/sagernet/sing/common/network"
)

// 节点层:节点自己的总限速、每用户限速、用户容量(model.NodeOpt;节点表见 nodes.go)。
// 和用户层、代理池层叠加:一条连接的每次读写依次过各层的桶,谁严听谁的。只管认证过的用户连接。
//
//   - 节点总限速:这个节点一对令牌桶(上行 / 下行各一个,同一个速率),走这个节点的所有用户共用;
//   - 每用户限速:(节点, 用户)一对令牌桶,这个用户在这个节点上的所有连接共用;
//   - 用户容量:这个节点上同时在线的不同用户数。和设备数一样是并发语义:idleWindow 内没有流量就算下线、让出名额;
//     满了只拒新来的用户,已经在线的不踢(调小容量也不踢)。
//
// 改设置对在线连接立刻生效(桶位原地改速率);一个节点从「什么都没设」变成有设置时,之前建好的连接没挂这一层的桶,
// 要等它们重连。

// RejectNode 拒绝原因:这个节点的用户容量满了。
const RejectNode = "node"

type nodeState struct {
	spec  NodeSpec
	total userEntry             // 节点总限速的桶位
	users map[string]*userEntry // 用户 → 在这个节点上的桶位
	seen  map[string]int64      // 用户 → 最近一次有流量的时间(容量按它数)
}

func mbps(n int) int64 { return int64(n) * 125000 } // Mbps → 字节/秒

func (st *nodeState) apply(spec NodeSpec) {
	st.spec = spec
	setRate(&st.total.up, mbps(spec.Speed))
	setRate(&st.total.down, mbps(spec.Speed))
	for _, e := range st.users {
		setRate(&e.up, mbps(spec.UserSpeed))
		setRate(&e.down, mbps(spec.UserSpeed))
	}
}

// SetNodes 换上新的节点表(渲染配置时算好的);nil / 空 = 没有节点单独设东西。
func (l *Limiter) SetNodes(t NodeTable) {
	specs := map[string]NodeSpec{}
	for _, in := range t {
		if in.Direct.Key != "" {
			specs[in.Direct.Key] = in.Direct
		}
		for _, s := range in.Sources {
			if s.Key != "" {
				specs[s.Key] = s
			}
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(t) == 0 {
		l.nodeTab.Store(nil)
	} else {
		l.nodeTab.Store(&t)
	}
	for key, st := range l.nstates {
		spec, ok := specs[key]
		if !ok {
			// 这个节点不再有单独的设置:桶全部置空(挂着它的在线连接随之放开);没人在线的状态顺手清掉
			st.apply(NodeSpec{})
			if len(st.seen) == 0 {
				delete(l.nstates, key)
			}
			continue
		}
		st.apply(spec)
	}
}

// NodeFor 这条连接是从哪个节点进来的(见 NodeTable.Lookup);没有表或这个节点什么都没设返回空的。
func (l *Limiter) NodeFor(inbound string, src netip.Addr) NodeSpec {
	t := l.nodeTab.Load()
	if t == nil {
		return NodeSpec{}
	}
	return t.Lookup(inbound, src)
}

func (l *Limiter) nodeStateLocked(spec NodeSpec) *nodeState {
	st := l.nstates[spec.Key]
	if st == nil {
		st = &nodeState{users: map[string]*userEntry{}, seen: map[string]int64{}}
		st.apply(spec)
		l.nstates[spec.Key] = st
	}
	return st
}

// nodePruneLocked 清掉这个节点上空闲超时的用户(连同他们的桶位:再来时新建)。
func (l *Limiter) nodePruneLocked(st *nodeState, now int64) {
	cutoff := now - int64(l.idleWindow.Seconds())
	for u, at := range st.seen {
		if at < cutoff {
			delete(st.seen, u)
			delete(st.users, u)
		}
	}
}

// nodeTouchLocked 记下这个用户刚在这个节点上有流量(或刚连上)。节点已经没有状态(设置撤掉了)就不记。
func (l *Limiter) nodeTouchLocked(key, user string, now int64) {
	if st := l.nstates[key]; st != nil {
		st.seen[user] = now
	}
}

// AllowConnNode 同 AllowConnWhy,多过一层节点的用户容量。先看容量再登记设备:被容量拒掉的连接不该占一个设备名额。
func (l *Limiter) AllowConnNode(user, ip string, spec NodeSpec) (bool, string) {
	if user == "" || ip == "" {
		return true, ""
	}
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	var st *nodeState
	if spec.Key != "" {
		st = l.nodeStateLocked(spec)
		if spec.Cap > 0 {
			l.nodePruneLocked(st, now)
			if _, online := st.seen[user]; !online && len(st.seen) >= spec.Cap {
				return false, RejectNode
			}
		}
	}
	if why := l.admitLocked(user, ip, now); why != "" {
		return false, why
	}
	if st != nil {
		st.seen[user] = now
	}
	return true, ""
}

// nodeEntriesLocked 这条连接要挂的节点层桶位:节点总限速的、这个用户在这个节点上的。节点什么都没设返回 nil。
func (l *Limiter) nodeEntriesLocked(user string, spec NodeSpec) (total, mine *userEntry) {
	if spec.Key == "" {
		return nil, nil
	}
	st := l.nodeStateLocked(spec)
	e := st.users[user]
	if e == nil {
		e = &userEntry{}
		setRate(&e.up, mbps(st.spec.UserSpeed))
		setRate(&e.down, mbps(st.spec.UserSpeed))
		st.users[user] = e
	}
	return &st.total, e
}

// wrapConnNode 同 wrapConn,多挂节点层的桶位;line 是这条连接走的线路(入站 tag,规则忽略了这条线路时换桶位用,可为空)。
func (l *Limiter) wrapConnNode(conn net.Conn, user, ip, line string, spec NodeSpec) net.Conn {
	if user == "" {
		return conn
	}
	l.mu.Lock()
	e := l.entryLocked(user)
	var g *groupEntry
	if name := l.userGroup[user]; name != "" {
		g = l.groupEntryLocked(name)
	}
	total, mine := l.nodeEntriesLocked(user, spec)
	l.mu.Unlock()
	return &limitedConn{Conn: conn, limiter: l, name: user, line: line, user: e, group: g, node: total, nodeUser: mine, keepalive: l.keepaliveFor(user, ip, spec.Key)}
}

func (l *Limiter) wrapPacketConnNode(conn N.PacketConn, user, ip, line string, spec NodeSpec) N.PacketConn {
	if user == "" {
		return conn
	}
	l.mu.Lock()
	e := l.entryLocked(user)
	var g *groupEntry
	if name := l.userGroup[user]; name != "" {
		g = l.groupEntryLocked(name)
	}
	total, mine := l.nodeEntriesLocked(user, spec)
	l.mu.Unlock()
	return &limitedPacketConn{PacketConn: conn, limiter: l, name: user, line: line, user: e, group: g, node: total, nodeUser: mine, keepalive: l.keepaliveFor(user, ip, spec.Key)}
}

// NodeUsers 各节点此刻在线的用户(按节点键),给面板显示「在线 / 容量」用;只有单独设了东西的节点才有。
func (l *Limiter) NodeUsers() map[string][]string {
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string][]string{}
	for key, st := range l.nstates {
		l.nodePruneLocked(st, now)
		if len(st.seen) == 0 {
			continue
		}
		us := make([]string, 0, len(st.seen))
		for u := range st.seen {
			us = append(us, u)
		}
		sort.Strings(us)
		out[key] = us
	}
	return out
}
