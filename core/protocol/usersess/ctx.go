package usersess

import (
	"context"
	"net"
	"sync/atomic"

	"github.com/sagernet/sing/common"
)

// 会话怎么跟着流走到入站的处理函数:
//   - anytls:我们自己调 service.NewConnection,把会话塞进 ctx,每条流的 ctx 都派生自它(WithSession / FromContext)。
//   - hy2 / TUIC:流的 ctx 派生自 QUIC 连接的 ctx,而连接的 ctx 是我们在监听时(ConnContext)造的,
//     里面放一个槽位;Accept 到连接时把会话写进槽位,流进来时从流的 ctx 取(FromQUICStream)。

type sessionKey struct{}

// WithSession 把会话放进 ctx(anytls)。
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// FromContext 取 WithSession 放进去的会话;没有返回 nil。
func FromContext(ctx context.Context) *Session {
	s, _ := ctx.Value(sessionKey{}).(*Session)
	return s
}

type slotKey struct{}

// slot 连接 ctx 里的槽位:ConnContext 时连接对象还没生出来,只能先放一个空槽,Accept 之后再填。
type slot struct{ atomic.Pointer[Session] }

func withSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, slotKey{}, &slot{})
}

func slotOf(ctx context.Context) *slot {
	s, _ := ctx.Value(slotKey{}).(*slot)
	return s
}

// FromQUICStream 从一条 QUIC 流(hy2 的 serverConn、TUIC 可能再套一层 CachedConn)找到它所属的会话。
// 拿不到(不是 QUIC 流、或扩展点没生效)返回 nil,调用方退回只查名字。
func FromQUICStream(conn net.Conn) *Session {
	c, ok := common.Cast[interface{ Context() context.Context }](conn)
	if !ok {
		return nil
	}
	sl := slotOf(c.Context())
	if sl == nil {
		return nil
	}
	return sl.Load()
}
