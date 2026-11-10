package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/jobs"
	"github.com/Maoyangui/m-ui/render"

	"gorm.io/gorm"
)

// ---- 套餐操作:续费 N 期、换套餐(立即替换 / 叠加 / 按价值折算)、预览、幂等 ----
//
// 面板的续费、批量续费、换套餐,外部 API 的 /plan、/renew、/change 都走这里:
//
//	calcPlanOp  纯函数,只算:变更后的用户、要写的线路、用量怎么处理、折算与告警。预览和正式执行用同一个。
//	runPlanOp   执行器:一个 BEGIN IMMEDIATE 事务里依次查幂等键 → 重新读用户 → 核对 fromPlanId → 计算 → 护栏
//	            → 写用户(用量列用 SQL 原子结算)、线路、套餐记录、审计。事务体里只能用 tx:用 s.db 写会向第二条连接要写锁,
//	            一直等到 busy_timeout(10 秒)才失败。
//
// 记号:N 期数;目标套餐 Q 的天数 D、每期流量 V、重置天数 R;used = up + down。
// 「一次性额度」= 不周期重置且有上限,配额是整段时长的存量;「周期重置」的流量是每期额度,不叠加。

const (
	maxPlanPeriods  = 120   // 一次最多续 / 换 120 期
	maxPlanDays     = 36500 // 期数 × 套餐天数不超过约 100 年
	maxCreditDays   = 3650  // 折算出来的时长封顶
	earlierSlack    = 60    // 到期只早这么几秒不算"提前"(预览到执行之间的时间差)
	planRemarkMax   = 200   // 套餐记录的备注最多这么多字
	planOpMasterTag = "主面板"
)

// maxVolume 配额的饱和上限:套餐的 GB 数是浮点,× 期数可能溢出 int64。
const maxVolume = int64(1) << 62

var (
	errPlanRequired     = errors.New("缺少 planId 或 plan")
	errPlanNoUserPlan   = errors.New("用户没有所属套餐,请给 planId 或 plan")
	errPlanBadMode      = errors.New("mode 只能是 replace / stack / prorate")
	errPlanBadPeriods   = errors.New("期数需在 1 到 120 之间(折算可为 0),且总天数不超过 36500")
	errPlanBadUsage     = errors.New("usage 只能是 auto / keep / reset")
	errPlanBadRequestID = errors.New("requestId 最长 64 个字符,只能含字母、数字和 . _ : -")
	errPlanNoCurrent    = errors.New("用户没有所属套餐,不能按价值折算")
	errPlanSame         = errors.New("目标套餐就是当前套餐,不能折算:要从现在重新开始用 replace,要带结转续费用 stack")
	errPlanUnmeasurable = errors.New("当前订阅不限期也不限量,剩余价值没法估算")
	errPlanNoTimeOrPack = errors.New("目标套餐不限期又不是一次性流量,折算结果没法计入")
	errPlanNothing      = errors.New("没有可折算的剩余价值,期数不能为 0")
	errPlanNeedsForce   = errors.New("这次变更有需要确认的影响(见 warnings),确认后带 force: true 重试")
	errPlanChanged      = errors.New("用户当前套餐与 fromPlanId 不符")
	errPlanIdemConflict = errors.New("requestId 已用于另一笔不同的请求")
	errPlanUserGone     = errors.New("用户不存在")
)

// planOpError 带 code 的错误:接口返回 {"error": 中文, "code": …};409 needs_force 还附带 warnings 与 preview。
type planOpError struct {
	status  int
	code    string
	err     error
	preview *planResult
}

func (e *planOpError) Error() string { return e.err.Error() }
func (e *planOpError) Unwrap() error { return e.err }

func opErr(status int, code string, err error) error {
	return &planOpError{status: status, code: code, err: err}
}

// writePlanErr 输出套餐操作的错误;不是 planOpError 的(数据库错误之类)按 400 原样给。
func writePlanErr(w http.ResponseWriter, err error) {
	var pe *planOpError
	if !errors.As(err, &pe) {
		badRequest(w, err)
		return
	}
	out := map[string]interface{}{"error": pe.Error()}
	if pe.code != "" {
		out["code"] = pe.code
	}
	if pe.preview != nil {
		out["warnings"] = pe.preview.Warnings
		out["preview"] = pe.preview
	}
	writeJSON(w, pe.status, out)
}

// ---- 结果与预览 ----

// planOpOpt 一次操作的参数(已校验)。Usage 只对 replace / stack / prorate 有意义:auto | keep | reset。
type planOpOpt struct {
	Op      string // renew | extend | replace | stack | prorate
	Periods int
	Usage   string
}

// planWarning 一条影响提示:code 固定(界面按它翻译),text 是给人看的中文,args 是数字参数;block = 要带 force 才执行。
type planWarning struct {
	Code  string                 `json:"code"`
	Text  string                 `json:"text"`
	Args  map[string]interface{} `json:"args,omitempty"`
	Block bool                   `json:"block,omitempty"`
}

var planWarnText = map[string]string{
	"over_quota":             "保留的本周期已用量已达到新配额,下一轮判定就会停用,直到下次重置",
	"expiry_earlier":         "变更后的到期早于现在的到期",
	"volume_lower":           "配额变少",
	"fewer_devices":          "设备数上限变少",
	"slower":                 "限速变低",
	"lines_removed":          "按套餐替换线路后,用户会少几条线路",
	"per_period_not_stacked": "周期重置套餐的流量是每期额度,叠加只延长时间",
	"carry_unlimited":        "原来不限量,没有剩余流量可以结转,新配额从 0 起算",
	"credit_rounded":         "目标套餐从首次连接开始计时(按整天算),折算出来不足一天的部分舍去",
	"enables_manual":         "这个用户是手动停用的,执行后会被启用",
	"reseller_pool_low":      "变更后用户的剩余配额多于代理剩余的流量额度",
	"reseller_expires_first": "变更后的到期晚于代理自己的到期,代理到期时用户一并停用",
}

// planWarnBlock 这两种会立刻伤到用户(下一轮就停用 / 少了时间),/change 正式执行时要带 force。
var planWarnBlock = map[string]bool{"over_quota": true, "expiry_earlier": true}

func newPlanWarning(code string, args map[string]interface{}) planWarning {
	return planWarning{Code: code, Text: planWarnText[code], Args: args, Block: planWarnBlock[code]}
}

// planState 变更前 / 后的套餐状态(预览、套餐记录、重放都用它)。
type planState struct {
	PlanId         uint            `json:"planId"`
	Plan           string          `json:"plan"`
	Volume         int64           `json:"volume"`
	Used           int64           `json:"used"`
	Expiry         int64           `json:"expiry"`
	ActivateDays   int             `json:"activateDays"`
	AutoReset      bool            `json:"autoReset"`
	ResetDays      int             `json:"resetDays"`
	NextReset      int64           `json:"nextReset"`
	DeviceLimit    int             `json:"deviceLimit"`
	SpeedUp        int             `json:"speedUp"`
	SpeedDown      int             `json:"speedDown"`
	Enabled        bool            `json:"enabled"`
	DisabledReason string          `json:"disabledReason"`
	LineRefs       []model.LineRef `json:"lineRefs"`
}

func stateOf(u model.User, plan string, refs []model.LineRef) planState {
	if refs == nil {
		refs = []model.LineRef{}
	}
	return planState{PlanId: u.PlanId, Plan: plan, Volume: u.Volume, Used: u.Up + u.Down, Expiry: u.Expiry, ActivateDays: u.ActivateDays,
		AutoReset: u.AutoReset, ResetDays: u.ResetDays, NextReset: u.NextReset, DeviceLimit: u.DeviceLimit, SpeedUp: u.SpeedUp, SpeedDown: u.SpeedDown,
		Enabled: u.Enabled, DisabledReason: u.DisabledReason, LineRefs: refs}
}

// planRemaining 旧订阅还剩多少:秒、字节,以及按旧套餐折成的份额(ratio = 还值几期);null = 不限或无从估算。
// value = ratio × 旧套餐参考价,只有旧套餐填了参考价才有:商城可以拿它按自己的价目表算差价,再调 replace。
type planRemaining struct {
	Seconds      *int64   `json:"seconds"`
	Bytes        *int64   `json:"bytes"`
	TimeRatio    *float64 `json:"timeRatio"`
	TrafficRatio *float64 `json:"trafficRatio"`
	Ratio        *float64 `json:"ratio"`
	Value        *float64 `json:"value,omitempty"`
}

// planCredit 按价值折算的明细:旧订阅剩余价值 value = ratio × fromPrice,折成新套餐 periods = value ÷ toPrice 期,
// 再换成时长(seconds)与一次性流量(bytes)。
type planCredit struct {
	FromPrice float64 `json:"fromPrice"`
	ToPrice   float64 `json:"toPrice"`
	Ratio     float64 `json:"ratio"`
	Value     float64 `json:"value"`
	Periods   float64 `json:"periods"`
	Seconds   int64   `json:"seconds"`
	Bytes     int64   `json:"bytes"`
}

// planPool 代理用户才有:代理的流量额度、已用(全时用量口径)、剩余;volume 0 = 不限。
type planPool struct {
	Volume    int64 `json:"volume"`
	Used      int64 `json:"used"`
	Remaining int64 `json:"remaining"`
}

// planResult 一次套餐操作的结果 / 预览。
type planResult struct {
	Op         string        `json:"op"`
	Direction  string        `json:"direction"`
	Periods    int           `json:"periods"`
	Usage      string        `json:"usage"`
	DryRun     bool          `json:"dryRun"`
	Replayed   bool          `json:"replayed"`
	OpId       uint64        `json:"opId,omitempty"`
	RequestId  string        `json:"requestId,omitempty"`
	From       planState     `json:"from"`
	To         planState     `json:"to"`
	Remaining  planRemaining `json:"remaining"`
	Credit     *planCredit   `json:"credit"`
	Pool       *planPool     `json:"pool,omitempty"`
	Warnings   []planWarning `json:"warnings"`
	NeedsForce bool          `json:"needsForce"`
	User       *apiUserView  `json:"user,omitempty"`

	calc planCalc // 执行器落库用,不出接口
}

// ---- 计算 ----

// planCalc calcPlanOp 的结果。next 里的用量已按"清零与否"预测好;stack 的配额落库时用 SQL 原子结转(carry)。
type planCalc struct {
	next      model.User
	reset     bool            // 本周期用量并入历史、清零
	usage     string          // 实际的用量处理:keep | reset
	carry     string          // stack 结转一次性额度:"" | keep(volume + N×V)| reset(MAX(0, volume - used) + N×V)
	carryAdd  int64           // 结转时加上的 N×V
	refs      []model.LineRef // 套餐指定的线路;nil = 不改
	direction string
	remaining planRemaining
	credit    *planCredit
	warnings  []planWarning
}

func (c *planCalc) warn(code string, args map[string]interface{}) {
	c.warnings = append(c.warnings, newPlanWarning(code, args))
}

// calcPlanOp 算一次套餐操作,不碰数据库。u 是变更前的用户,old 是他现在的套餐(没有为 nil),q 是目标套餐,
// fromRefs 是他现在的线路。renew / extend 与 applyPlanN 同一套规则(N = 1 时与老的 applyPlan 逐字段一样)。
func calcPlanOp(u model.User, old *model.Plan, q model.Plan, o planOpOpt, fromRefs []model.LineRef, now int64) (planCalc, error) {
	n := o.Periods
	v := model.GBToBytes(q.VolumeGB)
	if v < 0 {
		v = 0
	}
	used := u.Up + u.Down
	c := planCalc{next: u}
	next := &c.next

	switch o.Op {
	case "renew", "extend":
		c.reset = o.Op == "renew"
		if applyPlanN(next, q, o.Op, n, now) != nil {
			c.refs = planRefs(q)
		}
	case "replace", "stack", "prorate":
		usage := o.Usage
		if usage == "" || usage == "auto" {
			usage = "reset" // replace / prorate:新套餐从现在开始,本期用量清零
			if o.Op == "stack" {
				usage = "keep" // 叠加:接着用;原来不限量换到有上限时没法结转,从 0 起
				if u.Volume == 0 && v > 0 {
					usage = "reset"
				}
			}
		}
		c.reset = usage == "reset"
		next.PlanId = q.Id
		next.DeviceLimit, next.SpeedUp, next.SpeedDown = q.DeviceLimit, q.SpeedUp, q.SpeedDown
		next.AutoReset, next.ResetDays = q.AutoReset, q.ResetDays
		days := n * q.Days
		switch o.Op {
		case "replace": // 从现在开始算 N 期,旧订阅剩的时间作废
			switch {
			case q.Days <= 0:
				next.Expiry, next.ActivateDays = 0, 0
			case q.FirstUse:
				next.Expiry, next.ActivateDays = 0, days
			default:
				next.Expiry, next.ActivateDays = now+int64(days)*86400, 0
			}
		case "stack": // 和续费一样顺延:保留旧订阅剩的时间再加 N 期
			extendByPlan(next, q, days, now, true)
		} // prorate 的到期要等折算出来(见下)
		switch {
		case v == 0:
			next.Volume = 0
		case q.AutoReset:
			next.Volume = v // 每期额度,不叠加
		case o.Op == "stack" && u.Volume > 0:
			c.carry, c.carryAdd = usage, satMul(v, n)
			if c.reset {
				next.Volume = satAdd(max(0, u.Volume-used), c.carryAdd)
			} else {
				next.Volume = satAdd(u.Volume, c.carryAdd)
			}
		default:
			next.Volume = satMul(v, n) // prorate 折出来的流量后面再加
		}
		if c.reset {
			next.TotalUp += next.Up
			next.TotalDown += next.Down
			next.Up, next.Down = 0, 0
		}
		if q.AutoReset {
			r := now + int64(q.ResetDays)*86400
			if !c.reset && u.AutoReset && u.NextReset > now && u.NextReset < r {
				r = u.NextReset // 保留当前周期:重置日不往后推,新周期更短时提前到 now+R
			}
			next.NextReset = r
		} else {
			next.NextReset = 0
		}
		next.Enabled, next.DisabledReason = true, "" // 和 applyPlan 一样:套餐是明确的商业动作,一律启用
		c.refs = planRefs(q)
	default:
		return c, opErr(http.StatusBadRequest, "bad_mode", errPlanBadMode)
	}
	c.usage = "keep"
	if c.reset {
		c.usage = "reset"
	}
	c.remaining = remainingOf(u, old, c.reset, now)

	if o.Op == "prorate" {
		cr, err := prorateCredit(u, old, q, c.remaining, n, v)
		if err != nil {
			return c, err
		}
		c.credit = cr
		days := n * q.Days
		switch {
		case q.Days <= 0:
			next.Expiry, next.ActivateDays = 0, 0
		case q.FirstUse:
			whole := int(cr.Seconds / 86400) // activateDays 是整天
			if total := days + whole; total > 0 {
				next.Expiry, next.ActivateDays = 0, total
				if drop := cr.Seconds - int64(whole)*86400; drop > 0 {
					c.warn("credit_rounded", map[string]interface{}{"seconds": drop})
				}
			} else {
				// 0 期又折不满一天:直接给到期时间,免得 activateDays = 0 且 expiry = 0 被当成不限期
				next.Expiry, next.ActivateDays = now+cr.Seconds, 0
			}
		default:
			next.Expiry, next.ActivateDays = now+int64(days)*86400+cr.Seconds, 0
		}
		if v > 0 && !q.AutoReset {
			next.Volume = satAdd(satMul(v, n), cr.Bytes)
			if next.Volume <= 0 {
				next.Volume = 1 // 有上限的套餐不能折出 0(0 = 不限)
			}
		}
	}

	c.direction = directionOf(tierOfCurrent(u, old), tierOfPlan(q))
	c.addWarnings(u, q, o, v, used, fromRefs, now)
	return c, nil
}

// extendByPlan 续费式顺延到期(applyPlan 的规则):不限期的套餐 → 不限;还没开始计时 → 加到等待的天数上;
// 首次连接才计时的套餐且已过期 / 不限期 → 等首次连接;其余从"现在与原到期中较晚者"顺延。
func extendByPlan(u *model.User, p model.Plan, days int, now int64, fromExpiry bool) {
	base := now
	if fromExpiry && u.Expiry > now {
		base = u.Expiry
	}
	switch {
	case p.Days <= 0:
		u.Expiry, u.ActivateDays = 0, 0
	case u.ActivateDays > 0:
		u.ActivateDays += days // 还没开始计时:照样等首次连接
	case p.FirstUse && u.Expiry <= now:
		u.Expiry, u.ActivateDays = 0, days // 新号或已过期的:等首次连接才开始计时
	default:
		u.Expiry = base + int64(days)*86400 // 正在计时的老用户续费:接着原到期顺延
	}
}

// addWarnings 变更的影响:立即超量、到期提前(这两条要 force)、配额 / 设备 / 限速变少、少了线路、叠加不了的每期额度……
func (c *planCalc) addWarnings(u model.User, q model.Plan, o planOpOpt, v, used int64, fromRefs []model.LineRef, now int64) {
	next := c.next
	if next.Volume > 0 && !c.reset && used >= next.Volume {
		c.warn("over_quota", map[string]interface{}{"used": used, "volume": next.Volume})
	}
	oldEnd, oldInf := endOf(u, now)
	newEnd, newInf := endOf(next, now)
	if !newInf && (oldInf || newEnd < oldEnd-earlierSlack) {
		if oldInf {
			oldEnd = 0
		}
		c.warn("expiry_earlier", map[string]interface{}{"from": oldEnd, "to": newEnd})
	}
	if lower(u.Volume, next.Volume) {
		c.warn("volume_lower", map[string]interface{}{"from": u.Volume, "to": next.Volume})
	}
	if lower(int64(u.DeviceLimit), int64(next.DeviceLimit)) {
		c.warn("fewer_devices", map[string]interface{}{"from": u.DeviceLimit, "to": next.DeviceLimit})
	}
	if lower(int64(u.SpeedUp), int64(next.SpeedUp)) || lower(int64(u.SpeedDown), int64(next.SpeedDown)) {
		c.warn("slower", map[string]interface{}{"from": []int{u.SpeedUp, u.SpeedDown}, "to": []int{next.SpeedUp, next.SpeedDown}})
	}
	if c.refs != nil {
		keep := map[uint]bool{}
		for _, r := range c.refs {
			keep[r.LineId] = true
		}
		var gone []uint
		for _, r := range fromRefs {
			if !keep[r.LineId] {
				gone = append(gone, r.LineId)
			}
		}
		if len(gone) > 0 {
			c.warn("lines_removed", map[string]interface{}{"lineIds": gone})
		}
	}
	if o.Op == "stack" && v > 0 {
		if q.AutoReset {
			lost := int64(0)
			if u.Volume > 0 {
				lost = max(0, u.Volume-used)
			}
			c.warn("per_period_not_stacked", map[string]interface{}{"lost": lost})
		} else if u.Volume == 0 {
			c.warn("carry_unlimited", nil)
		}
	}
	if !u.Enabled && u.DisabledReason == model.DisabledManual {
		c.warn("enables_manual", nil)
	}
}

// lower 新值比旧值"更紧":0 = 不限。
func lower(from, to int64) bool { return to > 0 && (from <= 0 || to < from) }

// endOf 用户的有效终点:不限期返回 (0, true);还没开始计时的按"现在 + 等待天数"算。
func endOf(u model.User, now int64) (int64, bool) {
	if u.ActivateDays > 0 {
		return now + int64(u.ActivateDays)*86400, false
	}
	if u.Expiry == 0 {
		return 0, true
	}
	return u.Expiry, false
}

// remainingOf 旧订阅的剩余份额。usageReset 为真时连剩余流量一起估值(用量要清零,剩的流量就作废了);
// 保留用量时只按时间估值(已用量会随用户带进新套餐,不能再按流量算一次)。只用和用户同一归属的套餐估值。
func remainingOf(u model.User, p *model.Plan, usageReset bool, now int64) planRemaining {
	var r planRemaining
	inf := math.Inf(1)
	used := u.Up + u.Down
	left := inf
	switch {
	case u.ActivateDays > 0:
		left = float64(u.ActivateDays) * 86400
	case u.Expiry == 0:
	case u.Expiry <= now:
		left = 0
	default:
		left = float64(u.Expiry - now)
	}
	if !math.IsInf(left, 1) {
		s := int64(left)
		r.Seconds = &s
	}
	if u.Volume > 0 {
		b := max(0, u.Volume-used)
		r.Bytes = &b
	}
	if p == nil || p.ResellerId != u.ResellerId {
		return r
	}
	whole := float64(p.Days) * 86400
	timeRatio := inf
	if p.Days > 0 {
		timeRatio = left / whole
	}
	var rho float64
	if !p.AutoReset { // 一次性额度或不限量:时间和流量谁先用完算谁
		traffic := inf
		if pv := model.GBToBytes(p.VolumeGB); usageReset && pv > 0 && u.Volume > 0 {
			traffic = float64(max(0, u.Volume-used)) / float64(pv)
		}
		rho = math.Min(timeRatio, traffic)
		r.TrafficRatio = finite(traffic)
	} else { // 周期重置:本期按剩余流量的比例折,之后的整期按时间
		cyc := float64(p.ResetDays) * 86400
		cycleLeft := cyc
		if u.NextReset > now {
			cycleLeft = float64(u.NextReset - now)
		}
		cycleLeft = math.Min(cycleLeft, left)
		f := 1.0
		if usageReset && u.Volume > 0 {
			f = math.Max(0, math.Min(1, float64(u.Volume-used)/float64(u.Volume)))
		}
		rho = inf
		if p.Days > 0 && !math.IsInf(left, 1) {
			rho = ((left - cycleLeft) + math.Min(cycleLeft, f*cyc)) / whole
		}
		r.TrafficRatio = &f
	}
	r.TimeRatio = finite(timeRatio)
	r.Ratio = finite(rho)
	if r.Ratio != nil && p.Price > 0 {
		val := *r.Ratio * p.Price
		r.Value = &val
	}
	return r
}

// prorateCredit 按参考价把旧订阅的剩余价值折成目标套餐的时长(一次性额度的套餐同时折流量)。
func prorateCredit(u model.User, old *model.Plan, q model.Plan, rem planRemaining, n int, v int64) (*planCredit, error) {
	if old == nil || old.ResellerId != u.ResellerId {
		return nil, opErr(http.StatusBadRequest, "no_current_plan", errPlanNoCurrent)
	}
	if old.Id == q.Id {
		return nil, opErr(http.StatusBadRequest, "same_plan", errPlanSame)
	}
	for _, p := range []*model.Plan{old, &q} {
		if !(p.Price > 0) {
			return nil, opErr(http.StatusBadRequest, "no_price", fmt.Errorf("套餐「%s」没填参考价,不能按价值折算", p.Name))
		}
	}
	if rem.Ratio == nil {
		return nil, opErr(http.StatusBadRequest, "unmeasurable", errPlanUnmeasurable)
	}
	value := *rem.Ratio * old.Price
	periods := value / q.Price
	cr := &planCredit{FromPrice: old.Price, ToPrice: q.Price, Ratio: *rem.Ratio, Value: value, Periods: periods}
	oneTime := v > 0 && !q.AutoReset
	switch {
	case q.Days > 0:
		eff := math.Min(periods, float64(maxCreditDays)/float64(q.Days)) // 时长封顶 3650 天,流量跟着同比例封顶
		cr.Seconds = int64(math.Round(eff * float64(q.Days) * 86400))
		if oneTime { // 一次性额度:一期 = D 天 + V 流量的存量,折 c 期就两样都给 c 份
			cr.Bytes = roundBytes(eff * float64(v))
		}
	case oneTime: // 不限期的流量包:折成流量
		cr.Bytes = roundBytes(periods * float64(v))
	default:
		return nil, opErr(http.StatusBadRequest, "unmeasurable", errPlanNoTimeOrPack)
	}
	if n == 0 && ((q.Days > 0 && cr.Seconds == 0) || (q.Days <= 0 && cr.Bytes == 0)) {
		return nil, opErr(http.StatusBadRequest, "nothing_to_credit", errPlanNothing)
	}
	return cr, nil
}

func finite(x float64) *float64 {
	if math.IsInf(x, 0) || math.IsNaN(x) {
		return nil
	}
	return &x
}

func roundBytes(f float64) int64 {
	if !(f > 0) {
		return 0
	}
	if f >= float64(maxVolume) {
		return maxVolume
	}
	return int64(math.Round(f))
}

func satMul(v int64, n int) int64 {
	if v <= 0 || n <= 0 {
		return 0
	}
	if v > maxVolume/int64(n) {
		return maxVolume
	}
	return v * int64(n)
}

func satAdd(a, b int64) int64 {
	if a > maxVolume-b {
		return maxVolume
	}
	return a + b
}

// ---- 升降级判定(只用于展示与审计)----

// planTier 每 30 天流量、设备数、上行、下行;0 = 不限,换成 +Inf 比较。
type planTier [4]float64

func perMonth(vol int64, autoReset bool, resetDays, days int) float64 {
	switch {
	case vol <= 0:
		return math.Inf(1)
	case autoReset && resetDays > 0:
		return float64(vol) * 30 / float64(resetDays)
	case days > 0:
		return float64(vol) * 30 / float64(days)
	}
	return float64(vol)
}

func limitOf(n int) float64 {
	if n <= 0 {
		return math.Inf(1)
	}
	return float64(n)
}

func tierOfPlan(p model.Plan) planTier {
	return planTier{perMonth(model.GBToBytes(p.VolumeGB), p.AutoReset, p.ResetDays, p.Days), limitOf(p.DeviceLimit), limitOf(p.SpeedUp), limitOf(p.SpeedDown)}
}

// tierOfCurrent 有套餐按套餐比,没有就用用户现在的字段。
func tierOfCurrent(u model.User, old *model.Plan) planTier {
	if old != nil {
		return tierOfPlan(*old)
	}
	return planTier{perMonth(u.Volume, u.AutoReset, u.ResetDays, 0), limitOf(u.DeviceLimit), limitOf(u.SpeedUp), limitOf(u.SpeedDown)}
}

// directionOf 全部 ≥ 且至少一项 > 是升级,全部 ≤ 且至少一项 < 是降级,全等是 same,其余 mixed。
func directionOf(from, to planTier) string {
	up, down := false, false
	for i := range from {
		a, b := from[i], to[i]
		if a == b || (!math.IsInf(a, 1) && !math.IsInf(b, 1) && math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b))) {
			continue
		}
		if b > a {
			up = true
		} else {
			down = true
		}
	}
	switch {
	case up && down:
		return "mixed"
	case up:
		return "upgrade"
	case down:
		return "downgrade"
	}
	return "same"
}

// ---- 执行器 ----

// planScope 发起方:rid 0 = 主面板(面板会话和主令牌共用一个幂等作用域),否则是那个代理;actor 记进审计和套餐记录。
type planScope struct {
	rid   uint
	actor string
}

// planOpCall 一次套餐操作(参数已校验、目标套餐已找到、代理校验已做)。
type planOpCall struct {
	endpoint  string // change | plan(/renew 同)| batch:算幂等指纹用
	opt       planOpOpt
	target    model.Plan
	fromPlan  *uint // 前置条件:用户现在的 planId 必须是它
	force     bool
	dryRun    bool
	requestID string
	remark    string
	quiet     bool // 批量:不逐个写审计、不逐个热更新(调用方最后统一做)
}

// fingerprint 规范化后的请求内容:同一个幂等键只认同一份内容。
func (c planOpCall) fingerprint(uid uint) string {
	from := int64(-1)
	if c.fromPlan != nil {
		from = int64(*c.fromPlan)
	}
	b, _ := json.Marshal([]interface{}{c.endpoint, uid, c.target.Id, c.opt.Op, c.opt.Periods, c.opt.Usage, from, c.force, c.remark})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// planOpInput 计算要读的东西,用传进来的 db 读(正式执行时是事务里的 tx)。
type planOpInput struct {
	user     model.User
	old      *model.Plan
	fromRefs []model.LineRef
	reseller *model.Reseller
	rsUsed   int64
}

func loadPlanOpInput(db *gorm.DB, uid uint) (planOpInput, error) {
	var in planOpInput
	if err := db.First(&in.user, uid).Error; err != nil {
		return in, opErr(http.StatusNotFound, "", errPlanUserGone)
	}
	if in.user.PlanId > 0 {
		var p model.Plan
		if db.First(&p, in.user.PlanId).Error == nil {
			in.old = &p
		}
	}
	links, scopes, err := render.EffectiveLines(db, uid)
	if err != nil {
		return in, err
	}
	in.fromRefs = lineRefsBy(links, scopes)[uid]
	if in.user.ResellerId > 0 {
		var rs model.Reseller
		if db.First(&rs, in.user.ResellerId).Error == nil {
			in.reseller = &rs
			in.rsUsed = jobs.ResellerUsed(db, rs)
		}
	}
	return in, nil
}

// buildPlanResult 计算并组装结果;toRefs 是整理好的套餐线路(套餐不带线路时为 nil)。
func buildPlanResult(in planOpInput, call planOpCall, toRefs []model.LineRef, now int64) (*planResult, error) {
	if call.fromPlan != nil && *call.fromPlan != in.user.PlanId {
		return nil, opErr(http.StatusConflict, "plan_changed", errPlanChanged)
	}
	c, err := calcPlanOp(in.user, in.old, call.target, call.opt, in.fromRefs, now)
	if err != nil {
		return nil, err
	}
	oldName := ""
	if in.old != nil {
		oldName = in.old.Name
	}
	refsTo := in.fromRefs
	if c.refs != nil {
		refsTo = toRefs
	}
	res := &planResult{Op: call.opt.Op, Direction: c.direction, Periods: call.opt.Periods, Usage: c.usage,
		From: stateOf(in.user, oldName, in.fromRefs), To: stateOf(c.next, call.target.Name, refsTo),
		Remaining: c.remaining, Credit: c.credit, Warnings: c.warnings, calc: c}
	if rs := in.reseller; rs != nil {
		pool := &planPool{Volume: rs.Volume, Used: in.rsUsed}
		if rs.Volume > 0 {
			pool.Remaining = max(0, rs.Volume-in.rsUsed)
			// 用户这边还能用多少(不限 = 无穷)多于代理剩的额度:代理先用尽,用户也就用不上了
			if c.next.Volume == 0 || c.next.Volume-(c.next.Up+c.next.Down) > pool.Remaining {
				var left interface{}
				if c.next.Volume > 0 {
					left = c.next.Volume - (c.next.Up + c.next.Down)
				}
				res.Warnings = append(res.Warnings, newPlanWarning("reseller_pool_low", map[string]interface{}{"remaining": left, "pool": pool.Remaining}))
			}
		}
		res.Pool = pool
		if end, inf := endOf(c.next, now); rs.Expiry > 0 && (inf || end > rs.Expiry) {
			res.Warnings = append(res.Warnings, newPlanWarning("reseller_expires_first", map[string]interface{}{"resellerExpiry": rs.Expiry}))
		}
	}
	if res.Warnings == nil {
		res.Warnings = []planWarning{}
	}
	for _, w := range res.Warnings {
		if w.Block {
			res.NeedsForce = true
		}
	}
	return res, nil
}

// planOpFields 要写回用户行的字段:用量列由 updateUserFields 用 SQL 结算(不吞统计这期间记的流量),
// stack 的一次性额度结转也在同一条 UPDATE 里算(右边取的是更新前的值)。
func planOpFields(c planCalc) map[string]interface{} {
	f := updateUserFields(c.next, c.reset)
	switch c.carry {
	case "keep":
		f["volume"] = gorm.Expr("volume + ?", c.carryAdd)
	case "reset":
		f["volume"] = gorm.Expr("MAX(0, volume - up - down) + ?", c.carryAdd)
	}
	return f
}

// runPlanOp 执行(或预览)一次套餐操作。调用方负责参数校验、找目标套餐和代理校验(它们要用 s.db 读,不能放进事务)。
func (s *Server) runPlanOp(sc planScope, uid uint, call planOpCall) (*planResult, error) {
	now := time.Now().Unix()
	toRefs := s.normalizeRefs(planRefs(call.target)) // 要读线路表,放在事务外
	if call.dryRun {
		var res *planResult
		err := database.ReadTx(s.db, func(tx *gorm.DB) error {
			in, err := loadPlanOpInput(tx, uid)
			if err != nil {
				return err
			}
			res, err = buildPlanResult(in, call, toRefs, now)
			return err
		})
		if err != nil {
			return nil, err
		}
		res.DryRun, res.RequestId = true, call.requestID
		return res, nil
	}

	hash := call.fingerprint(uid)
	var res *planResult
	var prev *model.PlanOp
	var before model.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		res, prev = nil, nil
		if call.requestID != "" { // 幂等:同一作用域里这个键执行过就原样重放,内容不同就 409
			p, err := findPlanOp(tx, sc.rid, call.requestID)
			if err != nil {
				return err
			}
			if p != nil {
				if p.ReqHash != hash {
					return opErr(http.StatusConflict, "idempotency_conflict", errPlanIdemConflict)
				}
				prev = p
				return nil
			}
		}
		// 在事务里重新读:持着写锁,统计任务这期间记下的流量都已经在库里,结转按最新的算
		in, err := loadPlanOpInput(tx, uid)
		if err != nil {
			return err
		}
		res, err = buildPlanResult(in, call, toRefs, now)
		if err != nil {
			return err
		}
		if call.endpoint == "change" && res.NeedsForce && !call.force {
			return &planOpError{status: http.StatusConflict, code: "needs_force", err: errPlanNeedsForce, preview: res}
		}
		before = in.user
		if err := tx.Model(&model.User{}).Where("id = ?", uid).Updates(planOpFields(res.calc)).Error; err != nil {
			return err
		}
		if res.calc.refs != nil {
			if err := writeUserLineRefs(tx, uid, toRefs); err != nil {
				return err
			}
		}
		rec := newPlanOp(sc, call, hash, in.user, res, now)
		if err := tx.Create(&rec).Error; err != nil {
			return err
		}
		res.OpId = rec.Id
		if !call.quiet {
			obj, _ := json.Marshal(in.user.Name) // 和老的套用套餐审计同一格式:老库补「所属套餐」时按它解析
			return tx.Create(&model.Change{DateTime: now, Actor: actorOr(sc.actor), Key: "user", Action: "plan:" + call.opt.Op + ":" + call.target.Name, Obj: obj}).Error
		}
		return nil
	})
	if err != nil && call.requestID != "" && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		// 唯一索引兜底(BEGIN IMMEDIATE 下同键请求本来就串行,第二个会在上面看到第一个的记录)
		if p, e := findPlanOp(s.db, sc.rid, call.requestID); e == nil && p != nil {
			if p.ReqHash != hash {
				return nil, opErr(http.StatusConflict, "idempotency_conflict", errPlanIdemConflict)
			}
			prev, err = p, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if prev != nil {
		r := planResultFromOp(*prev)
		r.Replayed = true
		return r, nil
	}
	res.RequestId = call.requestID
	if !call.quiet {
		s.reloadUsers("套餐 " + call.target.Name + " → " + before.Name)
		if before.PlanId != call.target.Id {
			s.rulesNow() // 换了套餐,按套餐选人的限速规则要立刻重判
		}
	}
	if next := res.calc.next; res.calc.reset || (before.Volume > 0 && (next.Volume == 0 || next.Volume > before.Volume)) {
		s.forgetQuotaAlert(before.Name) // 用量清零或配额变大:再到阈值要能再提醒
	}
	return res, nil
}

func actorOr(a string) string {
	if a == "" {
		return "?"
	}
	return a
}

func findPlanOp(db *gorm.DB, rid uint, key string) (*model.PlanOp, error) {
	var op model.PlanOp
	if err := db.Where("reseller_id = ? AND request_id = ?", rid, key).Limit(1).Find(&op).Error; err != nil {
		return nil, err
	}
	if op.Id == 0 {
		return nil, nil
	}
	return &op, nil
}

func newPlanOp(sc planScope, call planOpCall, hash string, u model.User, res *planResult, now int64) model.PlanOp {
	op := model.PlanOp{ResellerId: sc.rid, ReqHash: hash, UserId: u.Id, UserName: u.Name, Actor: actorOr(sc.actor),
		Op: call.opt.Op, Usage: res.Usage, Direction: res.Direction, FromPlanId: u.PlanId, FromPlan: res.From.Plan,
		ToPlanId: call.target.Id, ToPlan: call.target.Name, Periods: call.opt.Periods, Remark: call.remark, CreatedAt: now}
	if call.requestID != "" {
		key := call.requestID
		op.RequestId = &key
	}
	op.Before, _ = json.Marshal(res.From)
	op.After, _ = json.Marshal(res.To)
	op.Remaining, _ = json.Marshal(res.Remaining)
	if res.Credit != nil {
		op.Credit, _ = json.Marshal(res.Credit)
	}
	if len(res.Warnings) > 0 {
		op.Warnings, _ = json.Marshal(res.Warnings)
	}
	return op
}

// planResultFromOp 重放:原样给首次执行时的结果(变更前后、折算、告警)。
func planResultFromOp(op model.PlanOp) *planResult {
	res := &planResult{Op: op.Op, Direction: op.Direction, Periods: op.Periods, Usage: op.Usage, OpId: op.Id, Warnings: []planWarning{}}
	if op.RequestId != nil {
		res.RequestId = *op.RequestId
	}
	_ = json.Unmarshal(op.Before, &res.From)
	_ = json.Unmarshal(op.After, &res.To)
	if len(op.Remaining) > 0 {
		_ = json.Unmarshal(op.Remaining, &res.Remaining)
	}
	if len(op.Credit) > 0 {
		var cr planCredit
		if json.Unmarshal(op.Credit, &cr) == nil {
			res.Credit = &cr
		}
	}
	if len(op.Warnings) > 0 {
		_ = json.Unmarshal(op.Warnings, &res.Warnings)
	}
	for _, w := range res.Warnings {
		if w.Block {
			res.NeedsForce = true
		}
	}
	return res
}

// ---- 请求解析 ----

// planOpReq 套餐操作的请求体:外部 API 的 /plan、/renew、/change 与面板的同名接口共用。
type planOpReq struct {
	PlanId     *uint   `json:"planId"`
	Plan       *string `json:"plan"` // 套餐名,与 planId 二选一
	Mode       string  `json:"mode"`
	Periods    *int    `json:"periods"`
	Usage      string  `json:"usage"`
	FromPlanId *uint   `json:"fromPlanId"`
	Force      bool    `json:"force"`
	DryRun     bool    `json:"dryRun"`
	RequestId  string  `json:"requestId"`
	Remark     string  `json:"remark"`
}

// decodePlanOpReq 读请求体;空请求体等于 {}(续当前套餐时什么都不用带)。?dryRun=true 也算预览。
func decodePlanOpReq(r *http.Request) (planOpReq, error) {
	var req planOpReq
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			return req, errors.New("请求体不是合法 JSON: " + err.Error())
		}
	}
	if v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("dryRun"))); v == "1" || v == "true" {
		req.DryRun = true
	}
	return req, nil
}

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// planRequestID 幂等键:请求体的 requestId 优先,其次请求头 Idempotency-Key;都没有 = 不做幂等。
func planRequestID(r *http.Request, body string) (string, error) {
	key := strings.TrimSpace(body)
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	if key == "" {
		return "", nil
	}
	if !requestIDRe.MatchString(key) {
		return "", opErr(http.StatusBadRequest, "bad_request_id", errPlanBadRequestID)
	}
	return key, nil
}

// planPeriods 期数:默认 1,1–120(折算可以 0);多期时 N × 套餐天数不超过 36500。
func planPeriods(p *int, op string, days int) (int, error) {
	n := 1
	if p != nil {
		n = *p
	}
	least := 1
	if op == "prorate" {
		least = 0
	}
	if n < least || n > maxPlanPeriods || (n > 1 && days > 0 && n*days > maxPlanDays) {
		return 0, opErr(http.StatusBadRequest, "bad_periods", errPlanBadPeriods)
	}
	return n, nil
}

func planUsage(s string) (string, error) {
	switch u := strings.ToLower(strings.TrimSpace(s)); u {
	case "", "auto":
		return "auto", nil
	case "keep", "reset":
		return u, nil
	}
	return "", opErr(http.StatusBadRequest, "bad_usage", errPlanBadUsage)
}

func planRemark(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > planRemarkMax {
		s = string(r[:planRemarkMax])
	}
	return s
}

// ---- 两类操作的入口(面板与外部 API 共用)----

// renewUserPlan 续费 / 延期 N 期。没给套餐就续用户现在的套餐。byScope 为真(外部 API)时给的套餐只在调用方作用域里找,
// 和以前的 /plan 一致;面板按 id 找,代理照旧只能用自己的套餐。
func (s *Server) renewUserPlan(r *http.Request, sc planScope, u model.User, req planOpReq, byScope bool) (*planResult, error) {
	var q model.Plan
	switch {
	case (req.PlanId != nil && *req.PlanId > 0) || (req.Plan != nil && strings.TrimSpace(*req.Plan) != ""):
		if byScope || req.PlanId == nil || *req.PlanId == 0 {
			p, err := s.apiFindPlan(apiUserReq{PlanId: req.PlanId, Plan: req.Plan}, sc.rid)
			if err != nil {
				return nil, opErr(http.StatusBadRequest, "plan_not_found", err)
			}
			q = *p
		} else {
			p, err := s.loadPlan(*req.PlanId)
			if err != nil {
				return nil, opErr(http.StatusBadRequest, "plan_not_found", err)
			}
			q = p
		}
	case u.PlanId == 0:
		return nil, opErr(http.StatusBadRequest, "plan_required", errPlanNoUserPlan)
	default:
		p, err := s.loadPlan(u.PlanId)
		if err != nil {
			return nil, opErr(http.StatusBadRequest, "plan_not_found", err)
		}
		q = p
	}
	mode := req.Mode
	if mode != "extend" {
		mode = "renew" // 未知值照旧按 renew(兼容老调用方)
	}
	n, err := planPeriods(req.Periods, mode, q.Days)
	if err != nil {
		return nil, err
	}
	key, err := planRequestID(r, req.RequestId)
	if err != nil {
		return nil, err
	}
	if sc.rid > 0 { // 代理:只能续自己的套餐,线路在授权内,代理本身可用(启用、未到期、额度未用尽)
		if q.ResellerId != sc.rid {
			return nil, opErr(http.StatusBadRequest, "plan_not_found", errors.New("套餐不存在: id "+strconv.Itoa(int(q.Id))))
		}
		if err := s.checkResellerPlan(sc.rid, u, q); err != nil {
			return nil, opErr(http.StatusBadRequest, "reseller_denied", err)
		}
	}
	return s.runPlanOp(sc, u.Id, planOpCall{endpoint: "plan", opt: planOpOpt{Op: mode, Periods: n}, target: q,
		fromPlan: req.FromPlanId, dryRun: req.DryRun, requestID: key, remark: planRemark(req.Remark)})
}

// changeUserPlan 换套餐:mode 必填(replace / stack / prorate)。目标套餐按**用户的归属**找:主面板用户找主面板的套餐,
// 代理用户找那个代理的套餐(主令牌操作代理用户也一样),免得指到别人的套餐上。
func (s *Server) changeUserPlan(r *http.Request, sc planScope, u model.User, req planOpReq) (*planResult, error) {
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode != "replace" && mode != "stack" && mode != "prorate" {
		return nil, opErr(http.StatusBadRequest, "bad_mode", errPlanBadMode)
	}
	usage, err := planUsage(req.Usage)
	if err != nil {
		return nil, err
	}
	p, err := s.apiFindPlan(apiUserReq{PlanId: req.PlanId, Plan: req.Plan}, u.ResellerId)
	if err != nil {
		return nil, opErr(http.StatusBadRequest, "plan_not_found", err)
	}
	if p == nil {
		return nil, opErr(http.StatusBadRequest, "plan_required", errPlanRequired)
	}
	n, err := planPeriods(req.Periods, mode, p.Days)
	if err != nil {
		return nil, err
	}
	key, err := planRequestID(r, req.RequestId)
	if err != nil {
		return nil, err
	}
	if sc.rid > 0 {
		if err := s.checkResellerPlan(sc.rid, u, *p); err != nil {
			return nil, opErr(http.StatusBadRequest, "reseller_denied", err)
		}
	}
	return s.runPlanOp(sc, u.Id, planOpCall{endpoint: "change", opt: planOpOpt{Op: mode, Periods: n, Usage: usage}, target: *p,
		fromPlan: req.FromPlanId, force: req.Force, dryRun: req.DryRun, requestID: key, remark: planRemark(req.Remark)})
}

// ---- HTTP ----

// handleUserChange 面板:POST /users/{id}/change(代理面板经 guardScope 校验过归属)。返回结果对象,不带用户对象。
func (s *Server) handleUserChange(w http.ResponseWriter, r *http.Request, u model.User) {
	req, err := decodePlanOpReq(r)
	if err != nil {
		writePlanErr(w, err)
		return
	}
	res, err := s.changeUserPlan(r, planScope{rid: scope(r), actor: s.actor(r)}, u, req)
	if err != nil {
		writePlanErr(w, err)
		return
	}
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, res)
}

// apiRenew 外部 API:POST /users/{name|id}/plan(别名 /renew)。正式执行返回用户对象(和以前一样),预览返回结果对象。
func (s *Server) apiRenew(w http.ResponseWriter, r *http.Request, u model.User, sc apiScope) {
	req, err := decodePlanOpReq(r)
	if err != nil {
		writePlanErr(w, err)
		return
	}
	res, err := s.renewUserPlan(r, planScope{rid: sc.rid, actor: sc.actor}, u, req, true)
	if err != nil {
		writePlanErr(w, err)
		return
	}
	if res.DryRun {
		writeJSON(w, http.StatusOK, res)
		return
	}
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	var fresh model.User // 落库后的最新值(含统计这期间记的流量)
	if err := s.db.First(&fresh, u.Id).Error; err != nil {
		writePlanErr(w, opErr(http.StatusNotFound, "", errPlanUserGone))
		return
	}
	writeJSON(w, http.StatusOK, s.apiUser(fresh))
}

// apiChange 外部 API:POST /users/{name|id}/change。返回结果对象,正式执行与重放时带上最新的用户对象。
func (s *Server) apiChange(w http.ResponseWriter, r *http.Request, u model.User, sc apiScope) {
	req, err := decodePlanOpReq(r)
	if err != nil {
		writePlanErr(w, err)
		return
	}
	res, err := s.changeUserPlan(r, planScope{rid: sc.rid, actor: sc.actor}, u, req)
	if err != nil {
		writePlanErr(w, err)
		return
	}
	if !res.DryRun {
		var fresh model.User
		if s.db.First(&fresh, u.Id).Error == nil {
			v := s.apiUser(fresh)
			res.User = &v
		}
		if res.Replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// writePlanOps GET /users/{id}/plan-ops?limit=:套餐记录,最新的在前,limit 默认 20、最多 100。
// 代理看到的记录里,不是他自己(代理面板或他的令牌)操作的,操作人一律显示为「主面板」。
func (s *Server) writePlanOps(w http.ResponseWriter, r *http.Request, u model.User, rid uint) {
	limit := 20
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 100)
	}
	var ops []model.PlanOp
	if err := s.db.Where("user_id = ?", u.Id).Order("id desc").Limit(limit).Find(&ops).Error; err != nil {
		badRequest(w, err)
		return
	}
	if ops == nil {
		ops = []model.PlanOp{}
	}
	for i := range ops {
		if rid > 0 && ops[i].ResellerId != rid {
			ops[i].Actor = planOpMasterTag
		}
	}
	writeJSON(w, http.StatusOK, ops)
}
