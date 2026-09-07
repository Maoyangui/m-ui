package core

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/time/rate"
)

// Limiter 提供两层策略,均在数据面本机执行:
//
// 用户层(per-user)
//   - 限速:每用户一对令牌桶(上行/下行),该用户所有连接共享总带宽
//   - 设备数:限制**同时在线**的源 IP 数量
//
// 组层(代理池):代理名下所有用户共用一个池
//   - 带宽池:每组一对令牌桶,组内所有用户的所有连接再共享一次;每台服务器各一份(带宽是单机物理量)
//   - 设备池:组内所有用户同时在线的不同源 IP 总数;跨机按 Hub 下发的外部 IP 并集判定
//     一条连接要同时过用户层和组层。代理给单个用户填的限制只是该用户自己的上限,不再要求"之和不超过代理额度"。
//
// 设备数是并发语义,不是"锁定前 N 个 IP":某个 IP 只要在 idleWindow 内没有任何
// 流量就被视为下线并释放名额,新设备立刻可以顶上;有流量经过时持续刷新其活跃时间,
// 所以长连接(下载/看视频)的设备不会被误判下线。池满时只拒绝这一次新设备,已连接的不受影响。
//
// 跨多机的设备数并集判定在 Hub 侧完成(P4);本类型负责单机拦截与"当前在线 IP"上报。
type Limiter struct {
	mu      sync.Mutex
	limits  map[string]userLimit
	entries map[string]*userEntry // 用户桶位:连接持有的是条目而不是桶,改速率原地生效,在线连接立刻变速
	ips     map[string]map[string]int64
	// external 是其他机器上该用户当前在线的源 IP(Hub 下发),计入设备数;本机已在线的 IP 不重复计
	external   map[string]map[string]bool
	idleWindow time.Duration // 无流量多久判定该 IP 下线并释放名额

	// 组层
	userGroup  map[string]string   // 用户 → 组(如 r12)
	groupUsers map[string][]string // 组 → 用户
	groups     map[string]groupLimit
	gentries   map[string]*groupEntry
	grejects   map[string]int64 // 设备池满被拒的次数(GroupState 取走即清零)
}

// SetExternalIPs 全量替换"其他机器上在线的 IP"(跨机设备数并集判定)。
func (l *Limiter) SetExternalIPs(m map[string][]string) {
	ext := make(map[string]map[string]bool, len(m))
	for user, ips := range m {
		set := make(map[string]bool, len(ips))
		for _, ip := range ips {
			set[ip] = true
		}
		ext[user] = set
	}
	l.mu.Lock()
	l.external = ext
	l.mu.Unlock()
}

type userLimit struct {
	upBps       int64 // 字节/秒,0=不限
	downBps     int64
	deviceLimit int // 0=不限
}

type groupLimit struct {
	upBps, downBps int64
	deviceLimit    int
}

func NewLimiter() *Limiter {
	return &Limiter{
		limits:     map[string]userLimit{},
		entries:    map[string]*userEntry{},
		ips:        map[string]map[string]int64{},
		idleWindow: 60 * time.Second,
		userGroup:  map[string]string{},
		groupUsers: map[string][]string{},
		groups:     map[string]groupLimit{},
		gentries:   map[string]*groupEntry{},
		grejects:   map[string]int64{},
	}
}

// SetLimits 全量替换用户策略(来自渲染时的用户表)。mbps 为 0 表示不限;Group 非空表示该用户属于某个代理池。
func (l *Limiter) SetLimits(limits map[string]UserLimitSpec) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits = map[string]userLimit{}
	l.userGroup = map[string]string{}
	l.groupUsers = map[string][]string{}
	for name, s := range limits {
		lim := userLimit{
			upBps:       int64(s.UpMbps) * 125000, // Mbps → 字节/秒
			downBps:     int64(s.DownMbps) * 125000,
			deviceLimit: s.DeviceLimit,
		}
		l.limits[name] = lim
		if s.Group != "" {
			l.userGroup[name] = s.Group
			l.groupUsers[s.Group] = append(l.groupUsers[s.Group], name)
		}
		// 桶原地改速率:参数没变的桶不动(热更新一次不会回到满桶),变了的连同在线连接一起立刻变速
		e := l.entryLocked(name)
		setRate(&e.up, lim.upBps)
		setRate(&e.down, lim.downBps)
	}
	// 不在策略里的用户 = 不限速:桶置空,持有条目的在线连接随之放开
	for name, e := range l.entries {
		if _, ok := limits[name]; !ok {
			e.up.Store(nil)
			e.down.Store(nil)
		}
	}
}

// UserLimitSpec 是 SetLimits 的入参(与 model.User 解耦,便于数据面独立测试)。
type UserLimitSpec struct {
	UpMbps      int
	DownMbps    int
	DeviceLimit int
	Group       string // 所属代理池,空=不属于任何池
}

// GroupLimitSpec 是 SetGroups 的入参:一个代理池的上限,0=不限。
type GroupLimitSpec struct {
	UpMbps      int
	DownMbps    int
	DeviceLimit int
}

// SetGroups 全量替换代理池上限。
func (l *Limiter) SetGroups(groups map[string]GroupLimitSpec) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.groups = map[string]groupLimit{}
	for g, s := range groups {
		gl := groupLimit{upBps: int64(s.UpMbps) * 125000, downBps: int64(s.DownMbps) * 125000, deviceLimit: s.DeviceLimit}
		l.groups[g] = gl
		e := l.groupEntryLocked(g)
		setRate(&e.up, gl.upBps)
		setRate(&e.down, gl.downBps)
	}
	for g, e := range l.gentries {
		if _, ok := groups[g]; !ok {
			e.up.Store(nil)
			e.down.Store(nil)
		}
	}
}

// GroupState 是一个代理池当前的状态:本机在线设备数,以及自上次取走以来设备池满被拒的次数。
type GroupState struct {
	Devices int
	Rejects int64
}

// GroupState 返回各代理池的状态并清零拒绝计数(Hub 每轮取走上报给主机)。
func (l *Limiter) GroupState() map[string]GroupState {
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]GroupState{}
	for g := range l.groups {
		set := map[string]bool{}
		for _, u := range l.groupUsers[g] {
			l.pruneLocked(u, now)
			for ip := range l.ips[u] {
				set[ip] = true
			}
		}
		out[g] = GroupState{Devices: len(set), Rejects: l.grejects[g]}
		l.grejects[g] = 0
	}
	return out
}

// AllowConn 判定某用户从某源 IP 的新连接是否放行,并登记该 IP 的活跃时间。
// 空用户名(无鉴权连接)不受限。
//
// 判定顺序体现"同时在线"语义:先清掉已空闲的 IP 释放名额,已在线的 IP 直接放行,
// 只要当前在线数没到上限就接纳新 IP;超限只拒绝这一次,不对该 IP 留任何黑名单,
// 所以别的设备一下线,它下次重连即可进入。用户自己的上限和所属代理池的上限都要过。
func (l *Limiter) AllowConn(user, ip string) bool {
	if user == "" || ip == "" {
		return true
	}
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := l.limits[user].deviceLimit
	l.pruneLocked(user, now)
	active := l.ips[user]
	if active == nil {
		active = map[string]int64{}
		l.ips[user] = active
	}
	if _, online := active[ip]; online {
		active[ip] = now
		return true
	}
	ext := l.external[user]
	if limit > 0 && !ext[ip] { // 该设备已在别的机器上在线时,视为同一设备切换入口,不占新名额
		total := len(active)
		for eip := range ext {
			if _, dup := active[eip]; !dup {
				total++
			}
		}
		if total >= limit {
			return false
		}
	}
	if g := l.userGroup[user]; g != "" {
		if gl := l.groups[g]; gl.deviceLimit > 0 && !l.groupHasRoomLocked(g, ip, now, gl.deviceLimit) {
			l.grejects[g]++
			return false
		}
	}
	active[ip] = now
	return true
}

// groupHasRoomLocked 代理池里(本机 + 其他机器)同时在线的不同 IP 是否还没到上限;ip 已在池里则不占新名额。
func (l *Limiter) groupHasRoomLocked(g, ip string, now int64, limit int) bool {
	set := map[string]bool{}
	for _, u := range l.groupUsers[g] {
		l.pruneLocked(u, now)
		for a := range l.ips[u] {
			set[a] = true
		}
		for e := range l.external[u] {
			set[e] = true
		}
	}
	if set[ip] {
		return true
	}
	return len(set) < limit
}

// pruneLocked 清掉空闲超时的活跃 IP(调用方须持锁)。
func (l *Limiter) pruneLocked(user string, now int64) {
	active := l.ips[user]
	cutoff := now - int64(l.idleWindow.Seconds())
	for ip, seen := range active {
		if seen < cutoff {
			delete(active, ip)
		}
	}
}

// ActiveIPs 返回某用户当前活跃的源 IP(供面板展示与 Hub 聚合)。
func (l *Limiter) ActiveIPs(user string) []string {
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(user, now)
	ips := make([]string, 0, len(l.ips[user]))
	for ip := range l.ips[user] {
		ips = append(ips, ip)
	}
	return ips
}

// ActiveIPsAll 一次拿全量:用户 → 活跃源 IP。
// 面板过去是每个用户调一次 ActiveIPs,几百个用户就要抢几百次锁,
// 而数据面每有流量都要 touch 同一把锁——用户一多两边都卡。
func (l *Limiter) ActiveIPsAll() map[string][]string {
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string][]string, len(l.ips))
	for user := range l.ips {
		l.pruneLocked(user, now)
		if len(l.ips[user]) == 0 {
			continue
		}
		ips := make([]string, 0, len(l.ips[user]))
		for ip := range l.ips[user] {
			ips = append(ips, ip)
		}
		out[user] = ips
	}
	return out
}

// touch 刷新某用户某 IP 的活跃时间(有流量经过时调用)。
func (l *Limiter) touch(user, ip string, now int64) {
	if user == "" || ip == "" {
		return
	}
	l.mu.Lock()
	if l.ips[user] == nil {
		l.ips[user] = map[string]int64{}
	}
	l.ips[user][ip] = now
	l.mu.Unlock()
}

// userEntry 一个用户的桶位:连接建立时拿走这个条目,之后改限速只改条目里的桶(原地改速率或置空),
// 在线连接不用重连就变速。桶为 nil = 该方向不限。
type userEntry struct{ up, down atomic.Pointer[rate.Limiter] }

// groupEntry 代理池的桶位,同上。
type groupEntry struct{ up, down atomic.Pointer[rate.Limiter] }

// setRate 把桶位调到 bps:0 置空;没桶就建;有桶就原地改速率(桶不换,不会回到满桶)。
// 突发上限设为 1 秒带宽,兼顾峰值与平滑。
func setRate(p *atomic.Pointer[rate.Limiter], bps int64) {
	if bps <= 0 {
		p.Store(nil)
		return
	}
	if b := p.Load(); b != nil {
		if b.Limit() != rate.Limit(bps) || b.Burst() != int(bps) {
			b.SetLimit(rate.Limit(bps))
			b.SetBurst(int(bps))
		}
		return
	}
	p.Store(rate.NewLimiter(rate.Limit(bps), int(bps)))
}

func (l *Limiter) entryLocked(user string) *userEntry {
	e := l.entries[user]
	if e == nil {
		e = &userEntry{}
		l.entries[user] = e
	}
	return e
}

func (l *Limiter) groupEntryLocked(g string) *groupEntry {
	e := l.gentries[g]
	if e == nil {
		e = &groupEntry{}
		l.gentries[g] = e
	}
	return e
}

// wrapConn 给一条连接挂上用户与所属代理池的桶位,并在有流量时刷新该设备的在线状态。
// 每条认证过的连接都包装:现在不限速的用户,之后被规则限速时这条连接也要立刻变慢。
func (l *Limiter) wrapConn(conn net.Conn, user, ip string) net.Conn {
	if user == "" {
		return conn
	}
	l.mu.Lock()
	e := l.entryLocked(user)
	var g *groupEntry
	if name := l.userGroup[user]; name != "" {
		g = l.groupEntryLocked(name)
	}
	l.mu.Unlock()
	return &limitedConn{Conn: conn, user: e, group: g, keepalive: l.keepaliveFor(user, ip)}
}

func (l *Limiter) wrapPacketConn(conn N.PacketConn, user, ip string) N.PacketConn {
	if user == "" {
		return conn
	}
	l.mu.Lock()
	e := l.entryLocked(user)
	var g *groupEntry
	if name := l.userGroup[user]; name != "" {
		g = l.groupEntryLocked(name)
	}
	l.mu.Unlock()
	return &limitedPacketConn{PacketConn: conn, user: e, group: g, keepalive: l.keepaliveFor(user, ip)}
}

// keepaliveFor 返回一个"该设备刚有流量"的回调,按秒节流以免每次读写都抢锁。
func (l *Limiter) keepaliveFor(user, ip string) func() {
	var last int64
	return func() {
		now := time.Now().Unix()
		if now == atomic.LoadInt64(&last) {
			return
		}
		atomic.StoreInt64(&last, now)
		l.touch(user, ip, now)
	}
}

// throttle 按令牌桶为 n 字节配速,burst 上限内分块等待,不因单次读写过大而报错。
func throttle(b *rate.Limiter, n int) {
	if b == nil || n <= 0 {
		return
	}
	burst := b.Burst()
	if burst <= 0 {
		return
	}
	for n > 0 {
		chunk := n
		if chunk > burst {
			chunk = burst
		}
		_ = b.WaitN(context.Background(), chunk)
		n -= chunk
	}
}

type limitedConn struct {
	net.Conn
	user      *userEntry  // 用户桶位
	group     *groupEntry // 代理池桶位(不在池里为 nil)
	keepalive func()
}

func (c *limitedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.keepalive()
	}
	throttle(c.user.up.Load(), n)
	if c.group != nil {
		throttle(c.group.up.Load(), n)
	}
	return n, err
}

func (c *limitedConn) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.keepalive()
	}
	throttle(c.user.down.Load(), len(p))
	if c.group != nil {
		throttle(c.group.down.Load(), len(p))
	}
	return c.Conn.Write(p)
}

func (c *limitedConn) Upstream() any { return c.Conn }

type limitedPacketConn struct {
	N.PacketConn
	user      *userEntry
	group     *groupEntry
	keepalive func()
}

func (c *limitedPacketConn) ReadPacket(b *buf.Buffer) (M.Socksaddr, error) {
	dest, err := c.PacketConn.ReadPacket(b)
	if b.Len() > 0 {
		c.keepalive()
	}
	throttle(c.user.up.Load(), b.Len())
	if c.group != nil {
		throttle(c.group.up.Load(), b.Len())
	}
	return dest, err
}

func (c *limitedPacketConn) WritePacket(b *buf.Buffer, dest M.Socksaddr) error {
	if b.Len() > 0 {
		c.keepalive()
	}
	throttle(c.user.down.Load(), b.Len())
	if c.group != nil {
		throttle(c.group.down.Load(), b.Len())
	}
	return c.PacketConn.WritePacket(b, dest)
}

func (c *limitedPacketConn) Upstream() any { return c.PacketConn }
