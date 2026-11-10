package web

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 叠加(stack)的两条边界规则与几条提示 / 错误文案。

// 叠加到已经过期的订阅:旧订阅剩的流量随过期作废,按 0 结转(和折算的估值口径一致:过期的剩余份额是 0),
// 预览里有 carry_expired 提示带上作废的量;没过期的照旧结转。
func TestPlanOpStackExpiredCarriesNothing(t *testing.T) {
	once := model.Plan{Id: 31, Name: "大包", VolumeGB: 200, Days: 30}
	u := model.User{Id: 1, Name: "u", Enabled: false, DisabledReason: model.DisabledExpired, Volume: 100 * pcGB, Up: 10 * pcGB, Down: 20 * pcGB,
		Expiry: pcNow - 5*pcDay}
	c, err := calcPlanOp(u, nil, once, planOpOpt{Op: "stack", Periods: 1, Usage: "keep"}, nil, pcNow)
	if err != nil {
		t.Fatal(err)
	}
	// 保留用量:已用的 30 GB 照算,新的一份 200 GB 全额可用 → 配额 230 GB;到期从现在起算
	if c.next.Volume != 230*pcGB || c.next.Up+c.next.Down != 30*pcGB || c.next.Expiry != pcNow+30*pcDay {
		t.Fatalf("过期后叠加(保留用量): %d GB, 到期 %+d 天", c.next.Volume/pcGB, (c.next.Expiry-pcNow)/pcDay)
	}
	if w := hasWarn(c, "carry_expired"); w == nil || w.Args["lost"] != 70*pcGB || w.Block {
		t.Fatalf("应提示作废的 70 GB: %+v", c.warnings)
	}
	if c.remaining.Bytes == nil || *c.remaining.Bytes != 0 {
		t.Fatalf("过期订阅的剩余流量按 0 估: %+v", c.remaining)
	}
	c, _ = calcPlanOp(u, nil, once, planOpOpt{Op: "stack", Periods: 2, Usage: "reset"}, nil, pcNow)
	if c.next.Volume != 400*pcGB || c.next.Up+c.next.Down != 0 || c.carry != "" {
		t.Fatalf("过期后叠加(清零):只有新的 2 × 200 GB: %d GB", c.next.Volume/pcGB)
	}
	alive := u
	alive.Expiry = pcNow + pcDay
	c, _ = calcPlanOp(alive, nil, once, planOpOpt{Op: "stack", Periods: 1, Usage: "keep"}, nil, pcNow)
	if c.next.Volume != 300*pcGB || hasWarn(c, "carry_expired") != nil {
		t.Fatalf("没过期的照旧结转: %d GB %+v", c.next.Volume/pcGB, c.warnings)
	}
}

// 叠加到不限期 / 不限量的套餐:有限的到期、配额不会悄悄变成不限,原样保留并提示(要改成不限用立即替换);
// 原来就不限的照旧不限;已经过期的订阅没有可保留的东西,等于从现在开一份新的,按目标套餐来。
func TestPlanOpStackKeepsFiniteLimits(t *testing.T) {
	u := model.User{Id: 1, Name: "u", Enabled: true, Volume: 100 * pcGB, Up: 30 * pcGB, Expiry: pcNow + 10*pcDay}
	pack := model.Plan{Id: 41, Name: "加油包", VolumeGB: 50} // 不限期的一次性流量包
	free := model.Plan{Id: 42, Name: "畅享", Days: 30}      // 不限量 30 天

	c, _ := calcPlanOp(u, nil, pack, planOpOpt{Op: "stack", Periods: 1}, nil, pcNow)
	if c.next.Expiry != u.Expiry || c.next.ActivateDays != 0 || c.next.Volume != 150*pcGB {
		t.Fatalf("加油包:到期不变、流量加 50 GB: %+v", c.next)
	}
	if w := hasWarn(c, "expiry_kept"); w == nil || w.Block || w.Args["expiry"] != u.Expiry {
		t.Fatalf("应提示到期保留: %+v", c.warnings)
	}
	c, _ = calcPlanOp(u, nil, free, planOpOpt{Op: "stack", Periods: 1}, nil, pcNow)
	if c.next.Volume != 100*pcGB || c.next.Expiry != u.Expiry+30*pcDay || c.next.Up != u.Up {
		t.Fatalf("畅享:配额保留 100 GB、时间加 30 天: %+v", c.next)
	}
	if w := hasWarn(c, "volume_kept"); w == nil || w.Block || w.Args["left"] != 70*pcGB {
		t.Fatalf("应提示配额保留: %+v", c.warnings)
	}
	c, _ = calcPlanOp(u, nil, free, planOpOpt{Op: "stack", Periods: 1, Usage: "reset"}, nil, pcNow)
	if c.next.Volume != 70*pcGB || c.next.Up+c.next.Down != 0 {
		t.Fatalf("清零用量时保留剩下的 70 GB: %d", c.next.Volume)
	}
	spent := u
	spent.Up = 100 * pcGB
	c, _ = calcPlanOp(spent, nil, free, planOpOpt{Op: "stack", Periods: 1, Usage: "reset"}, nil, pcNow)
	if c.next.Volume != 1 {
		t.Fatalf("剩 0 也不能写成 0(0 = 不限): %d", c.next.Volume)
	}
	// 原来就不限:照旧
	inf := model.User{Id: 2, Name: "inf", Enabled: true}
	if c, _ := calcPlanOp(inf, nil, free, planOpOpt{Op: "stack", Periods: 1}, nil, pcNow); c.next.Volume != 0 || hasWarn(c, "volume_kept") != nil {
		t.Fatalf("不限量的照旧不限: %+v", c.next)
	}
	if c, _ := calcPlanOp(inf, nil, pack, planOpOpt{Op: "stack", Periods: 1}, nil, pcNow); c.next.Expiry != 0 || hasWarn(c, "expiry_kept") != nil {
		t.Fatalf("不限期的照旧不限: %+v", c.next)
	}
	// 还没开始计时:等待的天数不动
	w := model.User{Id: 3, Name: "w", Enabled: true, ActivateDays: 7, Volume: 10 * pcGB}
	if c, _ := calcPlanOp(w, nil, pack, planOpOpt{Op: "stack", Periods: 1}, nil, pcNow); c.next.ActivateDays != 7 || c.next.Expiry != 0 || hasWarn(c, "expiry_kept") == nil {
		t.Fatalf("等首次连接的天数保留: %+v", c.next)
	}
	// 已过期:旧的作废,按目标套餐来
	ex := u
	ex.Expiry = pcNow - pcDay
	if c, _ := calcPlanOp(ex, nil, pack, planOpOpt{Op: "stack", Periods: 1}, nil, pcNow); c.next.Expiry != 0 || c.next.Volume != 80*pcGB || hasWarn(c, "expiry_kept") != nil {
		t.Fatalf("过期后叠加加油包:不限期,已用 30 + 新 50: %+v %+v", c.next, c.warnings)
	}
	if c, _ := calcPlanOp(ex, nil, free, planOpOpt{Op: "stack", Periods: 1}, nil, pcNow); c.next.Volume != 0 || hasWarn(c, "volume_kept") != nil {
		t.Fatalf("过期后叠加畅享:不限量: %+v", c.next)
	}
	// 立即替换照旧按新套餐(不限就是不限)
	if c, _ := calcPlanOp(u, nil, pack, planOpOpt{Op: "replace", Periods: 1}, nil, pcNow); c.next.Expiry != 0 {
		t.Fatalf("replace 到不限期的套餐: %+v", c.next)
	}
}

// 正式执行落库的结果与预览一致(上面这些情形的配额在 SQL 里结转)。
func TestPlanOpStackExecutionMatchesPreview(t *testing.T) {
	e := newPCEnv(t)
	now := time.Now().Unix()
	e.mkPlan(model.Plan{Name: "大包", VolumeGB: 200, Days: 30})
	e.mkPlan(model.Plan{Name: "加油包", VolumeGB: 50})
	e.mkPlan(model.Plan{Name: "畅享", Days: 30})
	cases := []struct {
		name, body string
		user       model.User
	}{
		{"过期保留", `{"plan":"大包","mode":"stack","usage":"keep"}`, model.User{Volume: 100 << 30, Up: 30 << 30, Expiry: now - 86400}},
		{"过期清零", `{"plan":"大包","mode":"stack","usage":"reset","periods":2}`, model.User{Volume: 100 << 30, Up: 30 << 30, Expiry: now - 86400}},
		{"加油包", `{"plan":"加油包","mode":"stack"}`, model.User{Volume: 100 << 30, Up: 30 << 30, Expiry: now + 10*86400}},
		{"畅享保留", `{"plan":"畅享","mode":"stack","usage":"keep"}`, model.User{Volume: 100 << 30, Up: 30 << 30, Expiry: now + 10*86400}},
		{"畅享清零", `{"plan":"畅享","mode":"stack","usage":"reset"}`, model.User{Volume: 100 << 30, Up: 30 << 30, Expiry: now + 10*86400}},
		{"畅享用完", `{"plan":"畅享","mode":"stack","usage":"reset"}`, model.User{Volume: 100 << 30, Up: 120 << 30, Expiry: now + 10*86400}},
	}
	for i, c := range cases {
		name := "u" + strconv.Itoa(i)
		c.user.Name = name
		e.mkUser(c.user)
		pre := pcResult(t, e.api("POST", "users/"+name+"/change?dryRun=true", c.body))
		w := e.api("POST", "users/"+name+"/change", strings.Replace(c.body, "}", `,"force":true}`, 1))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
		u := e.user(name)
		if u.Volume != pre.To.Volume || u.Expiry != pre.To.Expiry || u.ActivateDays != pre.To.ActivateDays || u.Up+u.Down != pre.To.Used {
			t.Fatalf("%s:落库 %+v 与预览 %+v 不一致", c.name, u, pre.To)
		}
		if u.Volume == 0 && c.user.Expiry > now {
			t.Fatalf("%s:有限的配额不能变成不限", c.name)
		}
	}
}

// 提示与错误的文案要和实际情况一致。
func TestPlanOpWarningTexts(t *testing.T) {
	// over_quota:一次性额度的套餐没有"下次重置",不能说"直到下次重置";周期重置的带上重置时间
	once := model.Plan{Id: 51, Name: "小包", VolumeGB: 10, Days: 30}
	u := model.User{Id: 1, Name: "u", Enabled: true, Volume: 100 * pcGB, Up: 50 * pcGB, Expiry: pcNow + 10*pcDay}
	c, _ := calcPlanOp(u, nil, once, planOpOpt{Op: "replace", Periods: 1, Usage: "keep"}, nil, pcNow)
	if w := hasWarn(c, "over_quota"); w == nil || !w.Block || w.Args["nextReset"] != int64(0) || strings.Contains(w.Text, "重置") {
		t.Fatalf("一次性额度的超量提示: %+v", c.warnings)
	}
	monthly := model.Plan{Id: 52, Name: "月付", VolumeGB: 10, Days: 30, AutoReset: true, ResetDays: 30}
	c, _ = calcPlanOp(u, nil, monthly, planOpOpt{Op: "replace", Periods: 1, Usage: "keep"}, nil, pcNow)
	if w := hasWarn(c, "over_quota"); w == nil || w.Args["nextReset"] != c.next.NextReset || c.next.NextReset <= pcNow || !strings.Contains(w.Text, "重置") {
		t.Fatalf("周期重置的超量提示: %+v", c.warnings)
	}
	// carry_unlimited:原来不限量,新配额就是新套餐给的 N 期流量(不是"从 0 起算");保留用量时已用量照算
	inf := model.User{Id: 2, Name: "inf", Enabled: true, Up: 5 * pcGB, Expiry: pcNow + pcDay}
	c, _ = calcPlanOp(inf, nil, once, planOpOpt{Op: "stack", Periods: 2, Usage: "keep"}, nil, pcNow)
	if w := hasWarn(c, "carry_unlimited"); w == nil || w.Args["volume"] != 20*pcGB || strings.Contains(w.Text, "从 0") {
		t.Fatalf("carry_unlimited: %+v", c.warnings)
	}
	// unmeasurable:用户有配额、只是不限期,保留用量时只按时间估值 —— 不能说成"不限期也不限量"
	pack := model.Plan{Id: 53, Name: "流量包", VolumeGB: 100, Price: 10}
	target := model.Plan{Id: 54, Name: "月付", VolumeGB: 100, Days: 30, Price: 20}
	pu := model.User{Id: 3, Name: "p", Enabled: true, PlanId: 53, Volume: 100 * pcGB, Up: 10 * pcGB}
	_, err := calcPlanOp(pu, &pack, target, planOpOpt{Op: "prorate", Periods: 1, Usage: "keep"}, nil, pcNow)
	wantCode(t, err, "unmeasurable")
	if strings.Contains(err.Error(), "不限期也不限量") || !strings.Contains(err.Error(), "保留用量") {
		t.Fatalf("unmeasurable 的文案: %v", err)
	}
	if c, err := calcPlanOp(pu, &pack, target, planOpOpt{Op: "prorate", Periods: 1, Usage: "reset"}, nil, pcNow); err != nil || c.credit == nil {
		t.Fatalf("清零用量时按流量能折: %v", err)
	}
}
