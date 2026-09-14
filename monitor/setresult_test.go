package monitor

import "testing"

// 面板「测试」按钮测出来的结果要写进巡检结果,概览才不会一直挂着上一轮的旧故障;
// 比缓存新的才写,连续失败次数接着算。
func TestSetResultWritesBackAndKeepsFailStreak(t *testing.T) {
	m := New(Deps{})
	m.SetResult(UpstreamHealth{Id: 1, Name: "u", OK: false, Error: "timeout", CheckedAt: 10})
	if got := m.Results(); len(got) != 1 || got[0].Fails != 1 || got[0].OK {
		t.Fatalf("第一次失败应记 1 次: %+v", got)
	}
	m.SetResult(UpstreamHealth{Id: 1, Name: "u", OK: false, Error: "timeout", CheckedAt: 11})
	if got := m.Results(); got[0].Fails != 2 {
		t.Fatalf("连续失败应累计到 2: %+v", got)
	}
	m.SetResult(UpstreamHealth{Id: 1, Name: "u", OK: true, DelayMs: 80, CheckedAt: 12})
	if got := m.Results(); !got[0].OK || got[0].Fails != 0 || got[0].DelayMs != 80 {
		t.Fatalf("恢复应清零并更新延迟: %+v", got)
	}
	// 比缓存旧的结果(定时巡检和手动测试交错到达)不能把新的盖掉
	m.SetResult(UpstreamHealth{Id: 1, Name: "u", OK: false, CheckedAt: 5})
	if got := m.Results(); !got[0].OK || got[0].CheckedAt != 12 {
		t.Fatalf("旧结果不该覆盖新结果: %+v", got)
	}
}
