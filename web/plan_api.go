package web

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// ---- 套餐 CRUD ----

func (s *Server) handlePlans(w http.ResponseWriter, r *http.Request) {
	rid := scope(r)
	switch r.Method {
	case http.MethodGet:
		var plans []model.Plan
		s.db.Where("COALESCE(reseller_id,0) = ?", rid).Order("sort asc, id asc").Find(&plans)
		writeJSON(w, http.StatusOK, plans)
	case http.MethodPost:
		var p model.Plan
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		p.ResellerId = rid
		if err := s.validatePlan(&p); err != nil {
			badRequest(w, err)
			return
		}
		p.Id = 0
		if err := s.db.Create(&p).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "plan", "create", p.Name)
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
		var p model.Plan
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		p.Id, p.ResellerId = id, rid
		if err := s.validatePlan(&p); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.db.Model(&model.Plan{}).Where("id = ?", id).Select(
			"name", "volume_gb", "days", "device_limit", "speed_up", "speed_down", "auto_reset", "reset_days", "line_ids", "desc", "sort",
		).Updates(p).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "plan", "update", p.Name)
		writeJSON(w, http.StatusOK, p)
	case http.MethodDelete:
		if err := s.db.Delete(&model.Plan{}, id).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "plan", "delete", id)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

func (s *Server) validatePlan(p *model.Plan) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("套餐名称不能为空")
	}
	if p.VolumeGB < 0 || p.Days < 0 || p.DeviceLimit < 0 || p.SpeedUp < 0 || p.SpeedDown < 0 || p.ResetDays < 0 {
		return errors.New("数值不能为负")
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
	if p.ResellerId > 0 { // 代理的套餐只能用授权线路
		var ids []uint
		_ = json.Unmarshal(p.LineIds, &ids)
		allowed := map[uint]bool{}
		for _, lid := range s.resellerLineIds(p.ResellerId) {
			allowed[lid] = true
		}
		for _, lid := range ids {
			if !allowed[lid] {
				return errors.New("含未授权的线路")
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
	u.Volume = int64(p.VolumeGB) << 30
	u.DeviceLimit, u.SpeedUp, u.SpeedDown = p.DeviceLimit, p.SpeedUp, p.SpeedDown
	u.AutoReset, u.ResetDays = p.AutoReset, p.ResetDays

	base := now
	if mode != "new" && u.Expiry > now {
		base = u.Expiry
	}
	if p.Days > 0 {
		u.Expiry = base + int64(p.Days)*86400
	} else {
		u.Expiry = 0
	}
	if mode != "extend" {
		u.TotalUp += u.Up
		u.TotalDown += u.Down
		u.Up, u.Down = 0, 0
	}
	if p.AutoReset {
		if mode == "extend" && u.NextReset > now {
			// 保留当前周期
		} else {
			u.NextReset = now + int64(p.ResetDays)*86400
		}
	} else {
		u.NextReset = 0
	}
	u.Enabled = true

	if len(p.LineIds) > 0 {
		var ids []uint
		if json.Unmarshal(p.LineIds, &ids) == nil && len(ids) > 0 {
			return ids
		}
	}
	return nil
}

func (s *Server) loadPlan(id uint) (model.Plan, error) {
	var p model.Plan
	if err := s.db.First(&p, id).Error; err != nil {
		return p, errors.New("套餐不存在")
	}
	return p, nil
}

// handleUserPlan POST /users/{id}/plan {planId, mode}
func (s *Server) handleUserPlan(w http.ResponseWriter, r *http.Request, u model.User) {
	var req struct {
		PlanId uint   `json:"planId"`
		Mode   string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	p, err := s.loadPlan(req.PlanId)
	if err != nil {
		badRequest(w, err)
		return
	}
	if req.Mode != "extend" {
		req.Mode = "renew"
	}
	if rid := scope(r); rid > 0 { // 代理套餐同样受线路授权与设备额度约束
		if err := s.checkResellerPlan(rid, u, p); err != nil {
			badRequest(w, err)
			return
		}
	}
	if err := s.applyUserPlan(&u, p, req.Mode); err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "user", "plan:"+req.Mode+":"+p.Name, u.Name)
	s.reloadUsers("套餐 " + p.Name + " → " + u.Name)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": "1", "expiry": u.Expiry})
}

// applyUserPlan 把套餐套到已有用户上并落库(含套餐指定的线路);调用方负责审计与热更新。
func (s *Server) applyUserPlan(u *model.User, p model.Plan, mode string) error {
	lineIds := applyPlan(u, p, mode, time.Now().Unix())
	if err := s.db.Model(&model.User{}).Where("id = ?", u.Id).Select(
		"volume", "expiry", "device_limit", "speed_up", "speed_down", "auto_reset", "reset_days", "next_reset",
		"total_up", "total_down", "up", "down", "enabled",
	).Updates(*u).Error; err != nil {
		return err
	}
	if lineIds != nil {
		s.setUserLines(u.Id, lineIds)
	}
	return nil
}

// ---- 批量生成 ----

// handleUsersBulk POST /users/bulk {prefix, count, planId, nameMode: random|seq, startIndex, lineIds}
func (s *Server) handleUsersBulk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req struct {
		Prefix     string `json:"prefix"`
		Count      int    `json:"count"`
		PlanId     uint   `json:"planId"`
		NameMode   string `json:"nameMode"`
		StartIndex int    `json:"startIndex"`
		LineIds    []uint `json:"lineIds"`
		Remark     string `json:"remark"`
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
	type created struct {
		Name string `json:"name"`
		Link string `json:"link"`
	}
	var out []created
	err := s.db.Transaction(func(tx *gorm.DB) error {
		idx := req.StartIndex
		for len(out) < req.Count {
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
				if req.NameMode != "seq" {
					continue
				}
				continue
			}
			u := model.User{Name: name, Enabled: true, Remark: req.Remark, CreatedAt: now}
			b, _ := json.Marshal(creds.Generate(name))
			u.Credentials = b
			lineIds := req.LineIds
			if plan != nil {
				if ids := applyPlan(&u, *plan, "new", now); ids != nil {
					lineIds = ids
				}
			}
			if err := tx.Create(&u).Error; err != nil {
				return err
			}
			for _, lid := range lineIds {
				tx.Create(&model.UserLine{UserId: u.Id, LineId: lid})
			}
			out = append(out, created{Name: name, Link: s.subLinks(u)["clash"]})
			if idx-req.StartIndex > req.Count*3+10 { // 序号模式下冲突过多时终止
				break
			}
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

// handleUsersBatch POST /users/batch {ids, action: enable|disable|delete|reset|extend|plan, days, planId, mode}
func (s *Server) handleUsersBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req struct {
		Ids    []uint `json:"ids"`
		Action string `json:"action"`
		Days   int    `json:"days"`
		PlanId uint   `json:"planId"`
		Mode   string `json:"mode"`
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
		s.db.Model(&model.User{}).Where("id IN ? AND reseller_id = ?", req.Ids, rid).Pluck("id", &mine)
		if len(mine) == 0 {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权限"})
			return
		}
		req.Ids = mine
	}
	now := time.Now().Unix()
	var affected int64
	switch req.Action {
	case "enable", "disable":
		res := s.db.Model(&model.User{}).Where("id IN ?", req.Ids).Update("enabled", req.Action == "enable")
		affected = res.RowsAffected
	case "delete":
		var users []model.User
		s.db.Where("id IN ?", req.Ids).Find(&users)
		for _, u := range users {
			if err := s.deleteUser(u, s.actor(r)); err != nil {
				badRequest(w, err)
				return
			}
			affected++
		}
	case "reset":
		res := s.db.Model(&model.User{}).Where("id IN ?", req.Ids).Updates(map[string]interface{}{
			"total_up": gorm.Expr("total_up + up"), "total_down": gorm.Expr("total_down + down"), "up": 0, "down": 0, "enabled": true,
		})
		affected = res.RowsAffected
	case "extend":
		if req.Days <= 0 {
			badRequest(w, errors.New("延期天数需大于 0"))
			return
		}
		var users []model.User
		s.db.Where("id IN ?", req.Ids).Find(&users)
		for _, u := range users {
			base := now
			if u.Expiry > now {
				base = u.Expiry
			}
			s.db.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{"expiry": base + int64(req.Days)*86400, "enabled": true})
			affected++
		}
	case "plan":
		p, err := s.loadPlan(req.PlanId)
		if err != nil {
			badRequest(w, err)
			return
		}
		if req.Mode != "extend" {
			req.Mode = "renew"
		}
		var users []model.User
		s.db.Where("id IN ?", req.Ids).Find(&users)
		if rid := scope(r); rid > 0 {
			for _, u := range users {
				if err := s.checkResellerPlan(rid, u, p); err != nil {
					badRequest(w, err)
					return
				}
			}
		}
		for _, u := range users {
			lineIds := applyPlan(&u, p, req.Mode, now)
			s.db.Model(&model.User{}).Where("id = ?", u.Id).Select(
				"volume", "expiry", "device_limit", "speed_up", "speed_down", "auto_reset", "reset_days", "next_reset",
				"total_up", "total_down", "up", "down", "enabled",
			).Updates(u)
			if lineIds != nil {
				s.setUserLines(u.Id, lineIds)
			}
			affected++
		}
	default:
		badRequest(w, fmt.Errorf("未知操作 %q", req.Action))
		return
	}
	s.audit(r, "user", "batch:"+req.Action, req.Ids)
	s.reloadUsers(fmt.Sprintf("批量 %s %d 个用户", req.Action, affected))
	writeJSON(w, http.StatusOK, map[string]int64{"affected": affected})
}

// ---- CSV 导出 ----

func (s *Server) handleUsersExport(w http.ResponseWriter, r *http.Request) {
	var users []model.User
	s.db.Order("id asc").Find(&users)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=users-"+time.Now().Format("20060102")+".csv")
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
			expiry = time.Unix(u.Expiry, 0).Format("2006-01-02")
		}
		if u.OnlineAt > 0 {
			online = time.Unix(u.OnlineAt, 0).Format("2006-01-02 15:04")
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
