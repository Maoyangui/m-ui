package hub

import (
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 节点单独设了倍率(model.NodeOpt.Ratio):副机账本里多带四个附带数 —— o = 走了这些节点的原始字节(已含在 up / down 里)、
// x = 这部分按各自节点倍率乘好的字节。主机并账 = (增量 − o 的增量) × 服务器倍率 + x 的增量。

type ratioRig struct {
	*recreateRig
	u model.User
}

func newRatioRig(t *testing.T) *ratioRig {
	r := &ratioRig{recreateRig: newRecreateRig(t)}
	r.u = r.newUser(1000)
	r.sync()
	return r
}

// add 副机这一轮记了 alice:上行 up(其中 o 走了单独设倍率的节点,x 是乘好的),下行都是上行的两倍。
func (r *ratioRig) add(up, o, x int64) {
	r.t.Helper()
	row := model.AgentCounter{UserName: "alice", Up: up, Down: up * 2, OUp: o, ODown: o * 2, XUp: x, XDown: x * 2}
	if err := model.AddAgentCounters(r.agent, []model.AgentCounter{row}, 100); err != nil {
		r.t.Fatal(err)
	}
}

func (r *ratioRig) merge(at int64, ratio float64) MergeResult {
	r.t.Helper()
	var cs []model.AgentCounter
	if err := r.agent.Find(&cs).Error; err != nil {
		r.t.Fatal(err)
	}
	res, err := MergeCounters(r.host, 2, "n2", "", cs, at, 60, ratio)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func (r *ratioRig) used() (up, down int64) {
	r.t.Helper()
	var u model.User
	if err := r.host.First(&u, r.u.Id).Error; err != nil {
		r.t.Fatal(err)
	}
	return u.Up, u.Down
}

func (r *ratioRig) want(step string, up int64) {
	r.t.Helper()
	gotUp, gotDown := r.used()
	if gotUp != up || gotDown != up*2 {
		r.t.Fatalf("%s:用量应是 上行 %d / 下行 %d,实际 %d / %d", step, up, up*2, gotUp, gotDown)
	}
}

func TestMergeNodeRatio(t *testing.T) {
	r := newRatioRig(t)
	// 第一份(没有游标,从 0 计):1000 字节里 400 走了倍率 3 的节点(x = 1200),服务器倍率 2 → (1000−400)×2 + 1200 = 2400
	r.add(1000, 400, 1200)
	r.merge(1000, 2)
	r.want("第一份", 2400)
	// 按增量:新来的 500 全走了倍率 0.5 的节点(x = 250)→ 0×2 + 250
	r.add(500, 500, 250)
	r.merge(1060, 2)
	r.want("全走单独倍率的节点", 2650)
	// 新来的 100 都是普通流量 → 100×2
	r.add(100, 0, 0)
	r.merge(1120, 2)
	r.want("普通流量照服务器倍率", 2850)
	// 同一份再报一遍:什么都不计
	r.merge(1180, 2)
	r.want("重复的报告", 2850)
	// 游标把四个附带数也记下了
	var cur model.TrafficCursor
	if err := r.host.Where("node_id = ? AND user_name = ?", 2, "alice").First(&cur).Error; err != nil {
		t.Fatal(err)
	}
	if cur.Up != 1600 || cur.OUp != 900 || cur.XUp != 1450 || cur.ODown != 1800 || cur.XDown != 2900 {
		t.Fatalf("游标没有跟上附带数:%+v", cur)
	}
	// 服务器维度的时序和用户用量是同一个数
	var nodeUp int64
	r.host.Model(&model.Stats{}).Where("resource = ? AND tag = ? AND direction = ?", "node", "n2", true).Select("COALESCE(SUM(traffic), 0)").Scan(&nodeUp)
	if nodeUp != 2850 {
		t.Fatalf("服务器维度的上行时序应是 2850,实际 %d", nodeUp)
	}
}

// 服务器倍率改了:普通流量之后按新倍率,走了单独倍率节点的那部分不受影响。
func TestMergeNodeRatioServerRatioChange(t *testing.T) {
	r := newRatioRig(t)
	r.add(1000, 400, 1200)
	r.merge(1000, 2)
	r.want("倍率 2", 2400)
	r.add(1000, 400, 1200)
	r.merge(1060, 5)
	r.want("倍率改成 5", 2400+600*5+1200)
}

// 老副机不报附带数(都是 0):和以前一样全按服务器倍率。
func TestMergeNodeRatioOldAgent(t *testing.T) {
	r := newRatioRig(t)
	r.add(1000, 0, 0)
	r.merge(1000, 3)
	r.want("老副机", 3000)
}

// 账本回退(同一本账计数变小):对齐不计,附带数的游标也一起对齐;之后照常按增量。
func TestMergeNodeRatioRegress(t *testing.T) {
	r := newRatioRig(t)
	r.add(1000, 400, 1200)
	r.merge(1000, 2)
	r.want("回退之前", 2400)
	if err := r.agent.Model(&model.AgentCounter{}).Where("user_name = ?", "alice").
		Updates(map[string]interface{}{"up": 600, "down": 1200, "o_up": 200, "o_down": 400, "x_up": 600, "x_down": 1200}).Error; err != nil {
		t.Fatal(err)
	}
	if res := r.merge(1060, 2); res.Regressed != 1 {
		t.Fatalf("应按账本回退处理,实际 %+v", res)
	}
	r.want("回退的这一份不计", 2400)
	r.add(100, 100, 300)
	r.merge(1120, 2)
	r.want("回退之后按增量", 2700)
}

// 附带数不合理(o 比这一段的增量还大、x 是负的):往保守的一边取,不能把这一段算成负数。
func TestChargeOf(t *testing.T) {
	for _, c := range []struct {
		d, o, x int64
		ratio   float64
		want    int64
	}{
		{1000, 0, 0, 1, 1000},
		{1000, 0, 0, 2.5, 2500},
		{1000, 400, 1200, 2, 2400},
		{1000, 1000, 500, 2, 500},
		{100, 2000, 50, 2, 50}, // o 比增量大:按整段都走了单独倍率的节点算
		{100, 0, 999, 2, 200},  // 没有 o 却有 x:x 不算
		{100, -5, 10, 2, 200},  // o 是负的:当 0
		{100, 50, -10, 2, 100}, // x 是负的:当 0
		{0, 10, 10, 2, 0},      // 这一段没有流量
		{1000, 400, 1200, 0.5, 1500},
	} {
		if got := chargeOf(c.d, c.o, c.x, c.ratio); got != c.want {
			t.Errorf("chargeOf(%d, %d, %d, %v) = %d,应是 %d", c.d, c.o, c.x, c.ratio, got, c.want)
		}
	}
}
