package core

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/bufio"
	"github.com/sagernet/sing/common/network"
)

type Counter struct {
	read  *atomic.Int64
	write *atomic.Int64
}

type StatsTracker struct {
	access    sync.Mutex
	inbounds  map[string]Counter
	outbounds map[string]Counter
	users     map[string]Counter
	// userx 走了「单独设倍率的节点」的用户流量,键见 UserXTag(用户名 + 倍率)。这部分不进 users:每个字节只在一个计数器里
	userx map[string]Counter
	// lineu 「用户 × 线路」的计数(键见 lineUseKey):每条用户连接都记,和上面几样并行、互不影响,
	// 只给勾了「忽略部分线路」的突发规则算用量(jobs 每轮用 TakeLineUsage 取走;功能没开时取走就扔)
	lineu map[string]Counter
	nodes atomic.Pointer[NodeTable]
}

func NewStatsTracker() *StatsTracker {
	return &StatsTracker{
		inbounds:  make(map[string]Counter),
		outbounds: make(map[string]Counter),
		users:     make(map[string]Counter),
		userx:     make(map[string]Counter),
		lineu:     make(map[string]Counter),
	}
}

func (c *StatsTracker) Reset() {
	c.access.Lock()
	defer c.access.Unlock()
	c.inbounds = make(map[string]Counter)
	c.outbounds = make(map[string]Counter)
	c.users = make(map[string]Counter)
	c.userx = make(map[string]Counter)
	c.lineu = make(map[string]Counter)
}

// SetNodes 换上新的节点表(渲染配置时算好的);nil / 空 = 没有节点单独设倍率。只影响之后新建的连接。
func (c *StatsTracker) SetNodes(t NodeTable) {
	if len(t) == 0 {
		c.nodes.Store(nil)
		return
	}
	c.nodes.Store(&t)
}

// userCounterKey 这条连接的用户流量记到哪:走的节点单独设了倍率 → (true, UserXTag);否则 (false, 用户名)。
func (c *StatsTracker) userCounterKey(user string, metadata adapter.InboundContext) (bool, string) {
	if user == "" {
		return false, ""
	}
	if t := c.nodes.Load(); t != nil {
		if spec := t.Lookup(metadata.Inbound, metadata.Source.Addr); spec.Ratio > 0 {
			return true, UserXTag(user, spec.Ratio)
		}
	}
	return false, user
}

func (c *StatsTracker) countersFor(metadata adapter.InboundContext, outbound string) ([]*atomic.Int64, []*atomic.Int64) {
	owner := model.Owner(metadata.User) // 借用者的流量记到本人
	x, key := c.userCounterKey(owner, metadata)
	readCounter, writeCounter := c.getReadCounters(metadata.Inbound, outbound, "")
	if key == "" {
		return readCounter, writeCounter
	}
	c.access.Lock()
	defer c.access.Unlock()
	m := &c.users
	if x {
		m = &c.userx
	}
	ct := c.loadOrCreateCounter(m, key)
	readCounter, writeCounter = append(readCounter, ct.read), append(writeCounter, ct.write)
	if metadata.Inbound != "" {
		ratio := 0.0 // 0 = 按服务器的倍率
		if x {
			_, ratio, _ = SplitUserX(key)
		}
		lc := c.loadOrCreateCounter(&c.lineu, lineUseKey(owner, metadata.Inbound, ratio))
		readCounter, writeCounter = append(readCounter, lc.read), append(writeCounter, lc.write)
	}
	return readCounter, writeCounter
}

// LineUse 一个用户在一个入站(线路,入站 tag 就是线路名)上自上次取走以来的字节(上下行合计)。
// Ratio > 0 = 走的是单独设了倍率的节点(计费要乘它);0 = 按服务器的倍率。
type LineUse struct {
	User    string
	Inbound string
	Ratio   float64
	Bytes   int64
}

// lineUseKey 拼 lineu 的键:用户名、入站 tag、倍率用 userXSep 隔开(用户名和线路名里都不会有这个控制字符)。
func lineUseKey(user, inbound string, ratio float64) string {
	return user + userXSep + inbound + userXSep + strconv.FormatFloat(ratio, 'f', -1, 64)
}

// TakeLineUsage 取走「用户 × 线路」的计数(取走即清零,不论功能开没开:没开时调用方扔掉,计数器不会一直涨)。
func (c *StatsTracker) TakeLineUsage() []LineUse {
	c.access.Lock()
	defer c.access.Unlock()
	var out []LineUse
	for k, ct := range c.lineu {
		n := ct.read.Swap(0) + ct.write.Swap(0)
		if n <= 0 {
			continue
		}
		parts := strings.SplitN(k, userXSep, 3)
		if len(parts) != 3 {
			continue
		}
		r, err := strconv.ParseFloat(parts[2], 64)
		if err != nil || r < 0 {
			continue
		}
		out = append(out, LineUse{User: parts[0], Inbound: parts[1], Ratio: r, Bytes: n})
	}
	return out
}

func (c *StatsTracker) getReadCounters(inbound string, outbound string, user string) ([]*atomic.Int64, []*atomic.Int64) {
	var readCounter []*atomic.Int64
	var writeCounter []*atomic.Int64
	c.access.Lock()
	defer c.access.Unlock()

	if inbound != "" {
		readCounter = append(readCounter, c.loadOrCreateCounter(&c.inbounds, inbound).read)
		writeCounter = append(writeCounter, c.inbounds[inbound].write)
	}
	if outbound != "" {
		readCounter = append(readCounter, c.loadOrCreateCounter(&c.outbounds, outbound).read)
		writeCounter = append(writeCounter, c.outbounds[outbound].write)
	}
	if user != "" {
		readCounter = append(readCounter, c.loadOrCreateCounter(&c.users, user).read)
		writeCounter = append(writeCounter, c.users[user].write)
	}
	return readCounter, writeCounter
}

func (c *StatsTracker) loadOrCreateCounter(obj *map[string]Counter, name string) Counter {
	counter, loaded := (*obj)[name]
	if loaded {
		return counter
	}
	counter = Counter{read: &atomic.Int64{}, write: &atomic.Int64{}}
	(*obj)[name] = counter
	return counter
}

func (c *StatsTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) (out net.Conn) {
	defer func() { // 见 tracker_guard.go:记流量出意外,这一条不记,连接照常
		if v := recover(); v != nil {
			trackerRecovered("流量统计 tcp", v)
			out = conn
		}
	}()
	readCounter, writeCounter := c.countersFor(metadata, matchOutbound.Tag())
	return bufio.NewInt64CounterConn(conn, readCounter, writeCounter)
}

func (c *StatsTracker) RoutedPacketConnection(ctx context.Context, conn network.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) (out network.PacketConn) {
	defer func() { // 见 tracker_guard.go
		if v := recover(); v != nil {
			trackerRecovered("流量统计 udp", v)
			out = conn
		}
	}()
	readCounter, writeCounter := c.countersFor(metadata, matchOutbound.Tag())
	return bufio.NewInt64CounterPacketConn(conn, readCounter, nil, writeCounter, nil)
}

func (c *StatsTracker) GetStats() *[]model.Stats {
	c.access.Lock()
	defer c.access.Unlock()

	dt := time.Now().Unix()

	// Emit only directions that actually moved traffic; a zero-traffic row would
	// just bloat the stats table without changing any chart bucket.
	s := []model.Stats{}
	appendStat := func(resource, tag string, down, up int64) {
		if down > 0 {
			s = append(s, model.Stats{DateTime: dt, Resource: resource, Tag: tag, Direction: false, Traffic: down})
		}
		if up > 0 {
			s = append(s, model.Stats{DateTime: dt, Resource: resource, Tag: tag, Direction: true, Traffic: up})
		}
	}

	for inbound, counter := range c.inbounds {
		appendStat("inbound", inbound, counter.write.Swap(0), counter.read.Swap(0))
	}
	for outbound, counter := range c.outbounds {
		appendStat("outbound", outbound, counter.write.Swap(0), counter.read.Swap(0))
	}
	for user, counter := range c.users {
		appendStat("user", user, counter.write.Swap(0), counter.read.Swap(0))
	}
	for key, counter := range c.userx {
		appendStat(StatUserX, key, counter.write.Swap(0), counter.read.Swap(0))
	}
	return &s
}

// SnapshotStats reads the current counters without clearing them. Callers can
// persist the returned batch and then ConsumeStats after the transaction
// commits; bytes that arrive while the transaction is running remain counted.
func (c *StatsTracker) SnapshotStats() *[]model.Stats {
	c.access.Lock()
	defer c.access.Unlock()
	dt := time.Now().Unix()
	s := []model.Stats{}
	appendStat := func(resource, tag string, down, up int64) {
		if down > 0 {
			s = append(s, model.Stats{DateTime: dt, Resource: resource, Tag: tag, Direction: false, Traffic: down})
		}
		if up > 0 {
			s = append(s, model.Stats{DateTime: dt, Resource: resource, Tag: tag, Direction: true, Traffic: up})
		}
	}
	for inbound, counter := range c.inbounds {
		appendStat("inbound", inbound, counter.write.Load(), counter.read.Load())
	}
	for outbound, counter := range c.outbounds {
		appendStat("outbound", outbound, counter.write.Load(), counter.read.Load())
	}
	for user, counter := range c.users {
		appendStat("user", user, counter.write.Load(), counter.read.Load())
	}
	for key, counter := range c.userx {
		appendStat(StatUserX, key, counter.write.Load(), counter.read.Load())
	}
	return &s
}

// ConsumeStats removes exactly a previously persisted snapshot. Atomic
// subtraction preserves increments that happened after the snapshot.
func (c *StatsTracker) ConsumeStats(stats []model.Stats) {
	if len(stats) == 0 {
		return
	}
	c.access.Lock()
	defer c.access.Unlock()
	for _, st := range stats {
		var counters *map[string]Counter
		switch st.Resource {
		case "inbound":
			counters = &c.inbounds
		case "outbound":
			counters = &c.outbounds
		case "user":
			counters = &c.users
		case StatUserX:
			counters = &c.userx
		default:
			continue
		}
		counter, ok := (*counters)[st.Tag]
		if !ok {
			continue
		}
		if st.Direction {
			counter.read.Add(-st.Traffic)
		} else {
			counter.write.Add(-st.Traffic)
		}
	}
}

// RestoreStats puts a batch returned by GetStats back into the in-memory
// counters. It is used when the database transaction that consumed a batch
// fails, so a transient SQLite error cannot silently lose traffic.
func (c *StatsTracker) RestoreStats(stats []model.Stats) {
	if len(stats) == 0 {
		return
	}
	c.access.Lock()
	defer c.access.Unlock()
	for _, st := range stats {
		var counters *map[string]Counter
		switch st.Resource {
		case "inbound":
			counters = &c.inbounds
		case "outbound":
			counters = &c.outbounds
		case "user":
			counters = &c.users
		case StatUserX:
			counters = &c.userx
		default:
			continue
		}
		counter := c.loadOrCreateCounter(counters, st.Tag)
		if st.Direction {
			counter.read.Add(st.Traffic)
		} else {
			counter.write.Add(st.Traffic)
		}
	}
}

func (c *StatsTracker) RoutedFlow(ctx context.Context, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) tun.FlowTracker {
	return nil
}
