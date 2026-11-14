package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 套餐操作的纯计算:calcPlanOp 不碰数据库,这里逐条核对各模式的数学、护栏与折算。

const (
	pcNow = int64(1_800_000_000)
	pcDay = int64(86400)
	pcGB  = int64(1 << 30)
)

// legacyApplyPlan 改造前的 applyPlan 原样冻结在这里:续费 / 延期 N=1 时,新代码必须和它逐字段一样。
func legacyApplyPlan(u *model.User, p model.Plan, mode string, now int64) []uint {
	u.PlanId = p.Id
	u.Volume = model.GBToBytes(p.VolumeGB)
	u.DeviceLimit, u.SpeedUp, u.SpeedDown = p.DeviceLimit, p.SpeedUp, p.SpeedDown
	u.AutoReset, u.ResetDays = p.AutoReset, p.ResetDays
	base := now
	if mode != "new" && u.Expiry > now {
		base = u.Expiry
	}
	switch {
	case p.Days <= 0:
		u.Expiry, u.ActivateDays = 0, 0
	case u.ActivateDays > 0:
		u.ActivateDays += p.Days
	case p.FirstUse && u.Expiry <= now:
		u.Expiry, u.ActivateDays = 0, p.Days
	default:
		u.Expiry = base + int64(p.Days)*86400
	}
	if mode != "extend" {
		u.TotalUp += u.Up
		u.TotalDown += u.Down
		u.Up, u.Down = 0, 0
	}
	if p.AutoReset {
		if mode == "extend" && u.NextReset > now {
		} else {
			u.NextReset = now + int64(p.ResetDays)*86400
		}
	} else {
		u.NextReset = 0
	}
	u.Enabled, u.DisabledReason = true, ""
	if len(p.LineIds) > 0 {
		var ids []uint
		if json.Unmarshal(p.LineIds, &ids) == nil && len(ids) > 0 {
			return ids
		}
	}
	return nil
}

func pcUsers() map[string]model.User {
	return map[string]model.User{
		"新号":    {Id: 1, Name: "fresh", Enabled: true},
		"计时中":   {Id: 2, Name: "running", Enabled: true, Expiry: pcNow + 5*pcDay, Volume: 100 * pcGB, Up: 3 * pcGB, Down: 7 * pcGB, TotalUp: 1, TotalDown: 2, AutoReset: true, ResetDays: 30, NextReset: pcNow + 10*pcDay, DeviceLimit: 2, PlanId: 9},
		"已过期":   {Id: 3, Name: "expired", Enabled: false, DisabledReason: model.DisabledExpired, Expiry: pcNow - 3*pcDay, Volume: 50 * pcGB, Up: pcGB},
		"等首次连接": {Id: 4, Name: "waiting", Enabled: true, ActivateDays: 7, Volume: 10 * pcGB},
		"不限期":   {Id: 5, Name: "forever", Enabled: true, Volume: 0, Up: 5 * pcGB},
		"手动停用":  {Id: 6, Name: "manual", Enabled: false, DisabledReason: model.DisabledManual, Expiry: pcNow + 2*pcDay, Volume: 20 * pcGB, Down: pcGB, NextReset: pcNow - 1},
	}
}

func pcPlans() map[string]model.Plan {
	return map[string]model.Plan{
		"周期重置": {Id: 11, Name: "月付", VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30, DeviceLimit: 3, SpeedDown: 50, LineIds: []byte(`[1,2]`)},
		"一次性":  {Id: 12, Name: "流量月包", VolumeGB: 50, Days: 30, DeviceLimit: 1},
		"不限量":  {Id: 13, Name: "畅享", VolumeGB: 0, Days: 30},
		"不限期":  {Id: 14, Name: "终身", VolumeGB: 0.5, Days: 0},
		"首次连接": {Id: 15, Name: "礼品卡", VolumeGB: 20, Days: 30, FirstUse: true},
	}
}

// 续费 / 延期 N=1:和改造前的 applyPlan 逐字段一致(周期重置 / 一次性 / 不限量 / 不限期 / 首次连接 × 各种用户)。
func TestPlanOpRenewMatchesLegacy(t *testing.T) {
	for un, u := range pcUsers() {
		for pn, p := range pcPlans() {
			for _, mode := range []string{"renew", "extend"} {
				want := u
				ids := legacyApplyPlan(&want, p, mode, pcNow)
				c, err := calcPlanOp(u, nil, p, planOpOpt{Op: mode, Periods: 1}, nil, pcNow)
				if err != nil {
					t.Fatalf("%s/%s/%s: %v", un, pn, mode, err)
				}
				if !reflect.DeepEqual(c.next, want) {
					t.Errorf("%s/%s/%s 与老规则不一致:\n新 %+v\n老 %+v", un, pn, mode, c.next, want)
				}
				if (ids != nil) != (c.refs != nil) || c.reset != (mode == "renew") {
					t.Errorf("%s/%s/%s 线路或用量处理不一致: ids=%v refs=%v reset=%v", un, pn, mode, ids, c.refs, c.reset)
				}
				got := u
				gotIds := applyPlan(&got, p, mode, pcNow) // 保留下来的 N=1 包装
				if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotIds, ids) {
					t.Errorf("%s/%s/%s applyPlan 包装与老规则不一致", un, pn, mode)
				}
			}
		}
	}
}

// 续费 N 期:到期 + N×D;一次性额度 renew 的配额是 N×V,extend 仍是 V;周期重置的每期额度不叠加。
func TestPlanOpRenewPeriods(t *testing.T) {
	u := pcUsers()["计时中"]
	once, monthly := pcPlans()["一次性"], pcPlans()["周期重置"]
	c, _ := calcPlanOp(u, nil, once, planOpOpt{Op: "renew", Periods: 3}, nil, pcNow)
	if c.next.Expiry != u.Expiry+90*pcDay || c.next.Volume != 150*pcGB || c.next.Up != 0 {
		t.Fatalf("一次性 renew ×3: expiry %+d 天 volume %d GB", (c.next.Expiry-u.Expiry)/pcDay, c.next.Volume/pcGB)
	}
	c, _ = calcPlanOp(u, nil, once, planOpOpt{Op: "extend", Periods: 3}, nil, pcNow)
	if c.next.Expiry != u.Expiry+90*pcDay || c.next.Volume != 50*pcGB || c.next.Up != u.Up {
		t.Fatalf("一次性 extend ×3: 配额应仍为一期 %d GB,用量保留", c.next.Volume/pcGB)
	}
	c, _ = calcPlanOp(u, nil, monthly, planOpOpt{Op: "renew", Periods: 3}, nil, pcNow)
	if c.next.Expiry != u.Expiry+90*pcDay || c.next.Volume != 100*pcGB || c.next.NextReset != pcNow+30*pcDay {
		t.Fatalf("周期重置 renew ×3: %+v", c.next)
	}
	w := pcUsers()["等首次连接"]
	c, _ = calcPlanOp(w, nil, once, planOpOpt{Op: "renew", Periods: 3}, nil, pcNow)
	if c.next.ActivateDays != 7+90 || c.next.Expiry != 0 {
		t.Fatalf("还没开始计时的用户:等待天数 + N×D: %+v", c.next)
	}
	f := pcUsers()["已过期"]
	c, _ = calcPlanOp(f, nil, pcPlans()["首次连接"], planOpOpt{Op: "renew", Periods: 2}, nil, pcNow)
	if c.next.ActivateDays != 60 || c.next.Expiry != 0 {
		t.Fatalf("首次连接套餐、已过期:等首次连接 N×D 天: %+v", c.next)
	}
}

func hasWarn(c planCalc, code string) *planWarning {
	for i := range c.warnings {
		if c.warnings[i].Code == code {
			return &c.warnings[i]
		}
	}
	return nil
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pe *planOpError
	if !errors.As(err, &pe) || pe.code != code {
		t.Fatalf("应返回 %s,实际 %v", code, err)
	}
}

// 立即替换:从现在开始 N 期,旧的时间作废;四类目标套餐、首次连接、到期提前要 force。
func TestPlanOpReplace(t *testing.T) {
	yearly := model.Plan{Id: 21, Name: "年付", VolumeGB: 100, Days: 365, AutoReset: true, ResetDays: 30}
	u := model.User{Id: 1, Name: "u", Enabled: true, PlanId: 21, Expiry: pcNow + 200*pcDay, Volume: 100 * pcGB, Up: 4 * pcGB, Down: 6 * pcGB,
		AutoReset: true, ResetDays: 30, NextReset: pcNow + 20*pcDay, DeviceLimit: 5}
	monthly := model.Plan{Id: 22, Name: "月付", VolumeGB: 50, Days: 30, AutoReset: true, ResetDays: 30, DeviceLimit: 2}
	c, err := calcPlanOp(u, &yearly, monthly, planOpOpt{Op: "replace", Periods: 1, Usage: "auto"}, nil, pcNow)
	if err != nil {
		t.Fatal(err)
	}
	n := c.next
	if n.Expiry != pcNow+30*pcDay || n.ActivateDays != 0 || n.Volume != 50*pcGB || n.Up+n.Down != 0 || n.TotalUp+n.TotalDown != 10*pcGB ||
		n.NextReset != pcNow+30*pcDay || n.PlanId != 22 || n.DeviceLimit != 2 || c.usage != "reset" {
		t.Fatalf("年付剩 200 天换月付:从现在起 30 天,用量清零: %+v usage=%s", n, c.usage)
	}
	if w := hasWarn(c, "expiry_earlier"); w == nil || !w.Block {
		t.Fatal("到期提前应是要 force 的阻断告警")
	}
	if hasWarn(c, "volume_lower") == nil || hasWarn(c, "fewer_devices") == nil || c.direction != "downgrade" {
		t.Fatalf("降级的提示: %+v %s", c.warnings, c.direction)
	}
	// 一次性 ×2、不限量、不限期、首次连接
	once := model.Plan{Id: 23, Name: "包", VolumeGB: 10, Days: 30}
	c, _ = calcPlanOp(u, &yearly, once, planOpOpt{Op: "replace", Periods: 2}, nil, pcNow)
	if c.next.Volume != 20*pcGB || c.next.Expiry != pcNow+60*pcDay || c.next.NextReset != 0 || c.next.AutoReset {
		t.Fatalf("一次性 ×2: %+v", c.next)
	}
	c, _ = calcPlanOp(u, &yearly, model.Plan{Id: 24, Name: "畅", Days: 30}, planOpOpt{Op: "replace", Periods: 1}, nil, pcNow)
	if c.next.Volume != 0 {
		t.Fatalf("不限量: %d", c.next.Volume)
	}
	c, _ = calcPlanOp(u, &yearly, model.Plan{Id: 25, Name: "终身", VolumeGB: 1}, planOpOpt{Op: "replace", Periods: 1}, nil, pcNow)
	if c.next.Expiry != 0 || c.next.ActivateDays != 0 || hasWarn(c, "expiry_earlier") != nil {
		t.Fatalf("不限期: %+v %+v", c.next, c.warnings)
	}
	c, _ = calcPlanOp(u, &yearly, model.Plan{Id: 26, Name: "卡", VolumeGB: 1, Days: 30, FirstUse: true}, planOpOpt{Op: "replace", Periods: 3}, nil, pcNow)
	if c.next.Expiry != 0 || c.next.ActivateDays != 90 {
		t.Fatalf("首次连接:activateDays = N×D: %+v", c.next)
	}
	// 不限期换成有期限:也是到期提前
	forever := pcUsers()["不限期"]
	c, _ = calcPlanOp(forever, nil, monthly, planOpOpt{Op: "replace", Periods: 1}, nil, pcNow)
	if w := hasWarn(c, "expiry_earlier"); w == nil || w.Args["from"] != int64(0) {
		t.Fatalf("不限期变有期限应阻断: %+v", c.warnings)
	}
	// 保留用量:重置日不往后推,新周期更短时提前到 now+R
	c, _ = calcPlanOp(u, &yearly, monthly, planOpOpt{Op: "replace", Periods: 1, Usage: "keep"}, nil, pcNow)
	if c.next.NextReset != pcNow+20*pcDay || c.next.Up != u.Up {
		t.Fatalf("keep:保留当前周期 %d", (c.next.NextReset-pcNow)/pcDay)
	}
	weekly := monthly
	weekly.ResetDays = 7
	c, _ = calcPlanOp(u, &yearly, weekly, planOpOpt{Op: "replace", Periods: 1, Usage: "keep"}, nil, pcNow)
	if c.next.NextReset != pcNow+7*pcDay {
		t.Fatalf("keep 换到 7 天重置:提前到 7 天后,实际 %d 天", (c.next.NextReset-pcNow)/pcDay)
	}
	// 同一个套餐:允许,从现在重新开始
	c, err = calcPlanOp(u, &yearly, yearly, planOpOpt{Op: "replace", Periods: 1}, nil, pcNow)
	if err != nil || c.next.Expiry != pcNow+365*pcDay || c.direction != "same" {
		t.Fatalf("同套餐 replace: %v %+v %s", err, c.next, c.direction)
	}
}

// 叠加:保留旧的时间与一次性流量再加 N 期;keep / reset 两种结转、超用、不限量、周期重置不叠加、等待计时。
func TestPlanOpStack(t *testing.T) {
	once := model.Plan{Id: 31, Name: "大包", VolumeGB: 200, Days: 30}
	u := model.User{Id: 1, Name: "u", Enabled: true, Volume: 100 * pcGB, Up: 10 * pcGB, Down: 20 * pcGB, Expiry: pcNow + 10*pcDay}
	c, _ := calcPlanOp(u, nil, once, planOpOpt{Op: "stack", Periods: 1, Usage: "keep"}, nil, pcNow)
	if c.next.Volume != 300*pcGB || c.next.Up+c.next.Down != 30*pcGB || c.carry != "keep" || c.carryAdd != 200*pcGB {
		t.Fatalf("keep:旧配额 + N×V,用量照算: %d GB", c.next.Volume/pcGB)
	}
	if c.next.Expiry != pcNow+40*pcDay {
		t.Fatalf("到期 = max(now, 原到期) + N×D: %+d 天", (c.next.Expiry-pcNow)/pcDay)
	}
	c, _ = calcPlanOp(u, nil, once, planOpOpt{Op: "stack", Periods: 1, Usage: "reset"}, nil, pcNow)
	if c.next.Volume != 270*pcGB || c.next.Up+c.next.Down != 0 || c.carry != "reset" {
		t.Fatalf("reset:max(0, 旧配额 − used) + N×V = 270 GB,实际 %d GB", c.next.Volume/pcGB)
	}
	if c, _ := calcPlanOp(u, nil, once, planOpOpt{Op: "stack", Periods: 1}, nil, pcNow); c.usage != "keep" {
		t.Fatalf("stack 的 auto 是 keep: %s", c.usage)
	}
	over := u
	over.Up, over.Down = 70*pcGB, 50*pcGB // 用超了 20 GB
	c, _ = calcPlanOp(over, nil, once, planOpOpt{Op: "stack", Periods: 2, Usage: "reset"}, nil, pcNow)
	if c.next.Volume != 400*pcGB {
		t.Fatalf("超用时结转按 0 算:0 + 2×200 GB,实际 %d GB", c.next.Volume/pcGB)
	}
	// 旧配额不限:没法结转,N×V,auto 解析成 reset
	free := u
	free.Volume = 0
	c, _ = calcPlanOp(free, nil, once, planOpOpt{Op: "stack", Periods: 2}, nil, pcNow)
	if c.next.Volume != 400*pcGB || c.usage != "reset" || c.carry != "" || hasWarn(c, "carry_unlimited") == nil {
		t.Fatalf("不限量叠加到有上限: %d GB usage=%s %+v", c.next.Volume/pcGB, c.usage, c.warnings)
	}
	// 目标是周期重置:配额就是每期 V,重置日取 min,提示本期剩余不结转
	monthly := model.Plan{Id: 32, Name: "月付", VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30}
	pu := u
	pu.AutoReset, pu.ResetDays, pu.NextReset = true, 30, pcNow+5*pcDay
	c, _ = calcPlanOp(pu, nil, monthly, planOpOpt{Op: "stack", Periods: 1}, nil, pcNow)
	if c.next.Volume != 100*pcGB || c.next.NextReset != pcNow+5*pcDay || c.carry != "" {
		t.Fatalf("周期重置不叠加: %d GB, 重置 %d 天后", c.next.Volume/pcGB, (c.next.NextReset-pcNow)/pcDay)
	}
	if w := hasWarn(c, "per_period_not_stacked"); w == nil || w.Args["lost"] != 70*pcGB {
		t.Fatalf("应提示每期额度不叠加并带上丢掉的 70 GB: %+v", c.warnings)
	}
	// 还没开始计时:加到等待天数上;已过期:从现在起
	w := pcUsers()["等首次连接"]
	c, _ = calcPlanOp(w, nil, once, planOpOpt{Op: "stack", Periods: 2}, nil, pcNow)
	if c.next.ActivateDays != 67 || c.next.Expiry != 0 {
		t.Fatalf("等待计时的用户 stack: %+v", c.next)
	}
	ex := pcUsers()["已过期"]
	c, _ = calcPlanOp(ex, nil, once, planOpOpt{Op: "stack", Periods: 1}, nil, pcNow)
	if c.next.Expiry != pcNow+30*pcDay || !c.next.Enabled || c.next.DisabledReason != "" {
		t.Fatalf("已过期的从现在起算并启用: %+v", c.next)
	}
}

// 按价值折算:前提与各种 400、两类旧套餐的估值、设计文档的示例、流量包、首次连接取整、0 期。
func TestPlanOpProrate(t *testing.T) {
	monthly := model.Plan{Id: 41, Name: "月付", VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30, Price: 30}
	premium := model.Plan{Id: 42, Name: "高级月付", VolumeGB: 300, Days: 30, AutoReset: true, ResetDays: 30, Price: 60}
	u := model.User{Id: 1, Name: "u", Enabled: true, PlanId: 41, Volume: 100 * pcGB, Up: 10 * pcGB, Down: 20 * pcGB, Expiry: pcNow + 20*pcDay,
		AutoReset: true, ResetDays: 30, NextReset: pcNow + 20*pcDay}
	opt := planOpOpt{Op: "prorate", Periods: 1, Usage: "auto"}

	// 设计文档的例子:还剩 20 天,本期用了 30 GB,换高级月付 → 折 10 天,到期 now+40 天,配额 300 GB 从 0 起
	c, err := calcPlanOp(u, &monthly, premium, opt, nil, pcNow)
	if err != nil {
		t.Fatal(err)
	}
	if c.credit == nil || c.credit.Seconds != 10*pcDay || c.credit.Bytes != 0 || fmt.Sprintf("%.4f", c.credit.Ratio) != "0.6667" || fmt.Sprintf("%.2f", c.credit.Value) != "20.00" {
		t.Fatalf("折算明细: %+v", c.credit)
	}
	if c.next.Expiry != pcNow+40*pcDay || c.next.Volume != 300*pcGB || c.next.Up+c.next.Down != 0 || c.usage != "reset" {
		t.Fatalf("折算结果: %+v", c.next)
	}
	if c.remaining.TrafficRatio == nil || fmt.Sprintf("%.2f", *c.remaining.TrafficRatio) != "0.70" || c.remaining.Value == nil {
		t.Fatalf("剩余份额: %+v", c.remaining)
	}
	// 保留用量:只按时间估值(f = 1)
	c, _ = calcPlanOp(u, &monthly, premium, planOpOpt{Op: "prorate", Periods: 1, Usage: "keep"}, nil, pcNow)
	if c.credit.Seconds != 10*pcDay || c.next.Up+c.next.Down != 30*pcGB {
		t.Fatalf("keep 只按时间: %+v", c.credit)
	}
	// 本期流量几乎用完时,本期只按剩余流量的比例折
	heavy := u
	heavy.Up, heavy.Down = 50*pcGB, 40*pcGB // 剩 10% → 本期折 3 天
	c, _ = calcPlanOp(heavy, &monthly, premium, opt, nil, pcNow)
	if c.credit.Seconds != int64(1.5*float64(pcDay)) {
		t.Fatalf("本期剩 10%% 流量:3 天的价值折成高级月付 1.5 天,实际 %d 秒", c.credit.Seconds)
	}

	// 一次性旧套餐:时间与流量取小
	pack := model.Plan{Id: 43, Name: "季包", VolumeGB: 100, Days: 30, Price: 20}
	pu := model.User{Id: 2, Name: "p", Enabled: true, PlanId: 43, Volume: 100 * pcGB, Up: 80 * pcGB, Expiry: pcNow + 15*pcDay}
	big := model.Plan{Id: 44, Name: "大季包", VolumeGB: 200, Days: 30, Price: 40}
	c, _ = calcPlanOp(pu, &pack, big, opt, nil, pcNow)
	if fmt.Sprintf("%.3f", c.credit.Ratio) != "0.200" {
		t.Fatalf("一次性:min(时间 0.5, 流量 0.2) = 0.2,实际 %v", c.credit.Ratio)
	}
	// 目标是一次性额度:一期 = D 天 + V 流量,折 c 期两样都给(c = 0.2×20/40 = 0.1 → 3 天 + 20 GB)
	if c.credit.Seconds != 3*pcDay || c.credit.Bytes != 20*pcGB || c.next.Volume != 220*pcGB || c.next.Expiry != pcNow+33*pcDay {
		t.Fatalf("一次性目标的折算: %+v next=%+v", c.credit, c.next)
	}
	c, _ = calcPlanOp(pu, &pack, big, planOpOpt{Op: "prorate", Periods: 1, Usage: "keep"}, nil, pcNow)
	if fmt.Sprintf("%.3f", c.credit.Ratio) != "0.500" {
		t.Fatalf("keep 时一次性只按时间: %v", c.credit.Ratio)
	}
	// 流量包(D=0):折成字节;0 期时配额就是折出来的流量
	flow := model.Plan{Id: 45, Name: "流量包", VolumeGB: 10, Price: 10}
	c, err = calcPlanOp(pu, &pack, flow, planOpOpt{Op: "prorate", Periods: 0}, nil, pcNow)
	if err != nil || c.credit.Bytes != 4*pcGB || c.credit.Seconds != 0 || c.next.Volume != 4*pcGB || c.next.Expiry != 0 {
		t.Fatalf("流量包:0.2 × 20 / 10 = 0.4 期 = 4 GB,实际 %+v %+v %v", c.credit, c.next, err)
	}

	// 首次连接才计时的目标:折算向下取整到天,舍去的部分提示
	card := model.Plan{Id: 46, Name: "礼品月卡", Days: 30, FirstUse: true, Price: 100}
	c, _ = calcPlanOp(u, &monthly, card, opt, nil, pcNow) // 价值 20 → 0.2 期 = 6 天整
	if c.next.ActivateDays != 36 || c.next.Expiry != 0 || hasWarn(c, "credit_rounded") != nil {
		t.Fatalf("首次连接:30 + 6 天: %+v", c.next)
	}
	card.Price = 70 // 20/70 × 30 天 = 8.57 天 → 8 天,舍去 0.57 天
	c, _ = calcPlanOp(u, &monthly, card, opt, nil, pcNow)
	if c.next.ActivateDays != 38 || hasWarn(c, "credit_rounded") == nil {
		t.Fatalf("取整与提示: %+v %+v", c.next, c.warnings)
	}
	// 0 期、首次连接、折出来不足一天:直接给到期时间(不能 0 + 0 变成不限期)
	tiny := u
	tiny.Expiry, tiny.NextReset = pcNow+3600, pcNow+3600 // 只剩 1 小时
	c, err = calcPlanOp(tiny, &monthly, card, planOpOpt{Op: "prorate", Periods: 0}, nil, pcNow)
	if err != nil || c.next.ActivateDays != 0 || c.next.Expiry <= pcNow || c.next.Expiry > pcNow+pcDay {
		t.Fatalf("0 期不足一天: %+v %v", c.next, err)
	}

	// 各种 400
	_, err = calcPlanOp(u, nil, premium, opt, nil, pcNow)
	wantCode(t, err, "no_current_plan")
	other := monthly
	other.ResellerId = 9 // 不同归属的套餐不能拿来估值
	_, err = calcPlanOp(u, &other, premium, opt, nil, pcNow)
	wantCode(t, err, "no_current_plan")
	_, err = calcPlanOp(u, &monthly, monthly, opt, nil, pcNow)
	wantCode(t, err, "same_plan")
	free := premium
	free.Price = 0
	_, err = calcPlanOp(u, &monthly, free, opt, nil, pcNow)
	wantCode(t, err, "no_price")
	unpriced := monthly
	unpriced.Price = 0
	_, err = calcPlanOp(u, &unpriced, premium, opt, nil, pcNow)
	wantCode(t, err, "no_price")
	life := model.Plan{Id: 47, Name: "终身不限", Price: 10}
	lu := model.User{Id: 3, Name: "l", Enabled: true, PlanId: 47}
	_, err = calcPlanOp(lu, &life, premium, opt, nil, pcNow)
	wantCode(t, err, "unmeasurable")
	_, err = calcPlanOp(u, &monthly, model.Plan{Id: 48, Name: "不限期月重置", VolumeGB: 10, AutoReset: true, ResetDays: 30, Price: 5}, opt, nil, pcNow)
	wantCode(t, err, "unmeasurable")
	gone := u
	gone.Expiry = pcNow - pcDay // 已过期:没有剩余价值
	_, err = calcPlanOp(gone, &monthly, premium, planOpOpt{Op: "prorate", Periods: 0}, nil, pcNow)
	wantCode(t, err, "nothing_to_credit")
	if c, err := calcPlanOp(gone, &monthly, premium, opt, nil, pcNow); err != nil || c.credit.Seconds != 0 || c.next.Expiry != pcNow+30*pcDay {
		t.Fatalf("没有剩余价值但给了 1 期:照常 %v %+v", err, c.next)
	}
}

// 降级护栏:保留用量时立刻超量要 force;清零本期就不阻断。
func TestPlanOpOverQuota(t *testing.T) {
	big := model.Plan{Id: 51, Name: "大", VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30}
	small := model.Plan{Id: 52, Name: "小", VolumeGB: 50, Days: 30, AutoReset: true, ResetDays: 30}
	u := model.User{Id: 1, Name: "u", Enabled: true, PlanId: 51, Volume: 100 * pcGB, Up: 60 * pcGB, Expiry: pcNow + 20*pcDay, AutoReset: true, ResetDays: 30, NextReset: pcNow + 20*pcDay}
	c, _ := calcPlanOp(u, &big, small, planOpOpt{Op: "stack", Periods: 1, Usage: "keep"}, nil, pcNow)
	if w := hasWarn(c, "over_quota"); w == nil || !w.Block || w.Args["used"] != 60*pcGB || w.Args["volume"] != 50*pcGB {
		t.Fatalf("保留 60 GB 用量换 50 GB 每期额度应阻断: %+v", c.warnings)
	}
	c, _ = calcPlanOp(u, &big, small, planOpOpt{Op: "stack", Periods: 1, Usage: "reset"}, nil, pcNow)
	if hasWarn(c, "over_quota") != nil {
		t.Fatal("清零本期就不会超量")
	}
	// 续费 / 延期同样给出提示(是否阻断由接口决定:/renew 不阻断)
	c, _ = calcPlanOp(u, &big, small, planOpOpt{Op: "extend", Periods: 1}, nil, pcNow)
	if hasWarn(c, "over_quota") == nil {
		t.Fatal("extend 到更小的套餐也要提示超量")
	}
}

// 升降级判定:按每 30 天流量、设备、上下行比较,0 视为不限。
func TestPlanOpDirection(t *testing.T) {
	base := model.Plan{VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30, DeviceLimit: 3}
	cases := []struct {
		name string
		to   model.Plan
		want string
	}{
		{"更多流量", model.Plan{VolumeGB: 200, Days: 30, AutoReset: true, ResetDays: 30, DeviceLimit: 3}, "upgrade"},
		{"不限设备", model.Plan{VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30}, "upgrade"},
		{"更少设备", model.Plan{VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30, DeviceLimit: 1}, "downgrade"},
		{"加了限速", model.Plan{VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30, DeviceLimit: 3, SpeedDown: 10}, "downgrade"},
		{"季付同价位", model.Plan{VolumeGB: 300, Days: 90, DeviceLimit: 3}, "same"},
		{"多流量少设备", model.Plan{VolumeGB: 500, Days: 30, AutoReset: true, ResetDays: 30, DeviceLimit: 1}, "mixed"},
		{"不限量", model.Plan{Days: 30, DeviceLimit: 3}, "upgrade"},
	}
	for _, c := range cases {
		if got := directionOf(tierOfPlan(base), tierOfPlan(c.to)); got != c.want {
			t.Errorf("%s: 应为 %s,实际 %s", c.name, c.want, got)
		}
	}
	// 没有旧套餐:用用户现在的字段比
	u := model.User{Volume: 100 * pcGB, AutoReset: true, ResetDays: 30, DeviceLimit: 3}
	if got := directionOf(tierOfCurrent(u, nil), tierOfPlan(base)); got != "same" {
		t.Fatalf("没有旧套餐时按用户字段: %s", got)
	}
}

// 参数校验:期数、用量处理。
func TestPlanOpParams(t *testing.T) {
	p := func(n int) *int { return &n }
	for _, c := range []struct {
		n    *int
		op   string
		days int
		ok   bool
	}{
		{nil, "renew", 30, true}, {p(1), "renew", 30, true}, {p(120), "renew", 30, true}, {p(0), "renew", 30, false}, {p(121), "stack", 30, false},
		{p(0), "prorate", 30, true}, {p(-1), "prorate", 30, false}, {p(100), "renew", 365, true}, {p(101), "renew", 365, false},
		{p(1), "renew", 99999, true}, // 一期不受总天数限制(和以前一样能续)
	} {
		_, err := planPeriods(c.n, c.op, c.days)
		if (err == nil) != c.ok {
			t.Errorf("periods=%v op=%s days=%d: %v", c.n, c.op, c.days, err)
		}
	}
	for in, want := range map[string]string{"": "auto", "AUTO": "auto", "keep": "keep", " reset ": "reset"} {
		if got, err := planUsage(in); err != nil || got != want {
			t.Errorf("usage %q → %q %v", in, got, err)
		}
	}
	if _, err := planUsage("clear"); err == nil {
		t.Fatal("未知的 usage 应被拒")
	}
}
