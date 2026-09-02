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

// Limiter 提供两项 per-user 策略,均在数据面本机执行:
//   - 限速:每用户一对令牌桶(上行/下行),该用户所有连接共享总带宽
//   - 设备数:限制**同时在线**的源 IP 数量
//
// 设备数是并发语义,不是"锁定前 N 个 IP":某个 IP 只要在 idleWindow 内没有任何
// 流量就被视为下线并释放名额,新设备立刻可以顶上;有流量经过时持续刷新其活跃时间,
// 所以长连接(下载/看视频)的设备不会被误判下线。
//
// 跨双机的设备数并集判定在 Hub 侧完成(P4);本类型负责单机拦截与"当前在线 IP"上报。
type Limiter struct {
	mu     sync.Mutex
	limits map[string]userLimit
	up     map[string]*rate.Limiter // 上行桶(客户端→服务器,即 Read)
	down   map[string]*rate.Limiter // 下行桶(服务器→客户端,即 Write)
	ips    map[string]map[string]int64
	// external 是其他机器上该用户当前在线的源 IP(Hub 下发),计入设备数;本机已在线的 IP 不重复计
	external map[string]map[string]bool

	idleWindow time.Duration // 无流量多久判定该 IP 下线并释放名额
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

func NewLimiter() *Limiter {
	return &Limiter{
		limits:     map[string]userLimit{},
		up:         map[string]*rate.Limiter{},
		down:       map[string]*rate.Limiter{},
		ips:        map[string]map[string]int64{},
		idleWindow: 60 * time.Second,
	}
}

// SetLimits 全量替换用户策略(来自渲染时的用户表)。mbps 为 0 表示不限。
func (l *Limiter) SetLimits(limits map[string]UserLimitSpec) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits = map[string]userLimit{}
	for name, s := range limits {
		l.limits[name] = userLimit{
			upBps:       int64(s.UpMbps) * 125000, // Mbps → 字节/秒
			downBps:     int64(s.DownMbps) * 125000,
			deviceLimit: s.DeviceLimit,
		}
	}
	// 丢弃已不存在或参数变化的桶,下次取用时按新值重建
	l.up = map[string]*rate.Limiter{}
	l.down = map[string]*rate.Limiter{}
}

// UserLimitSpec 是 SetLimits 的入参(与 model.User 解耦,便于数据面独立测试)。
type UserLimitSpec struct {
	UpMbps      int
	DownMbps    int
	DeviceLimit int
}

// AllowConn 判定某用户从某源 IP 的新连接是否放行,并登记该 IP 的活跃时间。
// 空用户名(无鉴权连接)不受限。
//
// 判定顺序体现"同时在线"语义:先清掉已空闲的 IP 释放名额,已在线的 IP 直接放行,
// 只要当前在线数没到上限就接纳新 IP;超限只拒绝这一次,不对该 IP 留任何黑名单,
// 所以别的设备一下线,它下次重连即可进入。
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
	if limit > 0 {
		ext := l.external[user]
		if ext[ip] {
			// 该设备已在别的机器上在线,视为同一设备切换入口,不占新名额
			active[ip] = now
			return true
		}
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
	active[ip] = now
	return true
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

func (l *Limiter) bucketFor(m map[string]*rate.Limiter, user string, bps int64) *rate.Limiter {
	if bps <= 0 {
		return nil
	}
	if b, ok := m[user]; ok {
		return b
	}
	// 突发上限设为 1 秒带宽,兼顾峰值与平滑
	b := rate.NewLimiter(rate.Limit(bps), int(bps))
	m[user] = b
	return b
}

// wrapConn 按用户限速包装一条连接,并在有流量时刷新该设备的在线状态。
// 即使不限速,只要该用户有设备数限制也要包装,否则长连接设备会被误判为空闲下线。
func (l *Limiter) wrapConn(conn net.Conn, user, ip string) net.Conn {
	if user == "" {
		return conn
	}
	l.mu.Lock()
	lim := l.limits[user]
	upB := l.bucketFor(l.up, user, lim.upBps)
	downB := l.bucketFor(l.down, user, lim.downBps)
	l.mu.Unlock()
	if upB == nil && downB == nil && lim.deviceLimit == 0 {
		return conn
	}
	return &limitedConn{Conn: conn, up: upB, down: downB, keepalive: l.keepaliveFor(user, ip)}
}

func (l *Limiter) wrapPacketConn(conn N.PacketConn, user, ip string) N.PacketConn {
	if user == "" {
		return conn
	}
	l.mu.Lock()
	lim := l.limits[user]
	upB := l.bucketFor(l.up, user, lim.upBps)
	downB := l.bucketFor(l.down, user, lim.downBps)
	l.mu.Unlock()
	if upB == nil && downB == nil && lim.deviceLimit == 0 {
		return conn
	}
	return &limitedPacketConn{PacketConn: conn, up: upB, down: downB, keepalive: l.keepaliveFor(user, ip)}
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
	up        *rate.Limiter
	down      *rate.Limiter
	keepalive func()
}

func (c *limitedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.keepalive()
	}
	throttle(c.up, n)
	return n, err
}

func (c *limitedConn) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.keepalive()
	}
	throttle(c.down, len(p))
	return c.Conn.Write(p)
}

func (c *limitedConn) Upstream() any { return c.Conn }

type limitedPacketConn struct {
	N.PacketConn
	up        *rate.Limiter
	down      *rate.Limiter
	keepalive func()
}

func (c *limitedPacketConn) ReadPacket(b *buf.Buffer) (M.Socksaddr, error) {
	dest, err := c.PacketConn.ReadPacket(b)
	if b.Len() > 0 {
		c.keepalive()
	}
	throttle(c.up, b.Len())
	return dest, err
}

func (c *limitedPacketConn) WritePacket(b *buf.Buffer, dest M.Socksaddr) error {
	if b.Len() > 0 {
		c.keepalive()
	}
	throttle(c.down, b.Len())
	return c.PacketConn.WritePacket(b, dest)
}

func (c *limitedPacketConn) Upstream() any { return c.PacketConn }
