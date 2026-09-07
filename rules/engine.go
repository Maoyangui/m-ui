package rules

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

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

	mu      sync.Mutex
	windows map[wkey]*window
}

type wkey struct{ rule, user uint }

type sample struct{ ts, total int64 }

// window 一个"规则 × 用户"的滑动窗口:最近 span 秒内的用量样本。
type window struct{ samples []sample }

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
	var ruleList []model.Rule
	if err := e.DB.Where("enabled = ?", true).Order("id asc").Find(&ruleList).Error; err != nil {
		return false, err
	}
	var states []model.LimitState
	if err := e.DB.Find(&states).Error; err != nil {
		return false, err
	}
	if len(ruleList) == 0 && len(states) == 0 {
		e.windows = map[wkey]*window{}
		return false, nil
	}
	var users []model.User
	if len(ruleList) > 0 {
		if err := e.DB.Select("id, name, reseller_id, enabled, up, down").Find(&users).Error; err != nil {
			return false, err
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
	var notes []string
	for _, rule := range ruleList {
		targets := Targets(rule, users)
		switch rule.Kind {
		case KindSchedule:
			if !InWindow(rule, now, loc) {
				continue
			}
			for _, u := range targets {
				k := wkey{rule.Id, u.Id}
				st, ok := existing[k]
				if !ok {
					st = model.LimitState{RuleId: rule.Id, UserId: u.Id, Since: nowU, Reason: "时段 " + rule.Start + " 到 " + rule.End}
				}
				st.UserName, st.RuleName, st.UpMbps, st.DownMbps, st.TightenOnly, st.Until = u.Name, rule.Name, rule.UpMbps, rule.DownMbps, rule.TightenOnly, 0
				desired[k] = st
			}
		case KindBurst:
			span := int64(rule.WindowMin) * 60
			for _, u := range targets {
				k := wkey{rule.Id, u.Id}
				keep[k] = true
				w := e.windows[k]
				if w == nil {
					w = &window{}
					e.windows[k] = w
				}
				delta := w.add(nowU, u.Up+u.Down, span)
				if st, ok := existing[k]; ok {
					if st.Until > nowU { // 惩罚期内:保留,顺手同步规则里可能改过的限值
						st.UserName, st.RuleName, st.UpMbps, st.DownMbps, st.TightenOnly = u.Name, rule.Name, rule.UpMbps, rule.DownMbps, rule.TightenOnly
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
						UpMbps: rule.UpMbps, DownMbps: rule.DownMbps, TightenOnly: rule.TightenOnly, Since: nowU, Until: until, Reason: reason}
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
		if old.UpMbps != st.UpMbps || old.DownMbps != st.DownMbps || old.TightenOnly != st.TightenOnly || old.RuleName != st.RuleName || old.UserName != st.UserName {
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
	err := e.DB.Transaction(func(tx *gorm.DB) error {
		if len(add) > 0 {
			if err := tx.Create(&add).Error; err != nil {
				return err
			}
		}
		for _, st := range upd {
			if err := tx.Model(&model.LimitState{}).Where("id = ?", st.Id).Updates(map[string]interface{}{
				"user_name": st.UserName, "rule_name": st.RuleName, "up_mbps": st.UpMbps, "down_mbps": st.DownMbps, "tighten_only": st.TightenOnly,
			}).Error; err != nil {
				return err
			}
		}
		if len(lifted) > 0 {
			ids := make([]uint, 0, len(lifted))
			for _, st := range lifted {
				ids = append(ids, st.Id)
			}
			if err := tx.Where("id IN ?", ids).Delete(&model.LimitState{}).Error; err != nil {
				return err
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

// record 写一条操作审计(和执法任务同一张表、同一个页面看)。
func record(tx *gorm.DB, now int64, action string, text string) {
	b, _ := json.Marshal(text)
	tx.Create(&model.Change{DateTime: now, Actor: "RuleJob", Key: "rule", Action: action, Obj: b})
}

func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
