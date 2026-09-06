package core

import "testing"

func TestSetLimitsKeepsUnchangedBuckets(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"a": {UpMbps: 10, DownMbps: 20}, "b": {UpMbps: 5}})
	l.mu.Lock()
	ua, _, _, _, _ := l.bucketsLocked("a")
	ub, _, _, _, _ := l.bucketsLocked("b")
	l.mu.Unlock()
	if ua == nil || ub == nil {
		t.Fatal("应建出桶")
	}
	// 同样的参数再下发一遍(主机推了份只改备注的快照):桶不该换
	l.SetLimits(map[string]UserLimitSpec{"a": {UpMbps: 10, DownMbps: 20}, "b": {UpMbps: 5}})
	l.mu.Lock()
	ua2, _, _, _, _ := l.bucketsLocked("a")
	ub2, _, _, _, _ := l.bucketsLocked("b")
	l.mu.Unlock()
	if ua2 != ua || ub2 != ub {
		t.Fatal("参数没变的桶应保留(否则每次热更新都回到满桶)")
	}
	// 改了 a 的上行:a 的桶换新,b 不动;去掉 b:b 的桶被清
	l.SetLimits(map[string]UserLimitSpec{"a": {UpMbps: 1, DownMbps: 20}})
	l.mu.Lock()
	ua3, _, _, _, _ := l.bucketsLocked("a")
	_, okB := l.up["b"]
	l.mu.Unlock()
	if ua3 == ua || okB {
		t.Fatal("参数变了要重建,用户没了要清掉")
	}
}
