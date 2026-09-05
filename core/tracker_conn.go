package core

import (
	"context"
	"errors"
	"io"
	"net"
	"sort"
	"sync"

	"github.com/gofrs/uuid/v5"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"

	"github.com/Maoyangui/m-ui/database/model"
)

type ConnectionInfo struct {
	ID         string
	Conn       net.Conn
	PacketConn network.PacketConn
	Inbound    string
	User       string
	SourceIP   string // 客户端源 IP(建连时从 metadata 取,面板"在线设备"按它归类线路)
	Type       string // "tcp" or "udp"
}

type ConnTracker struct {
	access      sync.Mutex
	connections map[string]*ConnectionInfo
	limiter     *Limiter
}

func NewConnTracker(limiter *Limiter) *ConnTracker {
	return &ConnTracker{
		connections: make(map[string]*ConnectionInfo),
		limiter:     limiter,
	}
}

// rejectedConn 是设备数超限时返回的连接:任何读写立即失败,使该次拨号被拒绝。
type rejectedConn struct{ net.Conn }

func (rejectedConn) Read([]byte) (int, error)  { return 0, io.EOF }
func (rejectedConn) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func (c *ConnTracker) Reset() {
	c.access.Lock()
	old := c.connections
	c.connections = make(map[string]*ConnectionInfo)
	c.access.Unlock()

	for _, info := range old { // Close 是系统调用,别压在锁里
		if info.Conn != nil {
			_ = info.Conn.Close()
		}
		if info.PacketConn != nil {
			_ = info.PacketConn.Close()
		}
	}
}

func (c *ConnTracker) generateConnectionID() string {
	return uuid.Must(uuid.NewV4()).String()
}

// 连接记录里的 User 是数据面里的原名(临时共享的凭据叫 "名字#share"),用于按配置里的用户表断连;
// 对外的记账(设备数、限速、在线 IP、连接数、踢下线)一律按 model.Owner 归到本人。
func (c *ConnTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	ip := metadata.Source.Addr.String()
	owner := model.Owner(metadata.User)
	if c.limiter != nil && !c.limiter.AllowConn(owner, ip) {
		return rejectedConn{conn}
	}
	connID := c.generateConnectionID()
	connInfo := &ConnectionInfo{
		ID:       connID,
		Conn:     conn,
		Inbound:  metadata.Inbound,
		User:     metadata.User,
		SourceIP: ip,
		Type:     "tcp",
	}

	c.trackConnection(connID, connInfo)

	wrapped := c.createWrappedConn(conn, connID)
	if c.limiter != nil {
		return c.limiter.wrapConn(wrapped, owner, ip)
	}
	return wrapped
}

func (c *ConnTracker) RoutedPacketConnection(ctx context.Context, conn network.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) network.PacketConn {
	ip := metadata.Source.Addr.String()
	owner := model.Owner(metadata.User)
	if c.limiter != nil && !c.limiter.AllowConn(owner, ip) {
		conn.Close()
		return conn
	}
	connID := c.generateConnectionID()
	connInfo := &ConnectionInfo{
		ID:         connID,
		PacketConn: conn,
		Inbound:    metadata.Inbound,
		User:       metadata.User,
		SourceIP:   ip,
		Type:       "udp",
	}

	c.trackConnection(connID, connInfo)

	wrapped := c.createWrappedPacketConn(conn, connID)
	if c.limiter != nil {
		return c.limiter.wrapPacketConn(wrapped, owner, ip)
	}
	return wrapped
}

func (c *ConnTracker) CloseConnByInbound(inbound string) int {
	return c.closeMatching(func(info *ConnectionInfo) bool { return info.Inbound == inbound })
}

// CloseConnByUser 断开某用户在所有入站上的全部连接(踢下线):本人的和借用者的一起断。
func (c *ConnTracker) CloseConnByUser(user string) int {
	return c.closeMatching(func(info *ConnectionInfo) bool { return model.Owner(info.User) == user })
}

// CloseConnByDataPlaneName 只断某个数据面名字的连接:取消共享时传 "名字#share",本人的连接不动。
func (c *ConnTracker) CloseConnByDataPlaneName(name string) int {
	return c.closeMatching(func(info *ConnectionInfo) bool { return info.User == name })
}

// IPLinesByUser 返回 用户 → 源 IP → 该 IP 正在使用的线路(入站)名。
// 面板"在线设备"里据此标出每个 IP 走的是哪条线路,而不只是一串 IP。
func (c *ConnTracker) IPLinesByUser() map[string]map[string][]string {
	c.access.Lock()
	defer c.access.Unlock()
	seen := map[string]map[string]map[string]bool{} // user → ip → 线路集合
	for _, info := range c.connections {
		if info.User == "" || info.Inbound == "" {
			continue
		}
		ip := info.SourceIP
		if ip == "" {
			continue
		}
		user := model.Owner(info.User) // 借用者的设备也算本人的
		if seen[user] == nil {
			seen[user] = map[string]map[string]bool{}
		}
		if seen[user][ip] == nil {
			seen[user][ip] = map[string]bool{}
		}
		seen[user][ip][info.Inbound] = true
	}
	out := make(map[string]map[string][]string, len(seen))
	for user, ips := range seen {
		out[user] = make(map[string][]string, len(ips))
		for ip, lines := range ips {
			names := make([]string, 0, len(lines))
			for l := range lines {
				names = append(names, l)
			}
			sort.Strings(names)
			out[user][ip] = names
		}
	}
	return out
}

// ConnCountByUser 返回每个用户当前的连接数(面板展示用)。
func (c *ConnTracker) ConnCountByUser() map[string]int {
	c.access.Lock()
	defer c.access.Unlock()
	out := map[string]int{}
	for _, info := range c.connections {
		if info.User != "" {
			out[model.Owner(info.User)]++
		}
	}
	return out
}

func (c *ConnTracker) CloseConnByInboundUsers(inbound string, keepUsers map[string]struct{}) int {
	return c.closeMatching(func(info *ConnectionInfo) bool {
		if info.Inbound != inbound {
			return false
		}
		_, keep := keepUsers[info.User]
		return !keep
	})
}

// CloseConnsNotIn 一次扫描处理所有入站:keep[入站][用户名] 里没有的连接全部关掉。
// 热更新用户时不要按入站各扫一遍——线路一多就是几十遍全表扫描,每遍都占着锁。
func (c *ConnTracker) CloseConnsNotIn(keep map[string]map[string]struct{}) int {
	return c.closeMatching(func(info *ConnectionInfo) bool {
		users, ok := keep[info.Inbound]
		if !ok {
			return false // 这个入站不在本次下发范围内,不动它
		}
		_, ok = users[info.User]
		return !ok
	})
}

// closeMatching 在锁内挑出要关的连接并从表里摘掉,出锁之后再真正 Close。
func (c *ConnTracker) closeMatching(match func(*ConnectionInfo) bool) int {
	c.access.Lock()
	victims := make([]*ConnectionInfo, 0, 16)
	for connID, info := range c.connections {
		if match(info) {
			victims = append(victims, info)
			delete(c.connections, connID)
		}
	}
	c.access.Unlock()

	for _, info := range victims {
		if info.Conn != nil {
			info.Conn.Close()
		}
		if info.PacketConn != nil {
			info.PacketConn.Close()
		}
	}
	return len(victims)
}

func (c *ConnTracker) trackConnection(connID string, connInfo *ConnectionInfo) {
	c.access.Lock()
	defer c.access.Unlock()
	c.connections[connID] = connInfo
}

func (c *ConnTracker) untrackConnection(connID string) {
	c.access.Lock()
	defer c.access.Unlock()
	delete(c.connections, connID)
}

// shouldUntrackIOErr reports whether err indicates the connection is done (peer closed, reset, etc.).
func shouldUntrackIOErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return !ne.Temporary()
	}
	return true
}

func (c *ConnTracker) createWrappedConn(conn net.Conn, connID string) *wrappedConn {
	return &wrappedConn{
		Conn:    conn,
		tracker: c,
		connID:  connID,
	}
}

func (c *ConnTracker) createWrappedPacketConn(conn network.PacketConn, connID string) *wrappedPacketConn {
	return &wrappedPacketConn{
		PacketConn: conn,
		tracker:    c,
		connID:     connID,
	}
}

type wrappedConn struct {
	net.Conn
	tracker     *ConnTracker
	connID      string
	untrackOnce sync.Once
}

func (w *wrappedConn) doUntrack() {
	w.untrackOnce.Do(func() {
		w.tracker.untrackConnection(w.connID)
	})
}

func (w *wrappedConn) Read(b []byte) (int, error) {
	n, err := w.Conn.Read(b)
	if shouldUntrackIOErr(err) {
		w.doUntrack()
	}
	return n, err
}

func (w *wrappedConn) Write(b []byte) (int, error) {
	n, err := w.Conn.Write(b)
	if err != nil && shouldUntrackIOErr(err) {
		w.doUntrack()
	}
	return n, err
}

func (w *wrappedConn) Close() error {
	w.doUntrack()
	return w.Conn.Close()
}

func (w *wrappedConn) Upstream() any {
	return w.Conn
}

type wrappedPacketConn struct {
	network.PacketConn
	tracker     *ConnTracker
	connID      string
	untrackOnce sync.Once
}

func (w *wrappedPacketConn) doUntrack() {
	w.untrackOnce.Do(func() {
		w.tracker.untrackConnection(w.connID)
	})
}

func (w *wrappedPacketConn) ReadPacket(buffer *buf.Buffer) (destination M.Socksaddr, err error) {
	dest, err := w.PacketConn.ReadPacket(buffer)
	if shouldUntrackIOErr(err) {
		w.doUntrack()
	}
	return dest, err
}

func (w *wrappedPacketConn) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	err := w.PacketConn.WritePacket(buffer, destination)
	if err != nil && shouldUntrackIOErr(err) {
		w.doUntrack()
	}
	return err
}

func (w *wrappedPacketConn) Close() error {
	w.doUntrack()
	return w.PacketConn.Close()
}

func (w *wrappedPacketConn) Upstream() any {
	return w.PacketConn
}

func (t *ConnTracker) RoutedFlow(ctx context.Context, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) tun.FlowTracker {
	return nil
}
