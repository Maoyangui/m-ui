package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"

	"github.com/Maoyangui/m-ui/core/protocol/arrival"
)

// "到了但没过握手 / 认证"单独存:不进正常的最近连接、不挤掉它;同一来源再来只累加;新来源限频、有上限,满了丢最旧的。
func TestHandshakeFailuresSeparateAndLimited(t *testing.T) {
	c := NewConnTracker(nil)
	c.noteRecent("203.0.113.1", "alice", "tj", "trojan", "")
	c.NoteFailure("203.0.113.9", "tj", "trojan", arrival.TLS, "remote error: tls: bad certificate")
	c.NoteFailure("203.0.113.9", "tj", "trojan", arrival.Auth, "bad request")
	if r := c.Recent(0); len(r) != 1 || r[0].IP != "203.0.113.1" || r[0].Failed != "" {
		t.Fatalf("失败记录不能混进正常的最近连接: %+v", r)
	}
	f := c.Failures(0)
	if len(f) != 1 || f[0].Count != 2 || f[0].Failed != arrival.Auth || f[0].Detail != "bad request" || f[0].Inbound != "tj" {
		t.Fatalf("同一来源应累加、原因取最近一次: %+v", f)
	}

	// 新来源限频:桶里还剩 failBurst-1 个令牌,再来一大批只进得去这么多
	for i := 0; i < 200; i++ {
		c.NoteFailure(fmt.Sprintf("198.51.100.%d", i), "tj", "trojan", arrival.TLS, "x")
	}
	if n := len(c.Failures(0)); n != failBurst {
		t.Fatalf("新来源应被限频到 %d 个,实际 %d", failBurst, n)
	}
	// 令牌用完时,已在表里的来源照样累加(用户自己反复重试的那条一直在)
	c.NoteFailure("203.0.113.9", "tj", "trojan", arrival.TLS, "again")
	if f := c.Failures(1); len(f) != 1 || f[0].IP != "203.0.113.9" || f[0].Count != 3 {
		t.Fatalf("已有来源不占令牌、应排到最前: %+v", f)
	}
	// 过一阵补满令牌,一直有新来源:不超过上限,挤掉的是最久没动的
	for round := 0; round < 10; round++ {
		c.failMu.Lock()
		c.failAt = c.failAt.Add(-time.Duration(failBurst) * failRefill)
		c.failMu.Unlock()
		for i := 0; i < failBurst; i++ {
			c.NoteFailure(fmt.Sprintf("192.0.2.%d", round*failBurst+i), "tj", "trojan", arrival.TLS, "x")
		}
	}
	f = c.Failures(0)
	if len(f) != failCap {
		t.Fatalf("应封顶 %d 条,实际 %d", failCap, len(f))
	}
	for _, r := range f {
		if r.IP == "198.51.100.0" {
			t.Fatal("满了应先挤掉最久没动的")
		}
	}
	if r := c.Recent(0); len(r) != 1 {
		t.Fatalf("扫描刷不掉正常记录: %+v", r)
	}
}

type fakeRecorder struct{ got []string }

func (f *fakeRecorder) NoteFailure(ip, inbound, protocol, reason, detail string) {
	f.got = append(f.got, ip+"|"+inbound+"|"+protocol+"|"+reason+"|"+detail)
}

// 什么都没发就断(端口扫描、TCP 连通探测,包括面板自己的大陆 / IPv6 入站检测)、被重置、超时:不记。
// 真正的握手 / 认证错误照记,IPv4 映射地址按 IPv4 记;记录器为空(干跑)什么都不做。
func TestArrivalNoteSkipsQuietErrors(t *testing.T) {
	rec := &fakeRecorder{}
	src := M.SocksaddrFrom(netip.MustParseAddr("::ffff:203.0.113.5"), 443)
	for _, err := range []error{io.EOF, fmt.Errorf("tls: %w", io.ErrUnexpectedEOF), syscall.ECONNRESET, os.ErrDeadlineExceeded, context.Canceled} {
		arrival.Note(rec, src, "tj", "trojan", arrival.TLS, err)
	}
	if len(rec.got) != 0 {
		t.Fatalf("这些不该记: %v", rec.got)
	}
	arrival.Note(rec, src, "tj", "trojan", arrival.TLS, errors.New("remote error: tls: bad certificate"))
	arrival.Note(rec, M.Socksaddr{Fqdn: "example.com", Port: 443}, "tj", "trojan", arrival.TLS, errors.New("x"))
	arrival.NoteAddr(rec, "[2001:db8::7]:4433", "hy", "hysteria2", arrival.Auth, "")
	arrival.NoteAddr(rec, "not-an-addr", "hy", "hysteria2", arrival.Auth, "")
	arrival.Note(nil, src, "tj", "trojan", arrival.TLS, errors.New("x"))
	want := []string{"203.0.113.5|tj|trojan|tls|remote error: tls: bad certificate", "2001:db8::7|hy|hysteria2|auth|"}
	if fmt.Sprint(rec.got) != fmt.Sprint(want) {
		t.Fatalf("记录 = %v,应为 %v", rec.got, want)
	}
}

// 标记:协议服务认证之后才调入站的处理函数,处理函数里 Pass;服务返回错误时没 Pass 的才算认证没过。
func TestArrivalMark(t *testing.T) {
	ctx, m := arrival.WithMark(context.Background())
	if m.Passed() {
		t.Fatal("还没进到路由")
	}
	arrival.Pass(context.WithValue(ctx, struct{}{}, 1)) // 服务层派生的 ctx 照样找得到
	if !m.Passed() {
		t.Fatal("Pass 之后应为已通过")
	}
	arrival.Pass(context.Background()) // 没有标记时什么都不做
	var nilMark *arrival.Mark
	if nilMark.Passed() {
		t.Fatal("nil 标记")
	}
}
