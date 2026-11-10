package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
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
// 面板的续费、批量续费、换套餐,外部 API 的 /plan、/renew、/change 以及带套餐的 PATCH /users 都走这里:
//
//	calcPlanOp  纯函数,只算:变更后的用户、要写的线路、用量怎么处理、折算与告警。预览和正式执行用同一个。
//	runPlanOp   执行器:一个 BEGIN IMMEDIATE 事务里依次查幂等键 → 重新读用户 → 核对 fromPlanId 与「续当前套餐」的目标
//	            → 计算 → 护栏 → 写用户(用量列用 SQL 原子结算)、线路、套餐记录、审计。事务体里只能用 tx:用 s.db 写会向
//	            第二条连接要写锁,一直等到 busy_timeout(10 秒)才失败;用 s.db 读在并发时会把连接池耗尽(排队等写锁的
//	            事务各占着一条连接)。所以要用 s.db 读的校验(找套餐、代理授权、整理线路)都在事务外先做。
//
// 记号:N 期数;目标套餐 Q 的天数 D、每期流量 V、重置天数 R;used = up + down。
// 「一次性额度」= 不周期重置且有上限,配额是整段时长的存量;「周期重置」的流量是每期额度,不叠加。
// 配额、天数的加法都按 model.MaxVolume / model.MaxDays 饱和(溢出的整数在 SQLite 里会变成 REAL,整表读不出来)。

const (
	maxPlanPeriods   = 120   // 一次最多续 / 换 120 期
	maxPlanDays      = 36500 // 期数 × 套餐天数不超过约 100 年
	maxCreditDays    = 3650  // 折算出来的时长封顶
	earlierSlack     = 60    // 到期只早这么几秒不算"提前"(预览到执行之间的时间差)
	planRemarkMax    = 200   // 套餐记录的备注最多这么多字
	planOpMasterTag  = "主面板"
	planCurrentTries = 3 // 续当前套餐:事务里发现所属套餐刚被换掉时,按最新的用户重来几次
)

var (
	errPlanRequired     = errors.New("缺少 planId 或 plan")
	errPlanNoUserPlan   = errors.New("用户没有所属套餐,请给 planId 或 plan")
	errPlanNotFound     = errors.New("套餐不存在")
	errPlanBadMode      = errors.New("mode 只能是 replace / stack / prorate")
	errPlanBadPeriods   = errors.New("期数需在 1 到 120 之间(折算可为 0),且总天数不超过 36500")
	errPlanBadUsage     = errors.New("usage 只能是 auto / keep / reset")
	errPlanBadRequestID = errors.New("requestId 最长 64 个字符,只能含字母、数字和 . _ : -")
	errPlanBody         = errors.New("请求体要是 JSON 对象")
	errPlanNoCurrent    = errors.New("用户没有所属套餐,不能按价值折算")
	errPlanSame         = errors.New("目标套餐就是当前套餐,不能折算:要从现在重新开始用 replace,要带结转续费用 stack")
	errPlanUnmeasurable = errors.New("当前订阅估不出剩余价值:时间上不限期(或旧套餐不限天数),流量上又没法折(不限量,或保留用量时只按时间估值)")
	errPlanNoTimeOrPack = errors.New("目标套餐不限期又不是一次性流量,折算结果没法计入")
	errPlanNothing      = errors.New("没有可折算的剩余价值,期数不能为 0")
	errPlanNeedsForce   = errors.New("这次变更有需要确认的影响(见 warnings),确认后带 force: true 重试")
	errPlanChanged      = errors.New("用户当前套餐与 fromPlanId 不符")
	errPlanMoved        = errors.New("用户的所属套餐刚被别的操作换掉了,请确认后重试")
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
	"over_quota":             "保留的已用量已达到新配额,下一轮判定就会停用,到下次周期重置才恢复",
	"expiry_earlier":         "变更后的到期早于现在的到期",
	"volume_lower":           "配额变少",
	"fewer_devices":          "设备数上限变少",
	"slower":                 "限速变低",
	"lines_removed":          "按套餐替换线路后,用户会少几条线路",
	"per_period_not_stacked": "周期重置套餐的流量是每期额度,叠加只延长时间",
	"carry_unlimited":        "原来不限量,没有可结转的剩余流量:新配额就是新套餐给的流量",
	"carry_expired":          "旧订阅已经过期,剩下的流量随之作废,不结转",
	"expiry_kept":            "目标套餐不限期:叠加不会把到期改成不限,原来的到期保留(要改成不限期请用立即替换)",
	"volume_kept":            "目标套餐不限量:叠加不会把配额改成不限,原来剩下的流量保留(要改成不限量请用立即替换)",
	"credit_rounded":         "目标套餐从首次连接开始计时(按整天算),折算出来不足一天的部分舍去",
	"enables_manual":         "这个用户是手动停用的,执行后会被启用",
	"reseller_pool_low":      "变更后用户的剩余配额多于代理剩余的流量额度",
	"reseller_expires_first": "变更后的到期晚于代理自己的到期,代理到期时用户一并停用",
}

// overQuotaOnce over_quota 落在不周期重置的套餐上:没有"下次重置",要续费或清零用量才恢复。
const overQuotaOnce = "保留的已用量已达到新配额,下一轮判定就会停用,要续费或清零用量才恢复"

// planWarnBlock 这两种会立刻伤到用户(下一轮就停用 / 少了时间),/change 正式执行时要带 force。
var planWarnBlock = map[string]bool{"over_quota": true, "expiry_earlier": true}

func newPlanWarning(code string, args map[string]interface{}) planWarning {
	text := planWarnText[code]
	if code == "over_quota" && args["nextReset"] == int64(0) {
		text = overQuotaOnce
	}
	return planWarning{Code: code, Text: text, Args: args, Block: planWarnBlock[code]}
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
// 已经过期的订阅剩余一律是 0(剩下的流量随过期作废)。
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

	calc      planCalc        // 执行器落库用,不出接口
	writeRefs []model.LineRef // 要写的线路(已整理);nil = 不改
}

// ---- 计算 ----

// planCalc calcPlanOp 的结果。next 里的用量已按"清零与否"预测好;stack 的配额落库时用 SQL 原子结转(carry)。
type planCalc struct {
	next  model.User
	reset bool   // 本周期用量并入历史、清零
	usage string // 实际的用量处理:keep | reset
	// carry stack 结转一次性额度(SQL 里算,都按 MaxVolume 封顶):
	//	keep    volume + N×V
	//	reset   max(1, max(0, volume − used) + N×V)
	//	expired max(1, used + N×V)(旧订阅已过期,剩的作废;保留用量时新的一份全额可用)
	carry     string
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

// subExpired 订阅已经过期(不是还在等首次连接):剩下的时间和流量都已作废。
func subExpired(u model.User, now int64) bool {
	return u.ActivateDays == 0 && u.Expiry > 0 && u.Expiry <= now
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
	expired := subExpired(u, now)
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
				next.Expiry, next.ActivateDays = 0, model.CapDays(days)
			default:
				next.Expiry, next.ActivateDays = model.AddDays(now, days), 0
			}
		case "stack": // 和续费一样顺延:保留旧订阅剩的时间再加 N 期
			if q.Days <= 0 && !expired && (u.Expiry > 0 || u.ActivateDays > 0) {
				// 目标不限期:叠加不能把有限的到期悄悄变成不限,原来的到期(或等首次连接的天数)保留。
				// 已经过期的没有可保留的东西,等于从现在开一份新的,按目标套餐来(下面 extendByPlan 给不限期)
				c.warn("expiry_kept", map[string]interface{}{"expiry": u.Expiry, "activateDays": u.ActivateDays})
			} else {
				extendByPlan(next, q, days, now, true)
			}
		} // prorate 的到期要等折算出来(见下)
		left := max(0, u.Volume-used) // 旧订阅剩的流量
		switch {
		case v == 0 && o.Op == "stack" && u.Volume > 0 && !expired:
			// 目标不限量:同样不把有限的配额悄悄变成不限,原来剩下的保留(清零用量时配额就是剩下的,至少 1 字节:0 = 不限)
			c.carry, c.carryAdd = usage, 0
			if c.reset {
				next.Volume = max(1, min(left, model.MaxVolume))
			} else {
				next.Volume = min(u.Volume, model.MaxVolume)
			}
			c.warn("volume_kept", map[string]interface{}{"left": left})
		case v == 0:
			next.Volume = 0
		case q.AutoReset:
			next.Volume = v // 每期额度,不叠加
		case o.Op == "stack" && u.Volume > 0 && expired:
			// 旧订阅已过期:剩的流量随之作废,按 0 结转(和折算的估值口径一致);保留用量时已用量照算,新的一份全额可用
			c.carryAdd = satMul(v, n)
			next.Volume = c.carryAdd
			if !c.reset {
				c.carry = "expired"
				next.Volume = max(1, satAdd(used, c.carryAdd))
			}
		case o.Op == "stack" && u.Volume > 0:
			c.carry, c.carryAdd = usage, satMul(v, n)
			if c.reset {
				next.Volume = max(1, satAdd(left, c.carryAdd))
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
			r := model.AddDays(now, q.ResetDays)
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
			if total := model.SumDays(days, whole); total > 0 {
				next.Expiry, next.ActivateDays = 0, total
				if drop := cr.Seconds - int64(whole)*86400; drop > 0 {
					c.warn("credit_rounded", map[string]interface{}{"seconds": drop})
				}
			} else {
				// 0 期又折不满一天:直接给到期时间,免得 activateDays = 0 且 expiry = 0 被当成不限期
				next.Expiry, next.ActivateDays = now+cr.Seconds, 0
			}
		default:
			next.Expiry, next.ActivateDays = model.AddDays(now, days)+cr.Seconds, 0
		}
		if v > 0 && !q.AutoReset {
			next.Volume = satAdd(satMul(v, n), cr.Bytes)
			if next.Volume <= 0 {
				next.Volume = 1 // 有上限的套餐不能折出 0(0 = 不限)
			}
		}
	}

	c.direction = directionOf(tierOfCurrent(u, old), tierOfPlan(q))
	c.addWarnings(u, q, o, v, used, expired, fromRefs, now)
	return c, nil
}

// extendByPlan 续费式顺延到期(applyPlan 的规则):不限期的套餐 → 不限;还没开始计时 → 加到等待的天数上;
// 首次连接才计时的套餐且已过期 / 不限期 → 等首次连接;其余从"现在与原到期中较晚者"顺延。天数与到期都封顶,不会溢出。
func extendByPlan(u *model.User, p model.Plan, days int, now int64, fromExpiry bool) {
	base := now
	if fromExpiry && u.Expiry > now {
		base = u.Expiry
	}
	switch {
	case p.Days <= 0:
		u.Expiry, u.ActivateDays = 0, 0
	case u.ActivateDays > 0:
		u.ActivateDays = model.SumDays(u.ActivateDays, days) // 还没开始计时:照样等首次连接
	case p.FirstUse && u.Expiry <= now:
		u.Expiry, u.ActivateDays = 0, model.CapDays(days) // 新号或已过期的:等首次连接才开始计时
	default:
		u.Expiry = model.AddDays(base, days) // 正在计时的老用户续费:接着原到期顺延
	}
}

// addWarnings 变更的影响:立即超量、到期提前(这两条要 force)、配额 / 设备 / 限速变少、少了线路、叠加不了的每期额度……
func (c *planCalc) addWarnings(u model.User, q model.Plan, o planOpOpt, v, used int64, expired bool, fromRefs []model.LineRef, now int64) {
	next := c.next
	if next.Volume > 0 && !c.reset && used >= next.Volume {
		// nextReset:新套餐周期重置时到那一刻恢复;0 = 不重置,要续费或清零用量
		c.warn("over_quota", map[string]interface{}{"used": used, "volume": next.Volume, "nextReset": next.NextReset})
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
		left := int64(0) // 旧订阅剩的流量:叠加时结转不过来的那部分
		if u.Volume > 0 {
			left = max(0, u.Volume-used)
		}
		if expired && left > 0 {
			c.warn("carry_expired", map[string]interface{}{"lost": left}) // 随过期作废,和每期不叠加分开说
			left = 0
		}
		switch {
		case q.AutoReset:
			c.warn("per_period_not_stacked", map[string]interface{}{"lost": left})
		case u.Volume == 0:
			c.warn("carry_unlimited", map[string]interface{}{"volume": next.Volume})
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
		return model.AddDays(now, u.ActivateDays), false
	}
	if u.Expiry == 0 {
		return 0, true
	}
	return u.Expiry, false
}

// remainingOf 旧订阅的剩余份额。usageReset 为真时连剩余流量一起估值(用量要清零,剩的流量就作废了);
// 保留用量时只按时间估值(已用量会随用户带进新套餐,不能再按流量算一次)。只用和用户同一归属的套餐估值。
// 已经过期的订阅,剩余的时间与流量都按 0 算。
func remainingOf(u model.User, p *model.Plan, usageReset bool, now int64) planRemaining {
	var r planRemaining
	inf := math.Inf(1)
	used := u.Up + u.Down
	expired := subExpired(u, now)
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
	bytesLeft := max(0, u.Volume-used)
	if expired {
		bytesLeft = 0 // 过期作废
	}
	if u.Volume > 0 {
		r.Bytes = &bytesLeft
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
			traffic = float64(bytesLeft) / float64(pv)
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
			f = math.Max(0, math.Min(1, float64(bytesLeft)/float64(u.Volume)))
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
	if f >= float64(model.MaxVolume) {
		return model.MaxVolume
	}
	return int64(math.Round(f))
}

// satMul / satAdd 配额的乘、加,按 model.MaxVolume 饱和(参数都是非负的)。
func satMul(v int64, n int) int64 {
	if v <= 0 || n <= 0 {
		return 0
	}
	if v > model.MaxVolume/int64(n) {
		return model.MaxVolume
	}
	return v * int64(n)
}

func satAdd(a, b int64) int64 {
	if a > model.MaxVolume-b {
		return model.MaxVolume
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
	endpoint  string // change | plan(/renew 同)| batch | update(带套餐的 PATCH /users)
	opt       planOpOpt
	target    model.Plan
	current   bool  // 续当前套餐:事务里核对用户的所属套餐仍是 target(没被并发的换套餐换掉、也没被改过),否则放弃重来
	fromPlan  *uint // 前置条件:用户现在的 planId 必须是它
	force     bool
	dryRun    bool
	requestID string
	reqHash   string // 幂等指纹:按请求本身的内容算(套餐按请求里的写法),见 planHash
	remark    string
	quiet     bool       // 批量:不逐个写审计、不逐个热更新(调用方最后统一做)
	patch     *userPatch // 带套餐的 PATCH /users:套餐之后再盖上请求里显式给出的字段
	audit     string     // 审计动作;空 = plan:<动作>:<套餐名>
}

// planHash 幂等指纹:规范化后的请求内容,同一个幂等键只认同一份内容。目标套餐按请求里的写法记(planSelector),
// 不按解析出来的套餐:已经成功的请求重试时,套餐后来改了名或删了,照样认得出是同一笔。
func planHash(parts ...interface{}) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// planSelector 请求里怎么指定的目标套餐:id:<n> / name:<名字> / current(没给套餐)。
func planSelector(req planOpReq) string {
	switch {
	case req.PlanId != nil && *req.PlanId > 0:
		return "id:" + strconv.FormatUint(uint64(*req.PlanId), 10)
	case req.Plan != nil && strings.TrimSpace(*req.Plan) != "":
		return "name:" + strings.TrimSpace(*req.Plan)
	}
	return "current"
}

// fromOf fromPlanId 进指纹:没给记 -1(0 是"必须没有套餐")。
func fromOf(p *uint) int64 {
	if p == nil {
		return -1
	}
	return int64(*p)
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
func buildPlanResult(sc planScope, in planOpInput, call planOpCall, toRefs []model.LineRef, now int64) (*planResult, error) {
	if call.fromPlan != nil && *call.fromPlan != in.user.PlanId {
		return nil, opErr(http.StatusConflict, "plan_changed", errPlanChanged)
	}
	if call.current && (in.old == nil || !reflect.DeepEqual(*in.old, call.target)) {
		// 续当前套餐:事务外按请求开始时的用户找到、校验过的那个套餐,到事务里已经不是他的当前套餐了(并发的换套餐先提交),
		// 或者套餐刚被改过。不能照旧续,那会把用户静默改回旧套餐;调用方按最新的用户重新解析再来
		return nil, opErr(http.StatusConflict, "plan_changed", errPlanMoved)
	}
	old := in.old
	if old != nil && sc.rid > 0 && old.ResellerId != sc.rid {
		old = nil // 别人的套餐(主面板套到代理用户上的):名字不给代理看,也不拿它比升降级(估值本来就只认同归属的)
	}
	c, err := calcPlanOp(in.user, old, call.target, call.opt, in.fromRefs, now)
	if err != nil {
		return nil, err
	}
	if call.patch != nil {
		if err := call.patch.apply(&c, in.user, now); err != nil {
			return nil, err
		}
	}
	var write []model.LineRef
	if c.refs != nil {
		write = toRefs
	}
	if p := call.patch; p != nil && p.refsGiven {
		write = p.refs // 请求里显式给的线路优先于套餐的(给了空列表 = 不改,和以前一样)
	}
	oldName := ""
	if old != nil {
		oldName = old.Name
	}
	refsTo := in.fromRefs
	if write != nil {
		refsTo = write
	}
	res := &planResult{Op: call.opt.Op, Direction: c.direction, Periods: call.opt.Periods, Usage: c.usage,
		From: stateOf(in.user, oldName, in.fromRefs), To: stateOf(c.next, call.target.Name, refsTo),
		Remaining: c.remaining, Credit: c.credit, Warnings: c.warnings, calc: c, writeRefs: write}
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
// stack 的一次性额度结转也在同一条 UPDATE 里算(右边取的是更新前的值),按 model.MaxVolume 封顶:
// 整数加法溢出时 SQLite 会把结果存成 REAL,之后整表读用户都会失败。room = MaxVolume − N×V ≥ 0。
func planOpFields(c planCalc) map[string]interface{} {
	f := updateUserFields(c.next, c.reset)
	room := model.MaxVolume - c.carryAdd
	switch c.carry {
	case "keep":
		f["volume"] = gorm.Expr("MIN(volume, ?) + ?", room, c.carryAdd)
	case "reset":
		f["volume"] = gorm.Expr("MAX(1, MIN(MAX(0, volume - up - down), ?) + ?)", room, c.carryAdd)
	case "expired":
		f["volume"] = gorm.Expr("MAX(1, MIN(up + down, ?) + ?)", room, c.carryAdd)
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
			res, err = buildPlanResult(sc, in, call, toRefs, now)
			return err
		})
		if err != nil {
			return nil, err
		}
		res.DryRun, res.RequestId = true, call.requestID
		return res, nil
	}

	hash := call.reqHash
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
		res, err = buildPlanResult(sc, in, call, toRefs, now)
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
		if res.writeRefs != nil {
			if err := writeUserLineRefs(tx, uid, res.writeRefs); err != nil {
				return err
			}
		}
		if p := call.patch; p != nil { // PATCH 顺带的改名、外部节点
			if err := moveUserStats(tx, in.user.Name, res.calc.next.Name); err != nil {
				return err
			}
			if p.extIds != nil {
				if err := writeUserExts(tx, uid, *p.extIds); err != nil {
					return err
				}
			}
		}
		rec := newPlanOp(sc, call, hash, in.user, res, now)
		if err := tx.Create(&rec).Error; err != nil {
			return err
		}
		res.OpId = rec.Id
		if !call.quiet {
			action := call.audit
			if action == "" {
				action = "plan:" + call.opt.Op + ":" + call.target.Name
			}
			obj, _ := json.Marshal(res.calc.next.Name) // 和老的套用套餐审计同一格式:老库补「所属套餐」时按它解析
			return tx.Create(&model.Change{DateTime: now, Actor: actorOr(sc.actor), Key: "user", Action: action, Obj: obj}).Error
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
		s.reloadUsers("套餐 " + call.target.Name + " → " + res.calc.next.Name)
		if before.PlanId != call.target.Id {
			s.rulesNow() // 换了套餐,按套餐选人的限速规则要立刻重判
		}
	}
	if next := res.calc.next; res.calc.reset || (before.Volume > 0 && (next.Volume == 0 || next.Volume > before.Volume)) {
		s.forgetQuotaAlert(next.Name) // 用量清零或配额变大:再到阈值要能再提醒
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

// replayPlanOp 幂等键已经执行过:同内容原样重放,内容不同 409。调用方把它排在找套餐、校验期数、代理校验之前 ——
// 已经成功的请求重试时,套餐后来改了名或删了、天数改了、代理额度用完了,照样拿到 200 重放,而不是 4xx。
// 预览不看键(也不占用键)。没执行过返回 (nil, nil),由执行器在事务里再查一遍(并发的同键请求)。
func (s *Server) replayPlanOp(sc planScope, key, hash string, dryRun bool) (*planResult, error) {
	if key == "" || dryRun {
		return nil, nil
	}
	p, err := findPlanOp(s.db, sc.rid, key)
	if err != nil || p == nil {
		return nil, err
	}
	if p.ReqHash != hash {
		return nil, opErr(http.StatusConflict, "idempotency_conflict", errPlanIdemConflict)
	}
	r := planResultFromOp(*p)
	r.Replayed = true
	return r, nil
}

func newPlanOp(sc planScope, call planOpCall, hash string, u model.User, res *planResult, now int64) model.PlanOp {
	op := model.PlanOp{ResellerId: sc.rid, ReqHash: hash, UserId: u.Id, UserName: res.calc.next.Name, Actor: actorOr(sc.actor),
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

	// planGiven 请求体里有 planId / plan 字段(不管值是什么)。/plan、/renew 只有完全没带套餐字段才续当前套餐;
	// 带了却是 0、空串或 null 的,和以前一样 400。
	planGiven bool
}

// decodePlanOpReq 读请求体;空请求体等于 {}(续当前套餐时什么都不用带),不是 JSON 对象的 400。?dryRun=true 也算预览。
func decodePlanOpReq(r *http.Request) (planOpReq, error) {
	var req planOpReq
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return req, errors.New("请求体不是合法 JSON: " + err.Error())
		}
		body = bytes.TrimSpace(b)
	}
	if len(body) > 0 {
		if body[0] != '{' {
			return req, errPlanBody
		}
		var keys map[string]json.RawMessage
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&keys); err != nil {
			return req, errors.New("请求体不是合法 JSON: " + err.Error())
		}
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
			return req, errors.New("请求体不是合法 JSON: " + err.Error())
		}
		for k := range keys {
			if strings.EqualFold(k, "planId") || strings.EqualFold(k, "plan") { // 和 encoding/json 一样不分大小写
				req.planGiven = true
			}
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

// planPeriodsN 期数本身:默认 1,1–120(折算可以 0)。和套餐无关,能在找套餐之前校验。
func planPeriodsN(p *int, op string) (int, error) {
	n := 1
	if p != nil {
		n = *p
	}
	least := 1
	if op == "prorate" {
		least = 0
	}
	if n < least || n > maxPlanPeriods {
		return 0, opErr(http.StatusBadRequest, "bad_periods", errPlanBadPeriods)
	}
	return n, nil
}

// checkPlanDays 多期时 N × 套餐天数不超过 36500(一期不受限,和以前一样能续)。写成除法,天数再大也不会乘溢出。
func checkPlanDays(n, days int) error {
	if n > 1 && days > maxPlanDays/n {
		return opErr(http.StatusBadRequest, "bad_periods", errPlanBadPeriods)
	}
	return nil
}

// planPeriods 期数:默认 1,1–120(折算可以 0);多期时 N × 套餐天数不超过 36500。
func planPeriods(p *int, op string, days int) (int, error) {
	n, err := planPeriodsN(p, op)
	if err != nil {
		return 0, err
	}
	return n, checkPlanDays(n, days)
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
//
// 顺序:先校验请求本身(方式、期数、用量处理、幂等键的格式)→ 查幂等键(执行过就重放 / 409)→ 找目标套餐并核对归属
// → 按套餐天数校验期数 → 代理校验 → 执行。

// renewTarget 续费的目标套餐。给了就按给的找:外部 API(byScope)只在调用方作用域里找,和以前的 /plan 一致;面板按 id 找,
// 代理只认自己的。没给就是用户现在的套餐(current = true:执行时在事务里核对它没被换掉)。先核对归属再看别的 ——
// 代理拿别人的套餐 id 和拿不存在的 id 得到同一个错误,探不出别人有没有这个套餐、天数是多少。
func (s *Server) renewTarget(sc planScope, u model.User, req planOpReq, byScope bool) (model.Plan, bool, error) {
	notFound := opErr(http.StatusBadRequest, "plan_not_found", errPlanNotFound)
	if planSelector(req) != "current" {
		if byScope || req.PlanId == nil || *req.PlanId == 0 {
			p, err := s.apiFindPlan(apiUserReq{PlanId: req.PlanId, Plan: req.Plan}, sc.rid)
			if err != nil {
				return model.Plan{}, false, opErr(http.StatusBadRequest, "plan_not_found", err)
			}
			return *p, false, nil
		}
		p, err := s.loadPlan(*req.PlanId)
		if err != nil || (sc.rid > 0 && p.ResellerId != sc.rid) {
			return model.Plan{}, false, notFound
		}
		return p, false, nil
	}
	if u.PlanId == 0 {
		return model.Plan{}, false, opErr(http.StatusBadRequest, "plan_required", errPlanNoUserPlan)
	}
	p, err := s.loadPlan(u.PlanId)
	if err != nil || (sc.rid > 0 && p.ResellerId != sc.rid) {
		return model.Plan{}, false, notFound
	}
	return p, true, nil
}

// renewUserPlan 续费 / 延期 N 期。没带套餐字段就续用户现在的套餐(在执行的事务里核对);带了套餐字段却是 0 / 空 / null
// 的 400。byScope 为真(外部 API)时给的套餐只在调用方作用域里找,和以前的 /plan 一致;面板按 id 找,代理照旧只能用自己的套餐。
func (s *Server) renewUserPlan(r *http.Request, sc planScope, u model.User, req planOpReq, byScope bool) (*planResult, error) {
	mode := req.Mode
	if mode != "extend" {
		mode = "renew" // 未知值照旧按 renew(兼容老调用方)
	}
	sel := planSelector(req)
	if req.planGiven && sel == "current" {
		return nil, opErr(http.StatusBadRequest, "plan_required", errPlanRequired)
	}
	n, err := planPeriodsN(req.Periods, mode)
	if err != nil {
		return nil, err
	}
	key, err := planRequestID(r, req.RequestId)
	if err != nil {
		return nil, err
	}
	remark := planRemark(req.Remark)
	hash := planHash("plan", u.Id, sel, mode, n, fromOf(req.FromPlanId), remark)
	if prev, err := s.replayPlanOp(sc, key, hash, req.DryRun); prev != nil || err != nil {
		return prev, err
	}
	for try := 1; ; try++ {
		q, current, err := s.renewTarget(sc, u, req, byScope)
		if err != nil {
			return nil, err
		}
		if err := checkPlanDays(n, q.Days); err != nil {
			return nil, err
		}
		if sc.rid > 0 { // 代理:线路在授权内,代理本身可用(启用、未到期、额度未用尽)
			if err := s.checkResellerPlan(sc.rid, u, q); err != nil {
				return nil, opErr(http.StatusBadRequest, "reseller_denied", err)
			}
		}
		res, err := s.runPlanOp(sc, u.Id, planOpCall{endpoint: "plan", opt: planOpOpt{Op: mode, Periods: n}, target: q, current: current,
			fromPlan: req.FromPlanId, dryRun: req.DryRun, requestID: key, reqHash: hash, remark: remark})
		if !current || !errors.Is(err, errPlanMoved) || try >= planCurrentTries {
			return res, err
		}
		// 事务里发现所属套餐刚被换掉(或刚改过):按最新的用户重新解析、重新校验再来
		if e := s.db.First(&u, u.Id).Error; e != nil {
			return nil, opErr(http.StatusNotFound, "", errPlanUserGone)
		}
	}
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
	sel := planSelector(req)
	if sel == "current" {
		return nil, opErr(http.StatusBadRequest, "plan_required", errPlanRequired)
	}
	n, err := planPeriodsN(req.Periods, mode)
	if err != nil {
		return nil, err
	}
	key, err := planRequestID(r, req.RequestId)
	if err != nil {
		return nil, err
	}
	remark := planRemark(req.Remark)
	hash := planHash("change", u.Id, sel, mode, n, usage, fromOf(req.FromPlanId), req.Force, remark)
	if prev, err := s.replayPlanOp(sc, key, hash, req.DryRun); prev != nil || err != nil {
		return prev, err
	}
	p, err := s.apiFindPlan(apiUserReq{PlanId: req.PlanId, Plan: req.Plan}, u.ResellerId)
	if err != nil {
		return nil, opErr(http.StatusBadRequest, "plan_not_found", err)
	}
	if err := checkPlanDays(n, p.Days); err != nil {
		return nil, err
	}
	if sc.rid > 0 {
		if err := s.checkResellerPlan(sc.rid, u, *p); err != nil {
			return nil, opErr(http.StatusBadRequest, "reseller_denied", err)
		}
	}
	return s.runPlanOp(sc, u.Id, planOpCall{endpoint: "change", opt: planOpOpt{Op: mode, Periods: n, Usage: usage}, target: *p,
		fromPlan: req.FromPlanId, force: req.Force, dryRun: req.DryRun, requestID: key, reqHash: hash, remark: remark})
}

// ---- 带套餐的 PATCH / PUT /users ----

// userPatch 修改用户时同时带了套餐:先按套餐续一期,再把请求里显式给出的字段盖上去(显式字段优先于套餐),
// 和以前同一套规则;线路、改名、外部节点也在执行器的同一个事务里写。
type userPatch struct {
	req       apiUserReq
	rename    string          // 改名(已校验、已去空格);空 = 不改
	refs      []model.LineRef // 请求里显式给的线路(已整理);refsGiven 为假时不看
	refsGiven bool
	extIds    *[]uint // 只有主面板令牌能改
}

// apply 在套餐算好的 next 上盖显式字段;before 是套用前的用户(days 要从套用前的到期起算)。
func (p *userPatch) apply(c *planCalc, before model.User, now int64) error {
	next := &c.next
	if p.req.Days != nil && p.req.Expiry == nil {
		// 同时给了 days 就由它替代套餐自带的天数(同样从原到期起算)。以前在套餐延好的到期上再加一遍,多给一个周期(审计 MB09)
		next.Expiry = before.Expiry
	}
	if p.rename != "" {
		next.Name = p.rename
		if next.ActivateDays > 0 {
			next.Expiry = 0 // 以前改名会过一遍 validateUser,它顺带做这一步
		}
	}
	if err := applyReq(next, p.req, now, false); err != nil {
		return opErr(http.StatusBadRequest, "", err)
	}
	return nil
}

// apiUpdateUserPlan PATCH / PUT /users/{name|id} 同时带了 planId / plan:等同一次续费(renew / extend 一期),
// 走和 /plan 同一个执行器 —— 事务里重读用户、记套餐记录、认幂等键(requestId 或 Idempotency-Key);请求与响应的格式不变:
// 请求里其它显式字段照旧盖在套餐上面,返回用户对象,审计仍记 update:plan:<套餐名>。
func (s *Server) apiUpdateUserPlan(w http.ResponseWriter, r *http.Request, u model.User, sc apiScope, req apiUserReq, plan model.Plan) {
	mode := req.Mode
	if mode != "extend" {
		mode = "renew"
	}
	key, err := planRequestID(r, req.RequestId)
	if err != nil {
		writePlanErr(w, err)
		return
	}
	ps := planScope{rid: sc.rid, actor: sc.actor}
	canon := req
	canon.RequestId = ""
	hash := planHash("update", u.Id, canon)
	if prev, err := s.replayPlanOp(ps, key, hash, false); prev != nil || err != nil {
		if err != nil {
			writePlanErr(w, err)
			return
		}
		s.writeFreshUser(w, u.Id, true)
		return
	}
	// 事务外先在请求开始时读到的用户上算一遍:校验显式字段、改名、代理授权(都要用 s.db 读);落库时执行器按事务里重读的用户再算
	now := time.Now().Unix()
	pre, err := calcPlanOp(u, nil, plan, planOpOpt{Op: mode, Periods: 1}, nil, now)
	if err != nil {
		writePlanErr(w, err)
		return
	}
	patch := &userPatch{req: req}
	if req.Name != nil && *req.Name != u.Name {
		tmp := pre.next
		tmp.Name = *req.Name
		if err := s.validateUser(&tmp); err != nil {
			badRequest(w, err)
			return
		}
		patch.rename = tmp.Name
	}
	if err := patch.apply(&pre, u, now); err != nil {
		writePlanErr(w, err)
		return
	}
	refs := pre.refs // 套餐带线路就换成套餐的,请求里显式给了以请求为准
	switch {
	case req.LineRefs != nil:
		refs, patch.refsGiven = *req.LineRefs, true
	case req.LineIds != nil:
		refs, patch.refsGiven = lineRefsOf(*req.LineIds, nil), true
	}
	if sc.rid > 0 {
		check := refs
		if check == nil { // 没改线路就按现有的校验(已与当前授权取交集,收回的线路不算越权)
			check = s.userLineRefs(u.Id)
		}
		if err := s.checkResellerUser(sc.rid, u.Id, &pre.next, check); err != nil {
			badRequest(w, err)
			return
		}
	}
	if patch.refsGiven {
		patch.refs = s.normalizeRefs(refs) // 给了空列表整理出来是 nil:不改线路,和以前一样
	}
	if req.ExtIds != nil && sc.rid == 0 {
		patch.extIds = req.ExtIds
	}
	res, err := s.runPlanOp(ps, u.Id, planOpCall{endpoint: "update", opt: planOpOpt{Op: mode, Periods: 1}, target: plan,
		requestID: key, reqHash: hash, patch: patch, audit: "update:plan:" + plan.Name})
	if err != nil {
		writePlanErr(w, err)
		return
	}
	s.writeFreshUser(w, u.Id, res.Replayed)
}

// writeFreshUser 返回库里的最新用户对象(含统计这期间记的流量);重放时带 Idempotent-Replayed。
func (s *Server) writeFreshUser(w http.ResponseWriter, id uint, replayed bool) {
	var fresh model.User
	if err := s.db.First(&fresh, id).Error; err != nil {
		writePlanErr(w, opErr(http.StatusNotFound, "", errPlanUserGone))
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, s.apiUser(fresh))
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

// apiRenew 外部 API:POST /users/{name|id}/plan(别名 /renew)。正式执行与重放都返回用户对象(和以前一样),预览返回结果对象。
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
	s.writeFreshUser(w, u.Id, res.Replayed)
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
	if rid > 0 {
		// 不是他自己操作的记录:操作人显示「主面板」,不是他的套餐不显示名字(主面板可能给他的用户套过主面板的套餐)。
		// 他自己的记录在写入时已经按他的视角处理过
		var mine []uint
		s.db.Model(&model.Plan{}).Where("reseller_id = ?", rid).Pluck("id", &mine)
		own := make(map[uint]bool, len(mine))
		for _, id := range mine {
			own[id] = true
		}
		for i := range ops {
			if ops[i].ResellerId == rid {
				continue
			}
			ops[i].Actor = planOpMasterTag
			if !own[ops[i].FromPlanId] {
				ops[i].FromPlan, ops[i].Before = "", hidePlanName(ops[i].Before)
			}
			if !own[ops[i].ToPlanId] {
				ops[i].ToPlan, ops[i].After = "", hidePlanName(ops[i].After)
			}
		}
	}
	writeJSON(w, http.StatusOK, ops)
}

// hidePlanName 把记录里变更前 / 后状态的套餐名抹掉(套餐 id 照留)。
func hidePlanName(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return raw
	}
	m["plan"] = json.RawMessage(`""`)
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}
