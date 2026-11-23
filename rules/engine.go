package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
)

// Engine 主机上的规则判定器。每轮(统计任务之后,10 秒一次)调用 Tick:
//   - 时段规则:算此刻在不在窗口内,只在翻转的那一刻写入 / 删除状态;
//   - 突发规则:对目标用户采一次用量样本,滑动窗口内的增量达到阈值就写一条带到期时间的状态,到期删除;
//   - 规则停用 / 删除、用户停用 / 删除、不再是目标:它们的状态一并清掉。
//
// 状态表是唯一事实,窗口样本只在内存里(进程重启后窗口从空开始,跨过重启那一刻的突发可能漏判一次)。
type Engine struct {
	DB       *gorm.DB
	Location func() *time.Location // 面板时区(时段规则按它算);nil = 本机
	Notify   func(text string)     // 突发触发时的通知;nil = 不通知
	// Usage 「用户 × 线路」的用量(勾了「忽略部分线路」的突发规则按它算,见 LineUsage);nil = 这种规则也按用户总用量算。
	// 副机报上来的由 Hub 交给它;主机自己的每轮从 LocalMeter 取(nil = 不取)。
	Usage      *LineUsage
	LocalMeter func() (node uint, ratio float64, m model.LineMeter, ok bool)

	mu      sync.Mutex
	windows map[wkey]*window
	warned  map[uint]bool // 上一轮读不出来的规则(告警只在刚坏掉时打一次)
}

type wkey struct{ rule, user uint }

type sample struct{ ts, total int64 }

// window 一个"规则 × 用户"的滑动窗口:最近 span 秒内的用量样本。sig 是建窗口时规则忽略的线路(规整后的 json):
// 忽略的线路一变,用量的算法就变了(换了一个累计),旧样本不能和新样本相减,窗口从头算。
type window struct {
	samples []sample
	sig     string
}

// add 记一个样本,返回窗口内的增量(最新减最早)。用量变小(周期重置 / 手动清零)时窗口作废重来。
func (w *window) add(ts, total, span int64) int64 {
	if n := len(w.samples); n > 0 && total < w.samples[n-1].total {
		w.samples = w.samples[:0]
	}
	w.samples = append(w.samples, sample{ts, total})
	cut := ts - span
	i := 0
	for i < len(w.samples)-1 && w.samples[i].ts < cut {
		i++
	}
	w.samples = w.samples[i:]
	return total - w.samples[0].total
}

// rebase 解除限速时把窗口收成"当前这一点":之后的增量只算解除之后跑的流量,同一段突发不会刚解除又触发。
func (w *window) rebase() {
	if n := len(w.samples); n > 1 {
		w.samples = w.samples[n-1:]
	}
}

// Tick 判一轮;返回状态表有没有变化(变了调用方要重新下发限速,快照也会随之推给副机)。
func (e *Engine) Tick(now time.Time) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.windows == nil {
		e.windows = map[wkey]*window{}
	}
	ruleList, broken, err := e.loadRules()
	if err != nil {
		return false, err
	}
	var states []model.LimitState
	if err := e.DB.Find(&states).Error; err != nil {
		return false, err
	}
	if len(ruleList) == 0 && len(states) == 0 {
		e.windows = map[wkey]*window{}
		if e.Usage != nil {
			e.Usage.SetOn(false)
		}
		return false, nil
	}
	var users []model.User
	if len(ruleList) > 0 {
		if err := e.DB.Select("id, name, reseller_id, plan_id, enabled, up, down").Find(&users).Error; err != nil {
			return false, err
		}
	}
	// 忽略的线路只认现在还在的线路(线路删掉以后规则里留下的 id 不算数):一条都不剩就当没勾
	skips := make(map[uint]json.RawMessage, len(ruleList))
	var lineIDs map[uint]bool
	meterOn := false
	for _, rule := range ruleList {
		set := SkipSet(rule.SkipLineIds)
		if len(set) == 0 {
			continue
		}
		if lineIDs == nil {
			var ids []uint
			if err := e.DB.Model(&model.Line{}).Pluck("id", &ids).Error; err != nil {
				return false, err
			}
			lineIDs = make(map[uint]bool, len(ids))
			for _, id := range ids {
				lineIDs[id] = true
			}
		}
		if raw := SkipJSON(set, lineIDs); raw != nil {
			skips[rule.Id] = raw
			meterOn = meterOn || rule.Kind == KindBurst
		}
	}
	// 「用户 × 线路」的用量只在有突发规则用得着时才收(主机自己的这一份每轮取一次;副机的随报告由 Hub 交进来)
	if e.Usage != nil {
		e.Usage.SetOn(meterOn)
		if meterOn {
			if e.LocalMeter != nil {
				if node, ratio, m, ok := e.LocalMeter(); ok {
					e.Usage.Feed(node, ratio, m)
				}
			}
			e.Usage.Prune()
		}
	}
	loc := time.Local
	if e.Location != nil {
		if l := e.Location(); l != nil {
			loc = l
		}
	}
	nowU := now.Unix()
	existing := map[wkey]model.LimitState{}
	for _, st := range states {
		existing[wkey{st.RuleId, st.UserId}] = st
	}
	desired := map[wkey]model.LimitState{}
	keep := map[wkey]bool{}
	// 读不出来的规则:它名下生效中的状态原样保留(不能因为一行坏数据就悄悄解除),到期的突发状态照常解除;
	// 它的突发窗口也留着,修好后接着算
	for k, st := range existing {
		if broken[k.rule] && (st.Until == 0 || st.Until > nowU) {
			desired[k] = st
		}
	}
	for k := range e.windows {
		if broken[k.rule] {
			keep[k] = true
		}
	}
	var notes []string
	for _, rule := range ruleList {
		targets := Targets(rule, users)
		skipRaw := skips[rule.Id] // 这条规则忽略的线路(规整过、只剩还在的线路;nil = 不忽略)
		switch rule.Kind {
		case KindSchedule:
			if !InWindow(rule, now, loc) {
				continue
			}
			for _, u := range targets {
				k := wkey{rule.Id, u.Id}
				st, ok := existing[k]
				if !ok {
					st = model.LimitState{RuleId: rule.Id, UserId: u.Id, Since: nowU}
				}
				// 原因与 Until 也要跟着规则走:突发规则改成时段规则时,旧行带着惩罚截止时间,过点后被当成已解除(审计 MB12)
				st.Reason = "时段 " + rule.Start + " 到 " + rule.End
				st.UserName, st.RuleName, st.UpMbps, st.DownMbps, st.TightenOnly, st.Until = u.Name, rule.Name, rule.UpMbps, rule.DownMbps, rule.TightenOnly, 0
				st.SkipLineIds = skipRaw
				desired[k] = st
			}
		case KindBurst:
			span := int64(rule.WindowMin) * 60
			skip := SkipSet(skipRaw)
			for _, u := range targets {
				k := wkey{rule.Id, u.Id}
				keep[k] = true
				w := e.windows[k]
				if w == nil {
					w = &window{sig: string(skipRaw)}
					e.windows[k] = w
				}
				if w.sig != string(skipRaw) { // 忽略的线路改了:用量换了算法,旧样本作废,窗口从头算
					w.samples, w.sig = nil, string(skipRaw)
				}
				// 用量:平常是用户表里的总用量(上下行合计);勾了忽略的线路时只加没被忽略的那些线路上的
				total := u.Up + u.Down
				if len(skip) > 0 && e.Usage != nil {
					total = e.Usage.Sum(u.Name, skip)
				}
				delta := w.add(nowU, total, span)
				if st, ok := existing[k]; ok {
					if st.Until > nowU { // 惩罚期内:保留,顺手同步规则里可能改过的限值与忽略的线路
						st.UserName, st.RuleName, st.UpMbps, st.DownMbps, st.TightenOnly = u.Name, rule.Name, rule.UpMbps, rule.DownMbps, rule.TightenOnly
						st.SkipLineIds = skipRaw
						desired[k] = st
					} else {
						w.rebase() // 到期解除:之后的增量从现在算
					}
					continue
				}
				if rule.ThresholdBytes > 0 && delta >= rule.ThresholdBytes {
					until := nowU + int64(rule.PenaltyMin)*60
					reason := fmt.Sprintf("%d 分钟内 %s", rule.WindowMin, HumanBytes(delta))
					desired[k] = model.LimitState{RuleId: rule.Id, UserId: u.Id, UserName: u.Name, RuleName: rule.Name,
						UpMbps: rule.UpMbps, DownMbps: rule.DownMbps, TightenOnly: rule.TightenOnly, Since: nowU, Until: until, Reason: reason,
						SkipLineIds: skipRaw}
					notes = append(notes, fmt.Sprintf("⏱ <b>突发限速</b>:%s 触发「%s」(%s),%s,%s 解除",
						escapeHTML(u.Name), escapeHTML(rule.Name), reason, LimitText(rule.UpMbps, rule.DownMbps), time.Unix(until, 0).In(loc).Format("15:04")))
				}
			}
		}
	}
	for k := range e.windows { // 不再是突发目标的窗口丢掉
		if !keep[k] {
			delete(e.windows, k)
		}
	}

	var add, upd, lifted []model.LimitState
	for k, st := range desired {
		old, ok := existing[k]
		if !ok {
			add = append(add, st)
			continue
		}
		if old.UpMbps != st.UpMbps || old.DownMbps != st.DownMbps || old.TightenOnly != st.TightenOnly || old.RuleName != st.RuleName || old.UserName != st.UserName ||
			old.Until != st.Until || old.Reason != st.Reason || !bytes.Equal(old.SkipLineIds, st.SkipLineIds) {
			st.Id = old.Id
			upd = append(upd, st)
		}
	}
	for k, st := range existing {
		if _, ok := desired[k]; !ok {
			lifted = append(lifted, st)
		}
	}
	if len(add)+len(upd)+len(lifted) == 0 {
		return false, nil
	}
	sort.Slice(add, func(i, j int) bool { return add[i].UserId < add[j].UserId })
	sort.Slice(lifted, func(i, j int) bool { return lifted[i].UserId < lifted[j].UserId })
	err = e.DB.Transaction(func(tx *gorm.DB) error {
		if len(add) > 0 {
			// 分批:一行 11 个变量,时段规则选「全部用户」时一条 INSERT 写几千行会超过 SQLite 一条语句 32766 个变量的上限,
			// 整笔失败,规则永远不生效、也不会随快照下发到副机(审计 I4)
			if err := tx.CreateInBatches(&add, stateBatch).Error; err != nil {
				return err
			}
		}
		for _, st := range upd {
			if err := tx.Model(&model.LimitState{}).Where("id = ?", st.Id).Updates(map[string]interface{}{
				"user_name": st.UserName, "rule_name": st.RuleName, "up_mbps": st.UpMbps, "down_mbps": st.DownMbps, "tighten_only": st.TightenOnly,
				"until": st.Until, "reason": st.Reason, "skip_line_ids": st.SkipLineIds,
			}).Error; err != nil {
				return err
			}
		}
		if len(lifted) > 0 {
			ids := make([]uint, 0, len(lifted))
			for _, st := range lifted {
				ids = append(ids, st.Id)
			}
			for len(ids) > 0 {
				k := min(len(ids), stateBatch)
				if err := tx.Where("id IN ?", ids[:k]).Delete(&model.LimitState{}).Error; err != nil {
					return err
				}
				ids = ids[k:]
			}
		}
		for _, st := range add {
			record(tx, nowU, "apply", fmt.Sprintf("%s ← %s(%s,%s)", st.UserName, st.RuleName, st.Reason, LimitText(st.UpMbps, st.DownMbps)))
		}
		for _, st := range lifted {
			record(tx, nowU, "lift", fmt.Sprintf("%s ← %s", st.UserName, st.RuleName))
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if e.Notify != nil {
		for _, n := range notes {
			e.Notify(n)
		}
	}
	return true, nil
}

// loadRules 逐行读启用中的规则:一行读不出来(手工改库、user_ids 存成了别的类型等)只跳过这一行并告警,
// 不让整轮判定失败(审计 Q-33①)。broken 是读不出来的规则 id;同一条只在刚坏掉时告警一次,修好后清掉记号。
func (e *Engine) loadRules() (list []model.Rule, broken map[uint]bool, err error) {
	var ids []uint
	if err := e.DB.Model(&model.Rule{}).Where("enabled = ?", true).Order("id asc").Pluck("id", &ids).Error; err != nil {
		return nil, nil, err
	}
	broken = map[uint]bool{}
	for _, id := range ids {
		var r model.Rule
		if err := e.DB.Where("id = ?", id).Take(&r).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue // 两次查询之间被删了
			}
			broken[id] = true
			if !e.warned[id] {
				logger.Warningf("限速规则 #%d 读不出来,本轮跳过这一条(它名下生效中的限速保持不动,到期的突发限速照常解除),请在规则页重新保存或删除它: %v", id, err)
			}
			continue
		}
		list = append(list, r)
	}
	e.warned = broken
	return list, broken, nil
}

// stateBatch 写入 / 撤掉限速状态时每条语句的行数(SQLite 一条语句最多 32766 个变量)。
const stateBatch = 500

// record 写一条操作审计(和执法任务同一张表、同一个页面看)。
func record(tx *gorm.DB, now int64, action string, text string) {
	b, _ := json.Marshal(text)
	tx.Create(&model.Change{DateTime: now, Actor: model.ActorRuleJob, Key: "rule", Action: action, Obj: b})
}

func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
