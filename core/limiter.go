package core

import (
	"context"
	"net"
	"sort"
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
	external map[string]map[string]bool
	// externalAt 上一次收到外部 IP 的时间。主机失联(副机)/ 副机失联(主机)时这份表会一直冻结,
	// 陈旧的 IP 不能再占名额,更不能拿它去断掉回来的老连接:超过 externalGrace 没刷新就当没有。
	externalAt int64
	// reconciledRejects 是本轮跨机并集实际超额的设备；外部租约过期时一并失效。
	reconciledRejects map[string]map[string]bool
	deviceGen         atomic.Uint64
	idleWindow        time.Duration // 无流量多久判定该 IP 下线并释放名额
	// gen 策略代数:SetLimits 每次加一。在线连接靠它判断"用户所属的代理池有没有变",没变就不抢锁。
	gen atomic.Uint64

	// 组层
	userGroup  map[string]string   // 用户 → 组(如 r12)
	groupUsers map[string][]string // 组 → 用户
	groups     map[string]groupLimit
	gentries   map[string]*groupEntry
	grejects   map[string]int64 // 设备池满被拒的次数(GroupState 取走即清零)
}

// SetExternalIPs 全量替换"其他机器上在线的 IP"(跨机设备数并集判定)。
func (l *Limiter) SetExternalIPs(m map[string][]string) map[string][]string {
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
	l.externalAt = time.Now().Unix()
	victims := l.reconcileDevicesLocked(l.externalAt)
	l.deviceGen.Add(1)
	l.mu.Unlock()
	return victims
}

// ReconcileDevices 在用户/代理设备策略热更新后重新收敛当前在线设备。
// 返回值只包含本机需要断开的实际超额设备；调用方负责关闭对应连接。
func (l *Limiter) ReconcileDevices() map[string][]string {
	l.mu.Lock()
	victims := l.reconcileDevicesLocked(time.Now().Unix())
	l.deviceGen.Add(1)
	l.mu.Unlock()
	return victims
}

// reconcileDevicesLocked 并集(本机在线 + 副机上报)超过上限时,算出**哪些 IP 不再接受新登记**。
//
// 它**不会**断开任何已经连着的设备,返回值永远是空的。你定过的语义是"设备池跨机并集只拒新设备,
// 已连接的不动"(0.6.9 就是这么做的)。0.6.10 把这里改成按 IP 字典序踢掉在线设备:一个连了几小时、
// 完全合规的付费用户会因为 IP 排得靠后被断网,而且之后池已被别人占满还连不回来;副机失联恢复
// 那一轮同样可能踢掉健康主机上的老设备、留下失联期间在副机上新加的(踢谁只看 IP 排序)。
//
// 这里的做法:本机正在连着的设备一律保留(它们登记时是合规的,超限后靠空闲窗口自然释放);
// 剩余名额留给副机上报的 IP,按固定顺序取,取不到名额的那些在本机拒绝新登记。
// 各机以自己的在线集合为基准,算出的拒绝名单**不一定相同**(A 机保留自己的两台,B 机保留自己的一台),
// 但谁都不踢、谁都不放新 IP,并集不会再涨 —— 副机失联恢复后可能短暂超过上限(最坏各机上限之和),
// 靠 60 秒空闲窗口收敛。别为了"各机一致"去踢在线设备:0.6.10 就是这么走偏的。
func (l *Limiter) reconcileDevicesLocked(now int64) map[string][]string {
	all := make(map[string]map[string]bool, len(l.limits))
	local := make(map[string]map[string]bool, len(l.limits))
	l.reconciledRejects = map[string]map[string]bool{}
	for user := range l.limits {
		l.pruneLocked(user, now)
		set := map[string]bool{}
		mine := map[string]bool{}
		for ip := range l.ips[user] {
			set[ip] = true
			mine[ip] = true
		}
		for ip := range l.externalLocked(user, now) {
			set[ip] = true
		}
		all[user], local[user] = set, mine
		if limit := l.limits[user].deviceLimit; limit > 0 {
			for _, ip := range excessExternal(set, mine, limit) {
				l.rejectDeviceLocked(user, ip)
				delete(set, ip)
			}
		}
	}
	for group, lim := range l.groups {
		if lim.deviceLimit <= 0 {
			continue
		}
		set := map[string]bool{}
		mine := map[string]bool{}
		for _, user := range l.groupUsers[group] {
			for ip := range all[user] {
				set[ip] = true
			}
			for ip := range local[user] {
				mine[ip] = true
			}
		}
		for _, ip := range excessExternal(set, mine, lim.deviceLimit) {
			for _, user := range l.groupUsers[group] {
				if all[user][ip] && !local[user][ip] {
					l.rejectDeviceLocked(user, ip)
				}
			}
		}
	}
	return map[string][]string{} // 永远不踢已连接的设备
}

// excessExternal 在"本机在线的一律保留"之后,并集里还放不下的那些外部 IP(按固定顺序取,各机算出来一样)。
func excessExternal(set, local map[string]bool, limit int) []string {
	if len(set) <= limit {
		return nil
	}
	room := limit - len(local)
	ext := make([]string, 0, len(set))
	for ip := range set {
		if !local[ip] {
			ext = append(ext, ip)
		}
	}
	sort.Strings(ext)
	if room <= 0 {
		return ext
	}
	if room >= len(ext) {
		return nil
	}
	return ext[room:]
}

func excessDevices(set map[string]bool, limit int) []string {
	if len(set) <= limit {
		return nil
	}
	ips := make([]string, 0, len(set))
	for ip := range set {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	return ips[limit:]
}

func (l *Limiter) rejectDeviceLocked(user, ip string) {
	if l.reconciledRejects[user] == nil {
		l.reconciledRejects[user] = map[string]bool{}
	}
	l.reconciledRejects[user][ip] = true
}

func (l *Limiter) deviceRejectedLocked(user, ip string, now int64) bool {
	return now-l.externalAt <= externalGrace && l.reconciledRejects[user][ip]
}

// externalGrace 外部 IP 多久没刷新就作废(秒)。主机每 5 秒下发一次;这里给足几分钟,短暂抖动不误伤,
// 真失联了也不会拿几分钟前的 IP 去拒新设备、断老连接。
const externalGrace = 300

// externalLocked 某用户此刻有效的外部 IP(过期了就当没有)。调用方须持锁。
func (l *Limiter) externalLocked(user string, now int64) map[string]bool {
	if now-l.externalAt > externalGrace {
		return nil
	}
	return l.external[user]
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
	l.gen.Add(1)
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
	return l.allowConnLocked(user, ip, now)
}

// allowConnLocked applies the device limits for a connection or for a device
// that became active again after the idle window. The caller must hold l.mu.
func (l *Limiter) allowConnLocked(user, ip string, now int64) bool {
	if l.deviceRejectedLocked(user, ip, now) {
		return false
	}
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
	ext := l.externalLocked(user, now)
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
		for e := range l.externalLocked(u, now) {
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

// Forget 踢线后立刻清掉该用户的在线 IP 记录:连接都断了,不用再等空闲窗口才从"在线设备"里消失。
// 设备重连会重新登记;别的机器上的 IP 由下一轮同步刷新。只清本机的记账,策略与外部 IP 不动。
func (l *Limiter) Forget(user string) {
	if user == "" {
		return
	}
	l.mu.Lock()
	delete(l.ips, user)
	l.mu.Unlock()
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
func (l *Limiter) touch(user, ip string, now int64) bool {
	if user == "" || ip == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.deviceRejectedLocked(user, ip, now) {
		return false
	}
	active := l.ips[user]
	if active != nil {
		l.pruneLocked(user, now)
		// Keep the fast path for an already-active device. A connection that
		// resumes after the idle window must go through allowConnLocked below,
		// otherwise it could bypass a newly occupied device pool.
		if _, ok := active[ip]; ok {
			active[ip] = now
			return true
		}
	}
	return l.allowConnLocked(user, ip, now)
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
	return &limitedConn{Conn: conn, limiter: l, name: user, user: e, group: g, keepalive: l.keepaliveFor(user, ip)}
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
	return &limitedPacketConn{PacketConn: conn, limiter: l, name: user, user: e, group: g, keepalive: l.keepaliveFor(user, ip)}
}

// keepaliveFor 返回一个"该设备刚有流量"的回调,按秒节流以免每次读写都抢锁。
func (l *Limiter) keepaliveFor(user, ip string) func() bool {
	var last, generation atomic.Int64
	return func() bool {
		now := time.Now().Unix()
		gen := int64(l.deviceGen.Load())
		if now == last.Load() && gen == generation.Load() {
			return true
		}
		if !l.touch(user, ip, now) {
			return false
		}
		generation.Store(gen)
		last.Store(now)
		return true
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

// errDeviceRecheck 空闲后回来的设备被设备上限拒掉:这条连接关掉。识别为"连接已关"(net.ErrClosed),
// 拷贝循环只当普通断开处理,不往有界日志队列里刷 ERROR。
type deviceRecheckError struct{}

func (deviceRecheckError) Error() string        { return "device limit: connection closed" }
func (deviceRecheckError) Is(target error) bool { return target == net.ErrClosed }

var errDeviceRecheck error = deviceRecheckError{}

type limitedConn struct {
	net.Conn
	limiter   *Limiter
	name      string
	user      *userEntry  // 用户桶位
	group     *groupEntry // 建连时的代理池桶位(不在池里为 nil);运行期以 currentGroup 为准
	keepalive func() bool
	gen       atomic.Uint64              // 上次解析代理池时的策略代数
	groupNow  atomic.Pointer[groupEntry] // 按代数缓存的当前代理池
	denied    atomic.Bool                // 已被设备上限拒掉:之后所有读写立即失败,不会下一秒又放行
}

// currentGroup 这条连接此刻所属的代理池。用户被挪到别的代理、或移出代理之后,已有连接要跟着走,
// 所以不能只用建连时抓的指针。但也不能每次读写都抢 l.mu —— 那把锁是数据面的热点,
// keepalive 按秒节流就是为了躲它。按策略代数缓存:代数没变(绝大多数读写)一把锁都不碰。
func (c *limitedConn) currentGroup() *groupEntry {
	if c.limiter == nil {
		return c.group
	}
	return resolveGroup(c.limiter, c.name, &c.gen, &c.groupNow)
}

func resolveGroup(l *Limiter, name string, gen *atomic.Uint64, cache *atomic.Pointer[groupEntry]) *groupEntry {
	g := l.gen.Load()
	if gen.Load() == g {
		return cache.Load()
	}
	l.mu.Lock()
	var ng *groupEntry
	if gname := l.userGroup[name]; gname != "" {
		ng = l.groupEntryLocked(gname)
	}
	l.mu.Unlock()
	cache.Store(ng)
	gen.Store(g)
	return ng
}

func (c *limitedConn) Read(p []byte) (int, error) {
	if c.denied.Load() {
		return 0, errDeviceRecheck
	}
	n, err := c.Conn.Read(p)
	if n > 0 && !c.keepalive() {
		// 空闲后回来、名额已被占:这条连接到此为止,读到的字节丢掉(对端是被拒的设备,不该再收到任何东西)
		c.denied.Store(true)
		_ = c.Conn.Close()
		return 0, errDeviceRecheck
	}
	throttle(c.user.up.Load(), n)
	if g := c.currentGroup(); g != nil {
		throttle(g.up.Load(), n)
	}
	return n, err
}

func (c *limitedConn) Write(p []byte) (int, error) {
	if c.denied.Load() || (len(p) > 0 && !c.keepalive()) {
		c.denied.Store(true)
		_ = c.Conn.Close()
		return 0, errDeviceRecheck
	}
	throttle(c.user.down.Load(), len(p))
	if g := c.currentGroup(); g != nil {
		throttle(g.down.Load(), len(p))
	}
	if c.denied.Load() {
		return 0, errDeviceRecheck
	}
	return c.Conn.Write(p)
}

func (c *limitedConn) Upstream() any { return c.Conn }

type limitedPacketConn struct {
	N.PacketConn
	limiter   *Limiter
	name      string
	user      *userEntry
	group     *groupEntry
	keepalive func() bool
	gen       atomic.Uint64
	groupNow  atomic.Pointer[groupEntry]
	denied    atomic.Bool
}

func (c *limitedPacketConn) currentGroup() *groupEntry {
	if c.limiter == nil {
		return c.group
	}
	return resolveGroup(c.limiter, c.name, &c.gen, &c.groupNow)
}

// ReadPacket 被设备上限拒掉的 UDP 流走黑洞:包吞掉、连接不关。关掉的话 hysteria2 / tuic 的服务端会为下一个数据报
// 重建一条 UDP 会话、再起一个 goroutine、再被拒一次 —— 游戏 / 语音每秒几百个包就是几百次。留着它,客户端停发后由 UDP 超时回收。
func (c *limitedPacketConn) ReadPacket(b *buf.Buffer) (M.Socksaddr, error) {
	for {
		dest, err := c.PacketConn.ReadPacket(b)
		if err != nil {
			return dest, err
		}
		if c.denied.Load() {
			b.Reset()
			continue
		}
		if b.Len() > 0 && !c.keepalive() {
			c.denied.Store(true)
			b.Reset()
			continue
		}
		throttle(c.user.up.Load(), b.Len())
		if g := c.currentGroup(); g != nil {
			throttle(g.up.Load(), b.Len())
		}
		return dest, nil
	}
}

func (c *limitedPacketConn) WritePacket(b *buf.Buffer, dest M.Socksaddr) error {
	if c.denied.Load() || (b.Len() > 0 && !c.keepalive()) {
		c.denied.Store(true)
		b.Release() // 这里要接管缓冲区
		return nil  // 黑洞:吞掉,不报错、不关连接
	}
	throttle(c.user.down.Load(), b.Len())
	if g := c.currentGroup(); g != nil {
		throttle(g.down.Load(), b.Len())
	}
	return c.PacketConn.WritePacket(b, dest)
}

func (c *limitedPacketConn) Upstream() any { return c.PacketConn }
