package jobs

import (
	"testing"

	"github.com/Maoyangui/m-ui/core"
)

// 本机「用户 × 线路」的累计:没开不记(开了换一个新的 Boot);按线路 id 记,认不出的入站不记;
// 单独设了倍率的节点按节点倍率乘好记进 Billed,其余记原始字节;25 小时没涨的一项不再报;关掉就清空。
func TestLineMeterRound(t *testing.T) {
	s := &Scheduler{}
	ids := map[string]uint{"便宜": 7, "贵": 8}
	s.meter.round(false, []core.LineUse{{User: "alice", Inbound: "便宜", Bytes: 100}}, ids, 1000)
	if _, on := s.LineMeter(); on {
		t.Fatal("没开不记")
	}
	s.meter.round(true, []core.LineUse{
		{User: "alice", Inbound: "便宜", Bytes: 100},
		{User: "alice", Inbound: "贵", Ratio: 2, Bytes: 10},
		{User: "alice", Inbound: "别的入站", Bytes: 5},
		{User: "", Inbound: "便宜", Bytes: 5},
	}, ids, 1000)
	boot := s.meter.boot
	s.meter.round(true, []core.LineUse{{User: "alice", Inbound: "便宜", Bytes: 50}}, ids, 2000)
	got, on := s.LineMeter()
	if !on || got.Boot != boot || boot == "" || len(got.Rows) != 2 {
		t.Fatalf("开着时记两项、Boot 不变: %+v", got)
	}
	if r := got.Rows[0]; r.Line != 7 || r.Raw != 150 || r.Billed != 0 {
		t.Fatalf("便宜那条按原始字节累计: %+v", r)
	}
	if r := got.Rows[1]; r.Line != 8 || r.Raw != 0 || r.Billed != 20 {
		t.Fatalf("单独设倍率的节点按节点倍率乘好: %+v", r)
	}
	// 「贵」那一项 25 小时没涨:不再报;「便宜」刚涨过留着
	s.meter.round(true, []core.LineUse{{User: "alice", Inbound: "便宜", Bytes: 1}}, ids, 1000+meterIdle+1)
	if got, _ = s.LineMeter(); len(got.Rows) != 1 || got.Rows[0].Line != 7 {
		t.Fatalf("太久没涨的一项清掉: %+v", got)
	}
	// 关掉清空;再打开换一个 Boot
	s.meter.round(false, nil, ids, 3000)
	if _, on = s.LineMeter(); on {
		t.Fatal("关掉清空")
	}
	s.meter.round(true, nil, ids, 3000)
	if got, on = s.LineMeter(); !on || got.Boot == boot || len(got.Rows) != 0 {
		t.Fatalf("再打开换一个新的 Boot、从空开始: %+v", got)
	}
}
