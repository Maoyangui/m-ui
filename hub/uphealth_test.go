package hub

import "testing"

// 上游页在副机上测出来的结果要直接写进汇总(概览拼的就是这份),比那台上一轮上报的新才写。
func TestSetUpstreamHealthWritesBackNewerOnly(t *testing.T) {
	h := &Hub{upHealth: map[uint][]UpstreamHealth{}}
	h.SetUpstreamHealth(2, UpstreamHealth{Id: 7, Name: "u", OK: false, Error: "x", CheckedAt: 10})
	all := h.UpstreamHealthAll()
	if len(all[2]) != 1 || all[2][0].Fails != 1 {
		t.Fatalf("新条目失败应记 1 次: %+v", all[2])
	}
	h.SetUpstreamHealth(2, UpstreamHealth{Id: 7, Name: "u", OK: true, DelayMs: 60, CheckedAt: 11})
	all = h.UpstreamHealthAll()
	if len(all[2]) != 1 || !all[2][0].OK || all[2][0].Fails != 0 || all[2][0].DelayMs != 60 {
		t.Fatalf("恢复应覆盖同一条目并清零: %+v", all[2])
	}
	h.SetUpstreamHealth(2, UpstreamHealth{Id: 7, Name: "u", OK: false, CheckedAt: 3})
	if all = h.UpstreamHealthAll(); !all[2][0].OK {
		t.Fatalf("旧的失败不能盖掉新的恢复: %+v", all[2])
	}
	// 另一条上游追加,不影响已有的
	h.SetUpstreamHealth(2, UpstreamHealth{Id: 8, Name: "v", OK: true, CheckedAt: 11})
	if all = h.UpstreamHealthAll(); len(all[2]) != 2 {
		t.Fatalf("应有两条: %+v", all[2])
	}
}
