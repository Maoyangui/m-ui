package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// 代理面板的作用域:同一批接口,查询一律加上 reseller_id 限制,越权直接 403。

// withScope 把代理 ID 放进请求上下文(登录中间件与测试用)。
func withScope(r *http.Request, rid uint) context.Context {
	return context.WithValue(r.Context(), scopeKey, rid)
}

// scoped 给用户查询加上代理限制;主面板(scope=0)不加。
func (s *Server) scoped(r *http.Request, q *gorm.DB) *gorm.DB {
	if rid := scope(r); rid > 0 {
		return q.Where("reseller_id = ?", rid)
	}
	return q
}

// resellerUsed 代理已用流量:名下用户的全时用量之和 + 结转 - 主面板重置基线。
// 用全时用量是关键——代理自己重置/续费/周期清零都只是把 up/down 挪进 total_*,额度不会因此回血。
func resellerUsed(db *gorm.DB, rs model.Reseller) int64 {
	var live int64
	db.Model(&model.User{}).Where("reseller_id = ?", rs.Id).
		Select("COALESCE(SUM(up + down + total_up + total_down),0)").Scan(&live)
	used := live + rs.UsedCarried - rs.UsedBase
	if used < 0 {
		return 0
	}
	return used
}

// carryUsage 删用户前把它的全时用量结转到所属代理,防止"删号洗流量"。
func carryUsage(db *gorm.DB, u model.User) {
	if u.ResellerId == 0 {
		return
	}
	all := u.Up + u.Down + u.TotalUp + u.TotalDown
	if all <= 0 {
		return
	}
	db.Model(&model.Reseller{}).Where("id = ?", u.ResellerId).
		Update("used_carried", gorm.Expr("used_carried + ?", all))
}

// resellerUserNames 该代理名下的用户名集合。
func (s *Server) resellerUserNames(rid uint) map[string]bool {
	var names []string
	s.db.Model(&model.User{}).Where("reseller_id = ?", rid).Pluck("name", &names)
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// guardScope 代理面板下的用户接口守卫:批量类一律不给,单用户必须是自己的。
func (s *Server) guardScope(w http.ResponseWriter, r *http.Request) bool {
	rid := scope(r)
	if rid == 0 {
		return true
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	rest := path[strings.LastIndex(path, "/users/")+len("/users/"):]
	head := strings.Split(rest, "/")[0]
	switch head {
	case "bulk", "export", "import":
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权限"})
		return false
	case "batch":
		return true // 批量接口自己会把 id 过滤成这个代理的
	}
	id, err := strconv.ParseUint(head, 10, 64) // 子路由是 /users/{id}/xxx,只取 id 那一段
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
		return false
	}
	var n int64
	s.db.Model(&model.User{}).Where("id = ? AND reseller_id = ?", id, rid).Count(&n)
	if n == 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权限"})
		return false
	}
	return true
}

// prepareResellerUser 代理建用户:归到自己名下,发随机订阅令牌,并校验线路与设备额度。
func (s *Server) prepareResellerUser(rid uint, u *model.User, lineIds []uint) error {
	if err := s.checkResellerUser(rid, 0, u, lineIds); err != nil {
		return err
	}
	u.ResellerId = rid
	u.SubToken = randomSubToken()
	// 请求体里能塞的字段一律清掉:计量必须从 0 起(否则塞个负数就能把代理额度刷回来),
	// 共享令牌只能由用户自己在订阅页生成
	u.Up, u.Down, u.TotalUp, u.TotalDown, u.OnlineAt = 0, 0, 0, 0, 0
	u.ShareToken, u.ShareCreds, u.ShareAt = "", nil, 0
	return nil
}

// checkResellerUser 校验:代理可用、线路在授权范围内、设备数不超代理预算。
// id 为 0 表示新建;否则是改这个用户(算预算时先扣掉它原来的占用)。
func (s *Server) checkResellerUser(rid, id uint, u *model.User, lineIds []uint) error {
	var rs model.Reseller
	if err := s.db.First(&rs, rid).Error; err != nil {
		return errors.New("代理不存在")
	}
	if !rs.Enabled {
		return errors.New("代理已停用")
	}
	allowed := map[uint]bool{}
	for _, lid := range s.resellerLineIds(rid) {
		allowed[lid] = true
	}
	for _, lid := range lineIds {
		if !allowed[lid] {
			return errors.New("含未授权的线路")
		}
	}
	if rs.Expiry > 0 && rs.Expiry < time.Now().Unix() {
		return errors.New("代理已到期")
	}
	// 设备数与带宽不再是"分配预算":代理给单个用户填多少都行(0=不限),运行时由数据面按代理池限制 ——
	// 名下所有用户同时在线的设备总数不超过代理的设备池,合计带宽不超过代理的带宽池(每台服务器各一份)。
	if u.DeviceLimit < 0 || u.SpeedUp < 0 || u.SpeedDown < 0 {
		return errors.New("设备数与限速不能为负")
	}
	if id == 0 && rs.UserLimit > 0 { // 用户数上限只管新建;已有用户照常编辑
		var n int64
		s.db.Model(&model.User{}).Where("reseller_id = ?", rid).Count(&n)
		if int(n) >= rs.UserLimit {
			return errors.New("已达到用户数上限 " + strconv.Itoa(rs.UserLimit) + ",不能再建")
		}
	}
	if rs.Volume > 0 && resellerUsed(s.db, rs) >= rs.Volume {
		return errors.New("代理流量已用尽")
	}
	return nil
}

// checkResellerPlan 代理套用套餐时:套餐带的线路必须在授权范围内,设备数不能撑破总额。
func (s *Server) checkResellerPlan(rid uint, u model.User, p model.Plan) error {
	if p.ResellerId != rid {
		return errors.New("套餐不存在")
	}
	lineIds := []uint{}
	if len(p.LineIds) > 0 {
		if err := json.Unmarshal(p.LineIds, &lineIds); err != nil {
			return errors.New("套餐线路解析失败")
		}
	}
	if len(lineIds) == 0 { // 套餐不改线路时,沿用用户当前线路
		s.db.Model(&model.UserLine{}).Where("user_id = ?", u.Id).Pluck("line_id", &lineIds)
	}
	next := u
	next.DeviceLimit, next.SpeedUp, next.SpeedDown = p.DeviceLimit, p.SpeedUp, p.SpeedDown
	return s.checkResellerUser(rid, u.Id, &next, lineIds)
}

// resellerStatus 代理面板的概览数据:只统计自己名下。
func (s *Server) resellerStatus(r *http.Request, rid uint) map[string]interface{} {
	var rs model.Reseller
	s.db.First(&rs, rid)
	var userCount, enabledUsers, total int64
	s.db.Model(&model.User{}).Where("reseller_id = ?", rid).Count(&userCount)
	s.db.Model(&model.User{}).Where("reseller_id = ? AND enabled = ?", rid, true).Count(&enabledUsers)
	used := resellerUsed(s.db, rs)
	s.db.Model(&model.User{}).Where("reseller_id = ?", rid).
		Select("COALESCE(SUM(up+down+total_up+total_down),0)").Scan(&total)
	var lines int64
	s.db.Model(&model.ResellerLine{}).Where("reseller_id = ?", rid).Count(&lines)

	online := s.resellerOnlineIPs(rid, s.onlineIPsAll())
	sess := s.sessionOf(r)
	return map[string]interface{}{
		"scope":           "reseller",
		"reseller":        rs.Name,
		"mustSetPassword": sess.pending,
		"totpEnabled":     rs.TotpEnabled,
		"volume":          rs.Volume,
		"used":            used,
		"deviceLimit":     rs.DeviceLimit,
		"expiry":          rs.Expiry,
		"speedUp":         rs.SpeedUp,
		"speedDown":       rs.SpeedDown,
		"onlineDevices":   len(online),
		"users":           userCount,
		"enabledUsers":    enabledUsers,
		"lines":           lines,
		"linesEnabled":    lines,
		"trafficUp":       total, // 概览卡片用:代理名下累计
		"trafficDown":     int64(0),
		"onlineUsers":     len(s.onlineResellerUsers(rid)),
		"timezone":        s.panelLocation().String(),
		"version":         Version,
		"coreRunning":     s.run.CoreRunning(),
		"uptime":          s.run.Uptime(),
	}
}

func (s *Server) onlineResellerUsers(rid uint) []string {
	mine := s.resellerUserNames(rid)
	return filterNames(mergeIPs(s.run.Onlines().Users, s.run.Hub().RemoteOnlineUsers()), mine)
}

// sessionOf 取当前代理会话(用于读 pending 标记)。
func (s *Server) sessionOf(r *http.Request) session {
	c, err := r.Cookie(resellerCookie)
	if err != nil {
		return session{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[c.Value]
}

// randomSubToken 代理用户的订阅地址:32 位随机,不可猜。
func randomSubToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// applySubTokenPolicy 决定新用户的订阅地址形式:
// 设置 subUseUserName 关掉时发一个随机令牌(订阅地址与用户名无关),开着(默认)则沿用用户名。
// 只在建号时判断一次,改设置不会动到已有用户的订阅地址。
func (s *Server) applySubTokenPolicy(u *model.User) {
	if u.SubToken != "" || u.ResellerId > 0 {
		return // 代理建的用户本来就是随机令牌
	}
	if strings.EqualFold(s.setting("subUseUserName"), "false") {
		u.SubToken = randomSubToken()
	}
}
