package anytls

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/anytls/sing-anytls/padding"
	"github.com/anytls/sing-anytls/session"
	"github.com/sagernet/sing/common/atomic"
	"github.com/sagernet/sing/common/logger"
)

// TestStreamCloseWhileWriting 会话关闭(踢线、对端断开、换密码)和流上正在进行的 Write 同时发生。
// 上游 sing-anytls(v0.0.11,到 v0.0.13 仍未修)的 Stream.dieErr 是普通字段:Session.Close → closeLocally
// 在 sync.Once 里写它,Write / Read 在转发 goroutine 里不加锁读它,两边没有任何先后关系,-race 下每次必报。
// m-ui 用的是 third_party/sing-anytls 里打了补丁的副本(dieErr 改成 atomic.Pointer[error]),
// 这个用例在 -race 下跑(go test -race ./core/protocol/anytls/)就是在守这个补丁:换回未修的上游会立刻失败。
//
// 不依赖时机:写 dieErr 的(主 goroutine 里的 Close)和读 dieErr 的(另起的 Write)之间只有 go 语句这一个先后,
// 竞态检测按 happens-before 判断,不需要两边真的撞在同一时刻。
func TestStreamCloseWhileWriting(t *testing.T) {
	for i := 0; i < 20; i++ {
		local, remote := net.Pipe()
		go io.Copy(io.Discard, remote)

		var pad atomic.TypedValue[*padding.PaddingFactory]
		padding.UpdatePaddingScheme(padding.DefaultPaddingScheme, &pad)
		sess := session.NewClientSession(local, &pad, logger.NOP())
		st, err := sess.OpenStream()
		if err != nil {
			t.Fatalf("开流失败: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := st.Write([]byte("payload"))
			done <- err
		}()
		sess.Close()
		<-done // Write 在关闭前后完成都行,关心的只是 dieErr 的读写有没有同步

		// 关闭之后:Write 必须报已关闭,Read 不许卡住、要带回关闭原因
		if _, err := st.Write([]byte("after")); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("会话关闭后 Write 应返回 net.ErrClosed,得到 %v", err)
		}
		if _, err := st.Read(make([]byte, 8)); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("会话关闭后 Read 应返回 net.ErrClosed,得到 %v", err)
		}
		remote.Close()
	}
}

// TestCreateStreamWhileSessionDies 客户端刚开好流、会话就被对端断掉(服务端认证失败直接关连接、网络断)。
// 上游 Client.CreateStream 在 OpenStream 把流登记进会话之后才写 stream.dieHook,而会话的 recvLoop 读到断开就
// Session.Close → closeLocally 去读 dieHook,两边没有先后关系(e2e 里错误密码那一步在 -race 下撞到过)。
// 补丁把 dieHook 改成 atomic.Pointer[func()]。拨号直接给一条对端已关的管道,recvLoop 一跑就断开会话。
func TestCreateStreamWhileSessionDies(t *testing.T) {
	var pad atomic.TypedValue[*padding.PaddingFactory]
	padding.UpdatePaddingScheme(padding.DefaultPaddingScheme, &pad)
	for i := 0; i < 50; i++ {
		dial := func(context.Context) (net.Conn, error) {
			local, remote := net.Pipe()
			remote.Close()
			return local, nil
		}
		c := session.NewClient(context.Background(), logger.NOP(), dial, &pad, 0, 0, 0)
		st, err := c.CreateStream(context.Background())
		if err == nil {
			// 等 recvLoop 把会话关掉(流登记进去了就一定会被关);关掉后 Read 要带回关闭原因
			st.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := st.Read(make([]byte, 8)); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("会话断开后 Read 应返回 net.ErrClosed,得到 %v", err)
			}
			st.Close()
		}
		c.Close()
	}
}

// TestOpenStreamWhileWriting 同一个会话上一条流正在写,另一条流同时打开(会话复用时的常态)。
// 上游 OpenStream 不加锁写 Session.buffering,writeConn 在 connLock 里读它;补丁让 OpenStream 也在 connLock 里写。
func TestOpenStreamWhileWriting(t *testing.T) {
	for i := 0; i < 20; i++ {
		local, remote := net.Pipe()
		go io.Copy(io.Discard, remote)

		var pad atomic.TypedValue[*padding.PaddingFactory]
		padding.UpdatePaddingScheme(padding.DefaultPaddingScheme, &pad)
		sess := session.NewClientSession(local, &pad, logger.NOP())
		sess.Run()
		st, err := sess.OpenStream()
		if err != nil {
			t.Fatalf("开流失败: %v", err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			for j := 0; j < 5; j++ {
				if _, err := st.Write([]byte("payload")); err != nil {
					return
				}
			}
		}()
		st2, err := sess.OpenStream()
		if err != nil {
			t.Fatalf("开第二条流失败: %v", err)
		}
		<-done
		st2.Close()
		sess.Close()
		remote.Close()
	}
}
