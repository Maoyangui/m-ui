package core

import (
	"testing"

	"golang.org/x/time/rate"
)

// 同样的参数再下发一遍(主机推了份只改备注的快照):桶不该换,否则每次热更新都回到满桶;
// 参数变了桶原地改速率;用户没了桶置空。
func TestSetLimitsKeepsUnchangedBuckets(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"a": {UpMbps: 10, DownMbps: 20}, "b": {UpMbps: 5}})
	ua, ub := l.entries["a"].up.Load(), l.entries["b"].up.Load()
	if ua == nil || ub == nil {
		t.Fatal("应建出桶")
	}
	l.SetLimits(map[string]UserLimitSpec{"a": {UpMbps: 10, DownMbps: 20}, "b": {UpMbps: 5}})
	if l.entries["a"].up.Load() != ua || l.entries["b"].up.Load() != ub {
		t.Fatal("参数没变的桶应保留")
	}
	l.SetLimits(map[string]UserLimitSpec{"a": {UpMbps: 1, DownMbps: 20}})
	if got := l.entries["a"].up.Load(); got != ua || got.Limit() != rate.Limit(1*125000) {
		t.Fatal("参数变了应原地改速率,桶不换")
	}
	if l.entries["b"].up.Load() != nil {
		t.Fatal("用户没了桶应置空")
	}
}
