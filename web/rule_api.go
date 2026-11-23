package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/rules"
)

// 规则页接口(主面板专属):增删改查规则、看生效中的状态。规则由主机判定,改完立刻判一轮,不用等下一轮统计。

type rulePayload struct {
	Name        string  `json:"name"`
	Enabled     *bool   `json:"enabled"`
	Kind        string  `json:"kind"`
	AllUsers    bool    `json:"allUsers"`
	UserIds     []uint  `json:"userIds"`
	ResellerIds []uint  `json:"resellerIds"`
	PlanIds     []uint  `json:"planIds"`
	Days        string  `json:"days"`
	Start       string  `json:"start"`
	End         string  `json:"end"`
	WindowMin   int     `json:"windowMin"`
	ThresholdGB float64 `json:"thresholdGb"`
	PenaltyMin  int     `json:"penaltyMin"`
	UpMbps      int     `json:"upMbps"`
	DownMbps    int     `json:"downMbps"`
	TightenOnly *bool   `json:"tightenOnly"`
	// SkipLineIds 忽略的线路(界面上「忽略部分线路」勾选的):走这些线路的连接不受这条规则限速,突发规则也不把它们的流量计入用量。
	// 不给 = 修改时沿用原值、新建时不忽略;给空数组 = 不忽略
	SkipLineIds *[]uint `json:"skipLineIds"`
	Remark      string  `json:"remark"`
	Sort        int     `json:"sort"`
}

// ruleView 列表 / 详情:目标 id 展开成数组,阈值换算成 GB,再带上目标人数与生效人数。
type ruleView struct {
	model.Rule
	UserIds     []uint  `json:"userIds"`
	ResellerIds []uint  `json:"resellerIds"`
	PlanIds     []uint  `json:"planIds"`
	SkipLineIds []uint  `json:"skipLineIds"`
	ThresholdGB float64 `json:"thresholdGb"`
	TargetCount int     `json:"targetCount"`
	ActiveCount int     `json:"activeCount"`
}

const bytesPerGB = 1 << 30

// toRule 校验并生成规则;cur 非 nil 表示修改,没给的开关沿用原值。
func (p rulePayload) toRule(cur *model.Rule) (model.Rule, error) {
	r := model.Rule{}
	if cur != nil {
		r = *cur
	}
	r.Name = strings.TrimSpace(p.Name)
	if r.Name == "" || len([]rune(r.Name)) > 64 {
		return r, errors.New("规则名称不能为空,最多 64 字")
	}
	if p.Kind != rules.KindSchedule && p.Kind != rules.KindBurst {
		return r, errors.New("规则类型只能是 schedule(时段)或 burst(突发)")
	}
	r.Kind = p.Kind
	if p.Enabled != nil {
		r.Enabled = *p.Enabled
	} else if cur == nil {
		r.Enabled = true
	}
	if p.TightenOnly != nil {
		r.TightenOnly = *p.TightenOnly
	} else if cur == nil {
		r.TightenOnly = true
	}
	if p.UpMbps < 0 || p.DownMbps < 0 {
		return r, errors.New("限速不能为负")
	}
	if p.UpMbps == 0 && p.DownMbps == 0 {
		return r, errors.New("上行与下行至少填一个限速")
	}
	r.UpMbps, r.DownMbps = p.UpMbps, p.DownMbps
	if !p.AllUsers && len(p.UserIds) == 0 && len(p.ResellerIds) == 0 && len(p.PlanIds) == 0 {
		return r, errors.New("至少选一个目标:全部用户、指定用户、某个代理名下的用户或某个套餐的用户")
	}
	r.AllUsers = p.AllUsers
	r.UserIds, r.ResellerIds, r.PlanIds = nil, nil, nil
	if !p.AllUsers {
		r.UserIds, _ = json.Marshal(uniqueIDs(p.UserIds))
		r.ResellerIds, _ = json.Marshal(uniqueIDs(p.ResellerIds))
		r.PlanIds, _ = json.Marshal(uniqueIDs(p.PlanIds))
	}
	switch r.Kind {
	case rules.KindSchedule:
		if rules.ParseHM(p.Start) < 0 || rules.ParseHM(p.End) < 0 {
			return r, errors.New("开始 / 结束时刻要写成 HH:MM")
		}
		r.Start, r.End = strings.TrimSpace(p.Start), strings.TrimSpace(p.End)
		days := rules.DaySet(p.Days)
		keys := make([]int, 0, len(days))
		for d := range days {
			keys = append(keys, d)
		}
		sort.Ints(keys)
		parts := make([]string, 0, len(keys))
		for _, d := range keys {
			parts = append(parts, strconv.Itoa(d))
		}
		r.Days = strings.Join(parts, ",")
		r.WindowMin, r.ThresholdBytes, r.PenaltyMin = 0, 0, 0
	case rules.KindBurst:
		if p.WindowMin < 1 || p.WindowMin > 1440 {
			return r, errors.New("统计窗口要在 1 到 1440 分钟之间")
		}
		if p.ThresholdGB <= 0 || p.ThresholdGB > 100000 {
			return r, errors.New("触发流量要大于 0 GB")
		}
		if p.PenaltyMin < 1 || p.PenaltyMin > 10080 {
			return r, errors.New("限速时长要在 1 到 10080 分钟之间")
		}
		r.WindowMin, r.PenaltyMin = p.WindowMin, p.PenaltyMin
		r.ThresholdBytes = int64(math.Round(p.ThresholdGB * bytesPerGB))
		r.Days, r.Start, r.End = "", "", ""
	}
	r.Remark, r.Sort = strings.TrimSpace(p.Remark), p.Sort
	return r, nil
}

// skipLines 校验并规整「忽略的线路」。want 为 nil(请求里没给):修改时沿用原值,新建时不忽略。
// 新勾的线路必须存在;原来勾着、后来被删掉的线路悄悄去掉(规则页的开关是把整条规则原样送回来的,不能因此报错)。
func (s *Server) skipLines(want *[]uint, cur *model.Rule) (json.RawMessage, error) {
	if want == nil {
		if cur == nil {
			return nil, nil
		}
		return cur.SkipLineIds, nil
	}
	set := map[uint]bool{}
	for _, id := range *want {
		if id != 0 {
			set[id] = true
		}
	}
	if len(set) == 0 {
		return nil, nil
	}
	var ids []uint
	if err := s.db.Model(&model.Line{}).Pluck("id", &ids).Error; err != nil {
		return nil, fmt.Errorf("读取线路: %w", err) // 界面按这个前缀翻译(errmsg.js)
	}
	have := make(map[uint]bool, len(ids))
	for _, id := range ids {
		have[id] = true
	}
	var was map[uint]bool
	if cur != nil {
		was = rules.SkipSet(cur.SkipLineIds)
	}
	for id := range set {
		if !have[id] && !was[id] {
			return nil, fmt.Errorf("线路 #%d 不存在", id)
		}
	}
	return rules.SkipJSON(set, have), nil
}

func (s *Server) ruleViews(list []model.Rule) []ruleView {
	var users []model.User
	s.db.Select("id, enabled, reseller_id, plan_id").Find(&users)
	type cnt struct {
		RuleId uint
		N      int
	}
	var counts []cnt
	s.db.Model(&model.LimitState{}).Select("rule_id, COUNT(*) AS n").Where("until = 0 OR until > ?", time.Now().Unix()).Group("rule_id").Scan(&counts)
	active := map[uint]int{}
	for _, c := range counts {
		active[c.RuleId] = c.N
	}
	out := make([]ruleView, 0, len(list))
	for _, r := range list {
		v := ruleView{Rule: r, UserIds: rules.ParseIDs(r.UserIds), ResellerIds: rules.ParseIDs(r.ResellerIds), PlanIds: rules.ParseIDs(r.PlanIds), SkipLineIds: rules.ParseIDs(r.SkipLineIds),
			ThresholdGB: math.Round(float64(r.ThresholdBytes)/bytesPerGB*100) / 100, ActiveCount: active[r.Id]}
		if v.UserIds == nil {
			v.UserIds = []uint{}
		}
		if v.ResellerIds == nil {
			v.ResellerIds = []uint{}
		}
		if v.PlanIds == nil {
			v.PlanIds = []uint{}
		}
		if v.SkipLineIds == nil {
			v.SkipLineIds = []uint{}
		}
		if r.Enabled {
			v.TargetCount = len(rules.Targets(r, users))
		}
		out = append(out, v)
	}
	return out
}

// rulesNow 规则变了立刻判一轮并重新下发限速(测试里没有数据面)。
func (s *Server) rulesNow() {
	if s.onRulesNow != nil {
		s.onRulesNow()
	}
	if s.run != nil {
		s.run.RulesNow()
	}
}

// applyLimitsNow 按库里的规则状态重新下发本机限速(测试里没有数据面)。
func (s *Server) applyLimitsNow() {
	if s.run != nil {
		s.run.ApplyLimits()
	}
}

// handleRules GET /rules 列表;POST /rules 新建
func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var list []model.Rule
		s.db.Order("sort asc, id asc").Find(&list)
		writeJSON(w, http.StatusOK, s.ruleViews(list))
	case http.MethodPost:
		var p rulePayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		rule, err := p.toRule(nil)
		if err == nil {
			rule.SkipLineIds, err = s.skipLines(p.SkipLineIds, nil)
		}
		if err != nil {
			badRequest(w, err)
			return
		}
		rule.CreatedAt = time.Now().Unix()
		if err := s.db.Create(&rule).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "rule", "create", rule.Name)
		s.rulesNow()
		writeJSON(w, http.StatusOK, s.ruleViews([]model.Rule{rule})[0])
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// handleRuleItem GET/PUT/DELETE /rules/{id};GET /rules/{id}/states 该规则生效中的用户
func (s *Server) handleRuleItem(w http.ResponseWriter, r *http.Request) {
	idx := strings.Index(r.URL.Path, "/rules/")
	if idx < 0 {
		badRequest(w, errors.New("路径无效"))
		return
	}
	parts := strings.SplitN(strings.Trim(r.URL.Path[idx+len("/rules/"):], "/"), "/", 2)
	id64, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || id64 == 0 {
		badRequest(w, fmt.Errorf("id 无效: %s", parts[0]))
		return
	}
	id := uint(id64)
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	var cur model.Rule
	if err := s.db.First(&cur, id).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "规则不存在"})
		return
	}
	if sub == "states" {
		var st []model.LimitState
		s.db.Where("rule_id = ? AND (until = 0 OR until > ?)", id, time.Now().Unix()).Order("since desc").Find(&st)
		writeJSON(w, http.StatusOK, st)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.ruleViews([]model.Rule{cur})[0])
	case http.MethodPut:
		var p rulePayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		rule, err := p.toRule(&cur)
		if err == nil {
			rule.SkipLineIds, err = s.skipLines(p.SkipLineIds, &cur)
		}
		if err != nil {
			badRequest(w, err)
			return
		}
		// plan_ids 以前漏在这张单子外面:改规则时换的套餐目标存不进去(新建时存得进去)
		if err := s.db.Model(&model.Rule{}).Where("id = ?", id).Select(
			"name", "enabled", "kind", "all_users", "user_ids", "reseller_ids", "plan_ids", "days", "start", "end",
			"window_min", "threshold_bytes", "penalty_min", "up_mbps", "down_mbps", "tighten_only", "skip_line_ids", "remark", "sort",
		).Updates(rule).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "rule", "update", rule.Name)
		s.rulesNow()
		writeJSON(w, http.StatusOK, s.ruleViews([]model.Rule{rule})[0])
	case http.MethodDelete:
		if err := s.db.Delete(&model.Rule{}, id).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.db.Where("rule_id = ?", id).Delete(&model.LimitState{}) // 它造成的限速立刻解除
		s.audit(r, "rule", "delete", cur.Name)
		s.rulesNow()
		// 状态是上面直接删的,判定那一轮看不出有变化、不会重新下发:本机数据面要显式刷一次,
		// 否则本机一直按删掉的规则限速(副机随快照同步,不受影响)
		s.applyLimitsNow()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// handleLimitStates GET /limitstates 全部生效中的规则限速(用户页据此打"限速中"标记)
func (s *Server) handleLimitStates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var st []model.LimitState
	s.db.Where("until = 0 OR until > ?", time.Now().Unix()).Order("since desc").Find(&st)
	if st == nil {
		st = []model.LimitState{}
	}
	writeJSON(w, http.StatusOK, st)
}
