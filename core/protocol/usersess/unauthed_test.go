package usersess

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sagernet/quic-go"
)

// TUIC 认证没过的判定:QUIC 握手完了、却从没开过流(没绑上用户),不是我们撤销的、入站也没在关,
// 并且是本端用 CloseWithError 关的(sing-quic 的 TUIC 认证失败 / 超时没认证就是这样关的)。
func TestUnauthedSessionDetection(t *testing.T) {
	r := New(creds("alice", "pa"))
	never := r.Begin(func() {}, nil)
	if !neverAdmitted(never, r) {
		t.Fatal("没开过流的会话")
	}
	ok := r.Begin(func() {}, nil)
	r.Admit(ok, "alice")
	if neverAdmitted(ok, r) {
		t.Fatal("开过流、绑上了用户的不算")
	}
	revoked := r.Begin(func() {}, nil)
	revoked.Close() // 我们撤销 / 踢掉的
	if neverAdmitted(revoked, r) {
		t.Fatal("我们关的不算")
	}
	r.Clear() // 入站在关
	if neverAdmitted(never, r) {
		t.Fatal("入站在关时不算")
	}

	local := &quic.ApplicationError{ErrorCode: 0}
	remote := &quic.ApplicationError{ErrorCode: 0, Remote: true}
	if !closedLocally(local) || !closedLocally(fmt.Errorf("x: %w", local)) {
		t.Fatal("本端 CloseWithError 应算")
	}
	for _, err := range []error{remote, &quic.IdleTimeoutError{}, context.Canceled, errors.New("x"), nil} {
		if closedLocally(err) {
			t.Fatalf("%v 不算本端关的", err)
		}
	}
}
