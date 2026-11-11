package web

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/rules"

	"gorm.io/gorm"
)

// ---- 套餐 CRUD ----

func (s *Server) handlePlans(w http.ResponseWriter, r *http.Request) {
	rid := scope(r)
	switch r.Method {
	case http.MethodGet:
		var plans []model.Plan
		q := s.db.Order("sort asc, id asc")
		if !(rid == 0 && r.URL.Query().Has("all")) { // 主面板的规则页要连代理的套餐一起选;其余都只看自己的
			q = q.Where("COALESCE(reseller_id,0) = ?", rid)
		}
		q.Find(&plans)
		writeJSON(w, http.StatusOK, s.planViews(plans, rid))
	case http.MethodPost:
		var body planPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, err)
			return
		}
		p := body.Plan
		p.ResellerId = rid
		p.Price = 0
		if body.Price != nil {
			p.Price = *body.Price
		}
		if err := s.validatePlan(&p); err != nil {
			badRequest(w, err)
			return
		}
		p.Id = 0
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&p).Error; err != nil {
				return err
			}
			if rid == 0 && body.RuleIds != nil {
				return setPlanRules(tx, p.Id, *body.RuleIds)
			}
			return nil
		}); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "plan", "create", p.Name)
		if rid == 0 && body.RuleIds != nil {
			s.rulesNow()
		}
		writeJSON(w, http.StatusOK, p)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

func (s *Server) handlePlanItem(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "/plans/")
	if err != nil {
		badRequest(w, err)
		return
	}
	rid := scope(r)
	var owner model.Plan
	if s.db.First(&owner, id).Error != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "套餐不存在"})
		return
	}
	if owner.ResellerId != rid { // 代理碰不到主面板的套餐,反之亦然
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权限"})
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body planPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, err)
			return
		}
		p := body.Plan
		p.Id, p.ResellerId = id, rid
		p.Price = owner.Price // 没带 price(外部程序按老格式改套餐)就保留原来的参考价,不能抹成 0
		if body.Price != nil {
			p.Price = *body.Price
		}
		if err := s.validatePlan(&p); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.Plan{}).Where("id = ?", id).Select(
				"name", "volume_gb", "days", "device_limit", "speed_up", "speed_down", "auto_reset", "reset_days", "first_use", "line_ids", "line_nodes", "desc", "sort", "price",
			).Updates(p).Error; err != nil {
				return err
			}
			if rid == 0 && body.RuleIds != nil { // 没带 ruleIds(外部程序按老格式改套餐)就不动规则
				return setPlanRules(tx, id, *body.RuleIds)
			}
			return nil
		}); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "plan", "update", p.Name)
		if rid == 0 && body.RuleIds != nil {
			s.rulesNow()
		}
		writeJSON(w, http.StatusOK, p)
	case http.MethodDelete:
		// 套餐删了,用户和规则里指着它的 id 一并清掉:id 不会复用,但留着的话规则页、用户页都对不上名字
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Delete(&model.Plan{}, id).Error; err != nil {
				return err
			}
			if err := tx.Model(&model.User{}).Where("plan_id = ?", id).Update("plan_id", 0).Error; err != nil {
				return err
			}
			return setPlanRules(tx, id, nil)
		}); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "plan", "delete", id)
		s.rulesNow()
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// planPayload 新增 / 修改套餐的请求:RuleIds 是这个套餐要跟的限速规则;没带这个字段 = 不动(只有主面板能改)。
// Price 盖住 model.Plan 的同名字段,好分清「没带」和「给了 0」:修改时没带就保留原来的参考价。
type planPayload struct {
	model.Plan
	RuleIds *[]uint  `json:"ruleIds"`
	Price   *float64 `json:"price"`
}

// 参考价的范围:0 = 不填;填了就在这两个数之间(太大太小都会让折算结果出现 ±Inf,JSON 编不出来)。
const (
	minPlanPrice = 0.01
	maxPlanPrice = 1e9
)

var errPlanPriceRange = errors.New("参考价要在 0.01 到 1000000000 之间(0 = 不填)")

// planView 套餐列表:带上有多少用户、跟着哪些限速规则。
type planView struct {
	model.Plan
	UserCount    int64  `json:"userCount"`
	RuleIds      []uint `json:"ruleIds"`
	ResellerName string `json:"resellerName,omitempty"`
}

func (s *Server) planViews(plans []model.Plan, rid uint) []planView {
	type cnt struct {
		PlanId uint
		N      int64
	}
	var counts []cnt
	s.db.Model(&model.User{}).Select("plan_id, COUNT(*) AS n").Where("plan_id > 0").Group("plan_id").Scan(&counts)
	users := make(map[uint]int64, len(counts))
	for _, c := range counts {
		users[c.PlanId] = c.N
	}
	byPlan := map[uint][]uint{}
	if rid == 0 { // 规则是主面板的东西,代理的套餐页看不到
		var list []model.Rule
		s.db.Select("id, plan_ids").Where("COALESCE(reseller_id,0) = 0").Order("sort asc, id asc").Find(&list)
		for _, r := range list {
			for _, pid := range rules.ParseIDs(r.PlanIds) {
				byPlan[pid] = append(byPlan[pid], r.Id)
			}
		}
	}
	names := map[uint]string{}
	for _, p := range plans {
		if p.ResellerId != 0 && names[p.ResellerId] == "" {
			var n string
			s.db.Model(&model.Reseller{}).Select("name").Where("id = ?", p.ResellerId).Scan(&n)
			names[p.ResellerId] = n
		}
	}
	out := make([]planView, 0, len(plans))
	for _, p := range plans {
		ids := byPlan[p.Id]
		if ids == nil {
			ids = []uint{}
		}
		out = append(out, planView{Plan: p, UserCount: users[p.Id], RuleIds: ids, ResellerName: names[p.ResellerId]})
	}
	return out
}

// setPlanRules 让主面板的规则里只有 ruleIds 这几条选着这个套餐(规则那边存的是套餐 id 列表,两头改的是同一份数据)。
func setPlanRules(tx *gorm.DB, planID uint, ruleIds []uint) error {
	want := make(map[uint]bool, len(ruleIds))
	for _, id := range ruleIds {
		want[id] = true
	}
	var list []model.Rule
	if err := tx.Select("id, plan_ids").Where("COALESCE(reseller_id,0) = 0").Find(&list).Error; err != nil {
		return err
	}
	for _, r := range list {
		ids := rules.ParseIDs(r.PlanIds)
		has := false
		kept := make([]uint, 0, len(ids)+1)
		for _, id := range ids {
			if id == planID {
				has = true
				continue
			}
			kept = append(kept, id)
		}
		if has == want[r.Id] {
			continue
		}
		if want[r.Id] {
			kept = append(kept, planID)
		}
		b, _ := json.Marshal(kept)
		if err := tx.Model(&model.Rule{}).Where("id = ?", r.Id).Update("plan_ids", json.RawMessage(b)).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) validatePlan(p *model.Plan) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("套餐名称不能为空")
	}
	if p.VolumeGB < 0 || p.Days < 0 || p.DeviceLimit < 0 || p.SpeedUp < 0 || p.SpeedDown < 0 || p.ResetDays < 0 || p.Price < 0 {
		return errors.New("数值不能为负")
	}
	if p.Days > model.MaxDays || p.ResetDays > model.MaxDays {
		return errDaysTooMany
	}
	if math.IsNaN(p.Price) || math.IsInf(p.Price, 0) || p.Price > maxPlanPrice || (p.Price > 0 && p.Price < minPlanPrice) {
		return errPlanPriceRange
	}
	if p.AutoReset && p.ResetDays == 0 {
		p.ResetDays = 30
	}
	if len(p.LineIds) > 0 {
		var ids []uint
		if err := json.Unmarshal(p.LineIds, &ids); err != nil {
			return errors.New("线路列表格式错误")
		}
		if len(ids) == 0 {
			p.LineIds = nil
		}
	}
	if err := validatePlanRefs(p); err != nil {
		return err
	}
	if p.ResellerId > 0 { // 代理的套餐只能用授权线路(含服务器范围)
		if refs := planRefs(*p); refs != nil {
			if err := s.checkRefsGranted(p.ResellerId, refs); err != nil {
				return err
			}
		}
	}
	dup := s.db.Model(&model.Plan{}).Where("name = ? AND COALESCE(reseller_id,0) = ?", p.Name, p.ResellerId)
	if p.Id != 0 {
		dup = dup.Where("id != ?", p.Id)
	}
	var n int64
	dup.Count(&n)
	if n > 0 {
		return errors.New("套餐名称已存在")
	}
	return nil
}

// applyPlan 把套餐套到用户上,返回需要设置的线路(nil=不改)。
//
//	new    新建:从现在起算,用量为 0
//	renew  续费:从"现在与原到期中较晚者"起算,已用流量清零(进入新周期)
//	extend 延期:同 renew 的到期计算,但保留已用流量
func applyPlan(u *model.User, p model.Plan, mode string, now int64) []uint {
	return applyPlanN(u, p, mode, 1, now)
}

// applyPlanN 同 applyPlan,一次套 n 期:时长 × n;一次性额度(不周期重置、有上限)续费时配额也 × n,
// 周期重置套餐的流量是每期额度,不叠加;延期只顺延时间,配额仍是一期的。n = 1 时与以前逐字段相同。
func applyPlanN(u *model.User, p model.Plan, mode string, n int, now int64) []uint {
	if n < 1 {
		n = 1
	}
	u.PlanId = p.Id
	u.Volume = model.GBToBytes(p.VolumeGB)
	if n > 1 && mode == "renew" && !p.AutoReset && u.Volume > 0 {
		u.Volume = satMul(u.Volume, n)
	}
	u.DeviceLimit, u.SpeedUp, u.SpeedDown = p.DeviceLimit, p.SpeedUp, p.SpeedDown
	u.AutoReset, u.ResetDays = p.AutoReset, p.ResetDays

	extendByPlan(u, p, n*p.Days, now, mode != "new")
	if mode != "extend" {
		u.TotalUp += u.Up
		u.TotalDown += u.Down
		u.Up, u.Down = 0, 0
	}
	if p.AutoReset {
		if mode == "extend" && u.NextReset > now {
			// 保留当前周期
		} else {
			u.NextReset = model.AddDays(now, p.ResetDays)
		}
	} else {
		u.NextReset = 0
	}
	u.Enabled, u.DisabledReason = true, "" // 套餐是明确的商业动作(开号 / 续费 / 延期):一律启用,手动停用也解除

	if len(p.LineIds) > 0 {
		var ids []uint
		if json.Unmarshal(p.LineIds, &ids) == nil && len(ids) > 0 {
			return ids
		}
	}
	return nil
}

// applyPlanRefs 同 applyPlan,但返回线路 × 服务器的分配(nil = 不改)。
func applyPlanRefs(u *model.User, p model.Plan, mode string, now int64) []model.LineRef {
	if applyPlan(u, p, mode, now) == nil {
		return nil
	}
	return planRefs(p)
}

func (s *Server) loadPlan(id uint) (model.Plan, error) {
	var p model.Plan
	if err := s.db.First(&p, id).Error; err != nil {
		return p, errors.New("套餐不存在")
	}
	return p, nil
}

// handleUserPlan POST /users/{id}/plan {planId, mode, periods, dryRun, requestId, fromPlanId}:续费 / 延期 N 期,
// 不给 planId 就续现在的套餐;mode=bind 只归入套餐。正式执行的返回和以前一样,预览返回结果对象。
func (s *Server) handleUserPlan(w http.ResponseWriter, r *http.Request, u model.User) {
	req, err := decodePlanOpReq(r)
	if err != nil {
		writePlanErr(w, err)
		return
	}
	if req.Mode == "bind" { // 只归入套餐:额度和到期都不动(老用户想按套餐套规则时用)
		pid := uint(0)
		if req.PlanId != nil {
			pid = *req.PlanId
		}
		if req.DryRun { // 预览只校验:不改归属、不写审计、不触发规则重判
			if err := s.planBelongs(pid, u.ResellerId); err != nil {
				badRequest(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"ok": "1", "dryRun": true, "expiry": u.Expiry, "planId": pid})
			return
		}
		if err := s.bindUserPlan([]model.User{u}, pid); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "user", "plan:bind", u.Name)
		s.rulesNow()
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": "1", "expiry": u.Expiry})
		return
	}
	// 代理面板:只能用自己的套餐,线路在授权内,代理本身可用(校验在 renewUserPlan 里)
	res, err := s.renewUserPlan(r, planScope{rid: scope(r), actor: s.actor(r)}, u, req, false)
	if err != nil {
		writePlanErr(w, err)
		return
	}
	if res.DryRun {
		writeJSON(w, http.StatusOK, res)
		return
	}
	out := map[string]interface{}{"ok": "1", "expiry": res.To.Expiry}
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		out["replayed"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

// planBelongs 套餐和用户必须是同一个归属(主面板 / 同一个代理)。0 = 不属于任何套餐,总是可以。
func (s *Server) planBelongs(planID, resellerID uint) error {
	if planID == 0 {
		return nil
	}
	p, err := s.loadPlan(planID)
	if err != nil {
		return err
	}
	if p.ResellerId != resellerID {
		return errors.New("这个套餐不属于该用户的归属")
	}
	return nil
}

// bindUserPlan 把用户归入套餐(0 = 移出),不改额度。
func (s *Server) bindUserPlan(users []model.User, planID uint) error {
	for _, u := range users {
		if err := s.planBelongs(planID, u.ResellerId); err != nil {
			return err
		}
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		for _, u := range users {
			if err := tx.Model(&model.User{}).Where("id = ?", u.Id).Update("plan_id", planID).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- 批量生成 ----

// handleUsersBulk POST /users/bulk {prefix, count, planId, nameMode: random|seq, startIndex, lineIds}
func (s *Server) handleUsersBulk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req struct {
		Prefix     string          `json:"prefix"`
		Count      int             `json:"count"`
		PlanId     uint            `json:"planId"`
		NameMode   string          `json:"nameMode"`
		StartIndex int             `json:"startIndex"`
		LineIds    []uint          `json:"lineIds"`
		LineRefs   []model.LineRef `json:"lineRefs"` // 线路 × 服务器;给了它就以它为准
		Remark     string          `json:"remark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	if req.Count < 1 || req.Count > 500 {
		badRequest(w, errors.New("数量需在 1–500 之间"))
		return
	}
	req.Prefix = strings.TrimSpace(req.Prefix)
	if strings.ContainsAny(req.Prefix, "/?#& ") {
		badRequest(w, errors.New("前缀不能包含空格或 / ? # &"))
		return
	}
	var plan *model.Plan
	if req.PlanId != 0 {
		p, err := s.loadPlan(req.PlanId)
		if err != nil {
			badRequest(w, err)
			return
		}
		plan = &p
	}
	if req.StartIndex < 1 {
		req.StartIndex = 1
	}
	now := time.Now().Unix()
	// 线路范围整批一样,事务外整理一次;套餐带线路时以套餐的为准
	baseRefs := s.normalizeRefs(lineRefsOf(req.LineIds, req.LineRefs))
	var planRefsN []model.LineRef
	if plan != nil {
		planRefsN = s.normalizeRefs(planRefs(*plan))
	}
	rid := scope(r)
	if rid > 0 {
		s.resellerUserMu.Lock()
		defer s.resellerUserMu.Unlock()
		if plan != nil && plan.ResellerId != rid {
			badRequest(w, errors.New("套餐不存在"))
			return
		}
		if len(baseRefs) == 0 {
			baseRefs = s.resellerLineRefs(rid)
		}
		if err := s.checkRefsGranted(rid, baseRefs); err != nil {
			badRequest(w, err)
			return
		}
		if plan != nil {
			if err := s.checkRefsGranted(rid, planRefsN); err != nil {
				badRequest(w, err)
				return
			}
		}
		// 用户数上限按整批先算:事务里逐个校验用的是另一条连接,看不到本事务刚插入的用户,上限 10 的代理一次能建 500 个(审计 MB21)
		var rs model.Reseller
		if s.db.First(&rs, rid).Error == nil && rs.UserLimit > 0 {
			var n int64
			s.db.Model(&model.User{}).Where("reseller_id = ?", rid).Count(&n)
			if int(n)+req.Count > rs.UserLimit {
				badRequest(w, fmt.Errorf("代理用户数上限 %d,已有 %d,这次最多还能建 %d 个", rs.UserLimit, n, max(rs.UserLimit-int(n), 0)))
				return
			}
		}
	}
	type created struct {
		Name string `json:"name"`
		Link string `json:"link"`
	}
	var out []created
	err := s.db.Transaction(func(tx *gorm.DB) error {
		idx := req.StartIndex
		// 名字撞了就跳过再试;试的次数封顶,免得序号段全被占时在事务里空转
		for attempts := 0; len(out) < req.Count && attempts < req.Count*3+10; attempts++ {
			var name string
			if req.NameMode == "seq" {
				name = fmt.Sprintf("%s%03d", req.Prefix, idx)
				idx++
			} else {
				name = req.Prefix + strings.ToLower(creds.Password(8))
			}
			var exists int64
			tx.Model(&model.User{}).Where("name = ?", name).Count(&exists)
			if exists > 0 {
				continue
			}
			u := model.User{Name: name, Enabled: true, Remark: req.Remark, CreatedAt: now}
			b, _ := json.Marshal(creds.Generate(name))
			u.Credentials = b
			if req.NameMode == "seq" {
				// 序号名能顺着猜(vip001、vip002…),用户名作订阅地址时拿到一个就能拉到别人的凭据:序号模式一律用随机令牌(审计 MB29)
				u.SubToken = randomSubToken()
			}
			s.applySubTokenPolicy(&u) // 批量生成同样按设置决定订阅地址形式
			refs := baseRefs
			if plan != nil {
				if applyPlanRefs(&u, *plan, "new", now) != nil {
					refs = planRefsN
				}
			}
			if rid > 0 {
				if err := s.prepareResellerUser(rid, &u, refs); err != nil {
					return err
				}
			}
			if err := tx.Create(&u).Error; err != nil {
				return err
			}
			if err := writeUserLineRefs(tx, u.Id, refs); err != nil {
				return err
			}
			out = append(out, created{Name: name, Link: s.subLinks(u)["clash"]})
		}
		return nil
	})
	if err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "user", "bulk-create", map[string]interface{}{"prefix": req.Prefix, "count": len(out)})
	s.reloadUsers(fmt.Sprintf("批量创建 %d 个用户", len(out)))
	writeJSON(w, http.StatusOK, out)
}

// ---- 批量操作 ----

// handleUsersBatch POST /users/batch {ids, action: enable|disable|delete|reset|extend|plan, days, planId, mode, periods}
// idChunks 把 id 切成每批 500 个:SQLite 一条语句最多 32766 个变量,「选中全部筛选结果」一次能送上万个 id,
// 一条 IN 放不下时 Find 报错、结果为空,以前被当成"处理了 0 个"照样返回 200、写审计
func idChunks(ids []uint) [][]uint {
	const n = 500
	var out [][]uint
	for len(ids) > n {
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}

// usersByIDs 按批取用户,任一批出错就返回错误(不当成空)。
func (s *Server) usersByIDs(ids []uint) ([]model.User, error) {
	var out []model.User
	for _, part := range idChunks(ids) {
		var got []model.User
		if err := s.db.Where("id IN ?", part).Find(&got).Error; err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

func (s *Server) handleUsersBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req struct {
		Ids     []uint `json:"ids"`
		Action  string `json:"action"`
		Days    int    `json:"days"`
		PlanId  uint   `json:"planId"`
		Mode    string `json:"mode"`
		Periods *int   `json:"periods"` // action=plan:一次续 N 期(默认 1)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	if len(req.Ids) == 0 {
		badRequest(w, errors.New("未选择用户"))
		return
	}
	if rid := scope(r); rid > 0 { // 代理:只留自己的用户
		var mine []uint
		for _, part := range idChunks(req.Ids) {
			var got []uint
			if err := s.db.Model(&model.User{}).Where("id IN ? AND reseller_id = ?", part, rid).Pluck("id", &got).Error; err != nil {
				badRequest(w, err)
				return
			}
			mine = append(mine, got...)
		}
		if len(mine) == 0 {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权限"})
			return
		}
		req.Ids = mine
	}
	now := time.Now().Unix()
	var affected int64
	auditAction := "batch:" + req.Action
	switch req.Action {
	case "enable", "disable":
		n, err := s.setEnabled(s.db, req.Ids, req.Action == "enable")
		if err != nil {
			badRequest(w, err)
			return
		}
		affected = n
	case "delete":
		users, err := s.usersByIDs(req.Ids)
		if err != nil {
			badRequest(w, err)
			return
		}
		for _, u := range users {
			if err := s.deleteUser(u, s.actor(r)); err != nil {
				badRequest(w, err)
				return
			}
			affected++
		}
	case "reset":
		for _, id := range req.Ids {
			if err := s.resetUsage(s.db, id); err != nil {
				badRequest(w, err)
				return
			}
			affected++
		}
	case "extend":
		if req.Days <= 0 {
			badRequest(w, errors.New("延期天数需大于 0"))
			return
		}
		if req.Days > model.MaxDays {
			badRequest(w, errDaysTooMany)
			return
		}
		users, err := s.usersByIDs(req.Ids)
		if err != nil {
			badRequest(w, err)
			return
		}
		for _, u := range users {
			if ok, err := s.extendExpiry(s.db, u, req.Days, now); err != nil {
				badRequest(w, err)
				return
			} else if ok {
				affected++
			}
		}
	case "plan":
		if req.Mode == "bind" {
			auditAction = "batch:plan:bind"
			users, err := s.usersByIDs(req.Ids)
			if err != nil {
				badRequest(w, err)
				return
			}
			if err := s.bindUserPlan(users, req.PlanId); err != nil {
				badRequest(w, err)
				return
			}
			affected = int64(len(users))
			s.rulesNow()
			break
		}
		p, err := s.loadPlan(req.PlanId)
		if rid := scope(r); err == nil && rid > 0 && p.ResellerId != rid {
			err = errPlanNotFound // 先核对归属再校验期数:别人的套餐和不存在的套餐同一个错误,探不出别人的套餐与天数
		}
		if err != nil {
			badRequest(w, err)
			return
		}
		if req.Mode != "extend" {
			req.Mode = "renew"
		}
		// 和单人的 plan:<方式>:<套餐名> 同一写法,概况「最近操作」才写得出是哪个套餐、续费还是延期
		auditAction = "batch:plan:" + req.Mode + ":" + p.Name
		n, err := planPeriods(req.Periods, req.Mode, p.Days)
		if err != nil {
			writePlanErr(w, err)
			return
		}
		users, err := s.usersByIDs(req.Ids)
		if err != nil {
			badRequest(w, err)
			return
		}
		if rid := scope(r); rid > 0 {
			for _, u := range users {
				if err := s.checkResellerPlan(rid, u, p); err != nil {
					badRequest(w, err)
					return
				}
			}
		}
		// 每人一笔事务、一条套餐记录;审计照旧整批记一条,热更新与规则重判最后统一做
		sc := planScope{rid: scope(r), actor: s.actor(r)}
		replanned := false
		for _, u := range users {
			res, err := s.runPlanOp(sc, u.Id, planOpCall{endpoint: "batch", opt: planOpOpt{Op: req.Mode, Periods: n}, target: p, quiet: true,
				reqHash: planHash("batch", u.Id, p.Id, req.Mode, n)})
			if err != nil {
				writePlanErr(w, err)
				return
			}
			replanned = replanned || res.From.PlanId != p.Id
			affected++
		}
		if replanned {
			s.rulesNow()
		}
	default:
		badRequest(w, fmt.Errorf("未知操作 %q", req.Action))
		return
	}
	s.audit(r, "user", auditAction, req.Ids)
	s.reloadUsers(fmt.Sprintf("批量 %s %d 个用户", req.Action, affected))
	writeJSON(w, http.StatusOK, map[string]int64{"affected": affected})
}

// ---- CSV 导出 ----

func (s *Server) handleUsersExport(w http.ResponseWriter, r *http.Request) {
	var users []model.User
	s.db.Where("COALESCE(reseller_id,0) = 0").Order("id asc").Find(&users)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	loc := s.panelLocation() // 导出的日期跟面板时区一致
	w.Header().Set("Content-Disposition", "attachment; filename=users-"+time.Now().In(loc).Format("20060102")+".csv")
	w.Write([]byte("\xEF\xBB\xBF")) // BOM,Excel 正确识别 UTF-8
	cw := csv.NewWriter(w)
	cw.Write([]string{"用户名", "备注", "状态", "已用GB", "配额GB", "到期", "设备上限", "上行Mbps", "下行Mbps", "最近在线", "Clash订阅"})
	now := time.Now().Unix()
	for _, u := range users {
		status := "启用"
		switch {
		case !u.Enabled:
			status = "停用"
		case u.Expiry > 0 && u.Expiry < now:
			status = "已过期"
		case u.Volume > 0 && u.Up+u.Down >= u.Volume:
			status = "超量"
		}
		expiry, online := "", ""
		if u.Expiry > 0 {
			expiry = time.Unix(u.Expiry, 0).In(loc).Format("2006-01-02")
		}
		if u.OnlineAt > 0 {
			online = time.Unix(u.OnlineAt, 0).In(loc).Format("2006-01-02 15:04")
		}
		cw.Write([]string{
			u.Name, u.Remark, status,
			fmt.Sprintf("%.2f", float64(u.Up+u.Down)/(1<<30)), fmt.Sprintf("%d", u.Volume>>30),
			expiry, fmt.Sprintf("%d", u.DeviceLimit), fmt.Sprintf("%d", u.SpeedUp), fmt.Sprintf("%d", u.SpeedDown),
			online, s.subLinks(u)["clash"],
		})
	}
	cw.Flush()
}
