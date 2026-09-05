package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
)

// ---- 外部 API v1 ----
//
// 供商城/计费系统/机器人等外部程序调用,与面板会话无关:在"管理员"页开启并取得令牌,
// 请求带 `Authorization: Bearer <令牌>`(或 `X-API-Key: <令牌>`)。所有响应为 JSON,
// 失败返回 4xx 与 {"error": "..."}。用户既可用名称也可用数字 id 定位。完整说明见 docs/API.md。
//
// 代理也有同一套接口:在代理面板"我的账号"里开启并取令牌,地址前缀是代理面板的 <路径>api/v1,
// 作用域自动限定为该代理自己的用户、套餐与授权线路(和代理在面板里能做的完全一致)。
//
//	GET    /api/v1/ping                     连通性与版本
//	GET    /api/v1/plans                    套餐列表
//	GET    /api/v1/users[?q=关键字]          用户列表
//	POST   /api/v1/users                    创建用户(可直接套用套餐)
//	GET    /api/v1/users/{name|id}          用户详情(含订阅地址)
//	PATCH  /api/v1/users/{name|id}          修改字段(启停、配额、到期、线路…)
//	DELETE /api/v1/users/{name|id}          删除
//	POST   /api/v1/users/{name|id}/enable | disable | reset | kick
//	POST   /api/v1/users/{name|id}/plan     套用套餐 {planId|plan, mode: renew|extend}
//	GET    /api/v1/users/{name|id}/sub      订阅地址

// apiToken 从请求头取令牌:Authorization: Bearer 优先,其次 X-API-Key。
func apiToken(r *http.Request) string {
	got := r.Header.Get("X-API-Key")
	if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
		got = strings.TrimSpace(h[7:])
	}
	return got
}

func (s *Server) publicAuth(r *http.Request) bool {
	if s.setting("apiEnabled") != "true" {
		return false
	}
	want := s.setting("apiToken")
	if want == "" {
		return false
	}
	got := apiToken(r)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// apiScope 一次调用的作用域:主面板令牌 rid=0 看全部;代理令牌只看自己名下。
type apiScope struct {
	rid   uint
	actor string // 审计里的操作者:api / api:代理名
}

func (s *Server) handlePublicAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.publicAuth(r) {
		logger.Warning("外部 API 鉴权失败,来源 ", clientIP(r), " ", r.Method, " ", r.URL.Path)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "外部 API 未开启或令牌错误"})
		return
	}
	s.publicAPI(w, r, apiScope{actor: "api"})
}

// resellerByAPIToken 按令牌找到开启了外部 API 的代理;停用、到期的代理视同令牌错误。
func (s *Server) resellerByAPIToken(tok string) (model.Reseller, bool) {
	var rs model.Reseller
	if tok == "" || s.db.Where("api_enabled = ? AND api_token = ? AND api_token <> ''", true, tok).First(&rs).Error != nil {
		return rs, false
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(rs.ApiToken)) != 1 || !rs.Enabled || (rs.Expiry > 0 && rs.Expiry < time.Now().Unix()) {
		return rs, false
	}
	return rs, true
}

// handleResellerPublicAPI 代理面板上的 /api/v1/:令牌属于哪个代理,就只操作那个代理名下的东西。
func (s *Server) handleResellerPublicAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rs, ok := s.resellerByAPIToken(apiToken(r))
	if !ok {
		logger.Warning("代理外部 API 鉴权失败,来源 ", clientIP(r), " ", r.Method, " ", r.URL.Path)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "外部 API 未开启或令牌错误"})
		return
	}
	s.publicAPI(w, r, apiScope{rid: rs.Id, actor: "api:" + rs.Name})
}

func (s *Server) publicAPI(w http.ResponseWriter, r *http.Request, sc apiScope) {
	full := strings.TrimSuffix(r.URL.Path, "/")
	idx := strings.LastIndex(full, "/v1/")
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(full[idx+4:], "/")
	switch {
	case len(parts) == 1 && parts[0] == "ping":
		role := s.role()
		if sc.rid > 0 {
			role = "reseller"
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "version": Version, "role": role, "time": time.Now().Unix()})
	case len(parts) == 1 && parts[0] == "plans":
		var plans []model.Plan
		s.db.Where("COALESCE(reseller_id,0) = ?", sc.rid).Order("sort asc, id asc").Find(&plans)
		writeJSON(w, http.StatusOK, plans)
	case len(parts) == 1 && parts[0] == "users":
		switch r.Method {
		case http.MethodGet:
			s.apiListUsers(w, r, sc)
		case http.MethodPost:
			s.apiCreateUser(w, r, sc)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		}
	case (len(parts) == 2 || len(parts) == 3) && parts[0] == "users":
		u, ok := s.apiFindUser(parts[1], sc.rid)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
			return
		}
		if len(parts) == 2 {
			switch r.Method {
			case http.MethodGet:
				writeJSON(w, http.StatusOK, s.apiUser(u))
			case http.MethodPatch, http.MethodPut:
				s.apiUpdateUser(w, r, u, sc)
			case http.MethodDelete:
				s.deleteUser(u, sc.actor)
				writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
			default:
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			}
			return
		}
		s.apiUserAction(w, r, u, parts[2], sc)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "接口不存在"})
	}
}

// apiFindUser 按名称定位;找不到且形如数字时按 id。代理作用域下只在自己名下找。
func (s *Server) apiFindUser(key string, rid uint) (model.User, bool) {
	q := func() *gorm.DB {
		if rid > 0 {
			return s.db.Where("reseller_id = ?", rid)
		}
		return s.db
	}
	var u model.User
	if err := q().Where("name = ?", key).First(&u).Error; err == nil {
		return u, true
	}
	if id, err := strconv.ParseUint(key, 10, 64); err == nil {
		if err := q().First(&u, uint(id)).Error; err == nil {
			return u, true
		}
	}
	return u, false
}

// apiUserView 对外用户视图:去掉凭据,附带用量、线路与订阅地址。
type apiUserView struct {
	Id          uint                `json:"id"`
	Name        string              `json:"name"`
	Enabled     bool                `json:"enabled"`
	Volume      int64               `json:"volume"`
	Used        int64               `json:"used"`
	Up          int64               `json:"up"`
	Down        int64               `json:"down"`
	TotalUp     int64               `json:"totalUp"`
	TotalDown   int64               `json:"totalDown"`
	Expiry      int64               `json:"expiry"`
	Expired     bool                `json:"expired"`
	AutoReset   bool                `json:"autoReset"`
	ResetDays   int                 `json:"resetDays"`
	NextReset   int64               `json:"nextReset"`
	DeviceLimit int                 `json:"deviceLimit"`
	SpeedUp     int                 `json:"speedUp"`
	SpeedDown   int                 `json:"speedDown"`
	Remark      string              `json:"remark"`
	Desc        string              `json:"desc"`
	CreatedAt   int64               `json:"createdAt"`
	OnlineAt    int64               `json:"onlineAt"`
	OnlineIPs   []string            `json:"onlineIps"`
	OnlineLines map[string][]string `json:"onlineLines,omitempty"` // 源 IP → 线路(带服务器后缀)
	LineIds     []uint              `json:"lineIds"`
	LineRefs    []model.LineRef     `json:"lineRefs"`
	ExtIds      []uint              `json:"extIds"`
	SubLink     string              `json:"subLink"`
	SubClash    string              `json:"subClash"`
	SubJSON     string              `json:"subJson"` // sing-box 远程配置地址
}

// apiSnapshot 是列表渲染时"只算一次"的公共数据:在线 IP、线路映射、订阅前缀。
// 逐个用户去问数据面会把限速器与 Hub 的锁抢上几百次(用户一多就拖慢数据面)。
type apiSnapshot struct {
	localIPs, remoteIPs     map[string][]string
	localLines, remoteLines map[string]map[string][]string
	localName, subBase      string
	lineIdsBy, extIdsBy     map[uint][]uint
	refsBy                  map[uint][]model.LineRef
}

func (s *Server) apiSnapshot() *apiSnapshot {
	snap := &apiSnapshot{
		localIPs: map[string][]string{}, remoteIPs: map[string][]string{},
		localLines: map[string]map[string][]string{}, remoteLines: map[string]map[string][]string{},
		localName: s.localNodeName(), subBase: s.subBase(),
		lineIdsBy: s.userLineMap(), extIdsBy: s.userExtMap(), refsBy: s.userLineRefMap(),
	}
	if s.run != nil { // 测试里没有数据面
		snap.localIPs, snap.remoteIPs = s.run.OnlineIPsAll(), s.run.Hub().RemoteIPsAll()
		snap.localLines, snap.remoteLines = s.run.OnlineIPLines(), s.run.Hub().RemoteIPLinesAll()
	}
	return snap
}

func (s *Server) apiUser(u model.User) apiUserView {
	return s.apiUserWith(u, s.apiSnapshot())
}

func (s *Server) apiUserWith(u model.User, snap *apiSnapshot) apiUserView {
	ids, eids, refs := snap.lineIdsBy[u.Id], snap.extIdsBy[u.Id], snap.refsBy[u.Id]
	if ids == nil {
		ids = []uint{}
	}
	if eids == nil {
		eids = []uint{}
	}
	if refs == nil {
		refs = []model.LineRef{}
	}
	base := snap.subBase + subKey(u)
	links := map[string]string{"link": base, "clash": base + "?format=clash", "json": base + "?format=json"}
	now := time.Now().Unix()
	return apiUserView{
		Id: u.Id, Name: u.Name, Enabled: u.Enabled, Volume: u.Volume, Used: u.Up + u.Down, Up: u.Up, Down: u.Down,
		TotalUp: u.TotalUp, TotalDown: u.TotalDown, Expiry: u.Expiry, Expired: u.Expiry > 0 && u.Expiry < now,
		AutoReset: u.AutoReset, ResetDays: u.ResetDays, NextReset: u.NextReset,
		DeviceLimit: u.DeviceLimit, SpeedUp: u.SpeedUp, SpeedDown: u.SpeedDown, Remark: u.Remark, Desc: u.Desc,
		CreatedAt: u.CreatedAt, OnlineAt: u.OnlineAt,
		OnlineIPs:   mergeIPs(snap.localIPs[u.Name], snap.remoteIPs[u.Name]),
		OnlineLines: s.onlineLines(u.Name, snap.localName, snap.localLines, snap.remoteLines),
		LineIds:     ids, LineRefs: refs, ExtIds: eids, SubLink: links["link"], SubClash: links["clash"], SubJSON: links["json"]}
}

func (s *Server) apiListUsers(w http.ResponseWriter, r *http.Request, sc apiScope) {
	q := s.db.Order("id asc")
	if sc.rid > 0 {
		q = q.Where("reseller_id = ?", sc.rid)
	}
	if kw := strings.TrimSpace(r.URL.Query().Get("q")); kw != "" {
		q = q.Where("(name LIKE ? OR remark LIKE ?)", "%"+kw+"%", "%"+kw+"%")
	}
	if v := r.URL.Query().Get("enabled"); v == "true" || v == "false" {
		q = q.Where("enabled = ?", v == "true")
	}
	var users []model.User
	q.Find(&users)
	snap := s.apiSnapshot() // 整表算一次
	out := make([]apiUserView, 0, len(users))
	for _, u := range users {
		out = append(out, s.apiUserWith(u, snap))
	}
	writeJSON(w, http.StatusOK, out)
}

// apiUserReq 创建/修改用户的请求体;指针字段缺省表示"不改动"。
type apiUserReq struct {
	Name        *string          `json:"name"`
	Enabled     *bool            `json:"enabled"`
	PlanId      *uint            `json:"planId"`
	Plan        *string          `json:"plan"` // 套餐名,与 planId 二选一
	Mode        string           `json:"mode"` // 套用套餐的方式:renew(默认,清零用量)| extend(保留用量)
	VolumeGB    *float64         `json:"volumeGb"`
	Volume      *int64           `json:"volume"` // 字节,0=不限;与 volumeGb 二选一
	Days        *int             `json:"days"`   // 创建:自现在起 N 天;修改:在原到期(未过期时)基础上再加 N 天
	Expiry      *int64           `json:"expiry"` // 到期时间戳(秒),0=不限;优先级高于 days
	DeviceLimit *int             `json:"deviceLimit"`
	SpeedUp     *int             `json:"speedUp"`
	SpeedDown   *int             `json:"speedDown"`
	AutoReset   *bool            `json:"autoReset"`
	ResetDays   *int             `json:"resetDays"`
	Remark      *string          `json:"remark"`
	Desc        *string          `json:"desc"`
	LineIds     *[]uint          `json:"lineIds"`
	LineRefs    *[]model.LineRef `json:"lineRefs"` // 线路 × 服务器;给了它就以它为准
	ExtIds      *[]uint          `json:"extIds"`
}

// apiFindPlan 找套餐:主面板令牌只看主面板的套餐,代理令牌只看该代理自己的。
func (s *Server) apiFindPlan(req apiUserReq, rid uint) (*model.Plan, error) {
	var p model.Plan
	switch {
	case req.PlanId != nil && *req.PlanId > 0:
		if err := s.db.Where("COALESCE(reseller_id,0) = ?", rid).First(&p, *req.PlanId).Error; err != nil {
			return nil, errors.New("套餐不存在: id " + strconv.Itoa(int(*req.PlanId)))
		}
	case req.Plan != nil && strings.TrimSpace(*req.Plan) != "":
		if err := s.db.Where("name = ? AND COALESCE(reseller_id,0) = ?", strings.TrimSpace(*req.Plan), rid).First(&p).Error; err != nil {
			return nil, errors.New("套餐不存在: " + *req.Plan)
		}
	default:
		return nil, nil
	}
	return &p, nil
}

// applyReq 把请求里显式给出的字段写到用户上。
func applyReq(u *model.User, req apiUserReq, now int64, creating bool) error {
	if req.Enabled != nil {
		u.Enabled = *req.Enabled
	}
	if req.Volume != nil {
		u.Volume = *req.Volume
	} else if req.VolumeGB != nil {
		u.Volume = int64(*req.VolumeGB * float64(1<<30))
	}
	if req.Expiry != nil {
		u.Expiry = *req.Expiry
	} else if req.Days != nil {
		base := now
		if !creating && u.Expiry > now {
			base = u.Expiry
		}
		if *req.Days <= 0 {
			u.Expiry = 0
		} else {
			u.Expiry = base + int64(*req.Days)*86400
		}
	}
	if req.DeviceLimit != nil {
		u.DeviceLimit = *req.DeviceLimit
	}
	if req.SpeedUp != nil {
		u.SpeedUp = *req.SpeedUp
	}
	if req.SpeedDown != nil {
		u.SpeedDown = *req.SpeedDown
	}
	if req.AutoReset != nil {
		u.AutoReset = *req.AutoReset
	}
	if req.ResetDays != nil {
		u.ResetDays = *req.ResetDays
	}
	if u.AutoReset && u.ResetDays <= 0 {
		return errors.New("autoReset 需要 resetDays > 0")
	}
	if u.AutoReset && u.NextReset == 0 {
		u.NextReset = now + int64(u.ResetDays)*86400
	}
	if !u.AutoReset {
		u.NextReset = 0
	}
	if req.Remark != nil {
		u.Remark = *req.Remark
	}
	if req.Desc != nil {
		u.Desc = *req.Desc
	}
	if u.Volume < 0 || u.Expiry < 0 || u.DeviceLimit < 0 || u.SpeedUp < 0 || u.SpeedDown < 0 {
		return errors.New("数值字段不能为负")
	}
	return nil
}

func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request, sc apiScope) {
	var req apiUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, errors.New("请求体不是合法 JSON: "+err.Error()))
		return
	}
	if req.Name == nil {
		badRequest(w, errors.New("缺少 name"))
		return
	}
	now := time.Now().Unix()
	u := model.User{Name: *req.Name, Enabled: true, CreatedAt: now}
	if err := s.validateUser(&u); err != nil {
		badRequest(w, err)
		return
	}
	plan, err := s.apiFindPlan(req, sc.rid)
	if err != nil {
		badRequest(w, err)
		return
	}
	var refs []model.LineRef
	if plan != nil {
		refs = applyPlanRefs(&u, *plan, "new", now)
	}
	if err := applyReq(&u, req, now, true); err != nil {
		badRequest(w, err)
		return
	}
	switch {
	case req.LineRefs != nil:
		refs = *req.LineRefs
	case req.LineIds != nil:
		refs = lineRefsOf(*req.LineIds, nil)
	}
	if refs == nil {
		// 没指定线路:主面板分配全部线路,代理分配它被授权的全部(含服务器范围),订阅拿到就能用
		if sc.rid > 0 {
			refs = s.resellerLineRefs(sc.rid)
		} else {
			var ids []uint
			s.db.Model(&model.Line{}).Order("sort asc, id asc").Pluck("id", &ids)
			refs = lineRefsOf(ids, nil)
		}
	}
	if sc.rid > 0 {
		// 和代理在面板里建号完全一样的校验与归属:线路授权、用户数上限、流量额度、随机订阅令牌
		if err := s.prepareResellerUser(sc.rid, &u, refs); err != nil {
			badRequest(w, err)
			return
		}
	}
	u.Credentials = generateCredentials(u.Name)
	s.applySubTokenPolicy(&u) // 外部 API 建号也按设置来
	if err := s.db.Create(&u).Error; err != nil {
		badRequest(w, err)
		return
	}
	s.setUserLineRefs(u.Id, refs)
	if req.ExtIds != nil && sc.rid == 0 { // 外部节点只有主面板能分配
		s.setUserExts(u.Id, *req.ExtIds)
	}
	action := "create"
	if plan != nil {
		action += ":plan:" + plan.Name
	}
	s.auditAs(sc.actor, "user", action, u.Name)
	s.reloadUsers("外部 API 新增用户 " + u.Name)
	writeJSON(w, http.StatusOK, s.apiUser(u))
}

func (s *Server) apiUpdateUser(w http.ResponseWriter, r *http.Request, u model.User, sc apiScope) {
	var req apiUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, errors.New("请求体不是合法 JSON: "+err.Error()))
		return
	}
	now := time.Now().Unix()
	plan, err := s.apiFindPlan(req, sc.rid)
	if err != nil {
		badRequest(w, err)
		return
	}
	var planRefsV []model.LineRef
	if plan != nil {
		mode := req.Mode
		if mode != "extend" {
			mode = "renew"
		}
		planRefsV = applyPlanRefs(&u, *plan, mode, now)
	}
	if req.Name != nil && *req.Name != u.Name {
		u.Name = *req.Name
		if err := s.validateUser(&u); err != nil {
			badRequest(w, err)
			return
		}
	}
	if err := applyReq(&u, req, now, false); err != nil {
		badRequest(w, err)
		return
	}
	var refs []model.LineRef
	switch {
	case req.LineRefs != nil:
		refs = *req.LineRefs
	case req.LineIds != nil:
		refs = lineRefsOf(*req.LineIds, nil)
	case planRefsV != nil:
		refs = planRefsV
	}
	if sc.rid > 0 {
		check := refs
		if check == nil { // 没改线路就按现有的校验(授权可能已被主面板收回)
			check = s.userLineRefs(u.Id)
		}
		if err := s.checkResellerUser(sc.rid, u.Id, &u, check); err != nil {
			badRequest(w, err)
			return
		}
	}
	if err := s.db.Model(&model.User{}).Where("id = ?", u.Id).Select(
		"name", "enabled", "volume", "expiry", "auto_reset", "reset_days", "next_reset",
		"device_limit", "speed_up", "speed_down", "remark", "desc", "total_up", "total_down", "up", "down",
	).Updates(u).Error; err != nil {
		badRequest(w, err)
		return
	}
	if refs != nil {
		s.setUserLineRefs(u.Id, refs)
	}
	if req.ExtIds != nil && sc.rid == 0 {
		s.setUserExts(u.Id, *req.ExtIds)
	}
	action := "update"
	if plan != nil {
		action += ":plan:" + plan.Name
	}
	s.auditAs(sc.actor, "user", action, u.Name)
	s.reloadUsers("外部 API 修改用户 " + u.Name)
	writeJSON(w, http.StatusOK, s.apiUser(u))
}

func (s *Server) apiUserAction(w http.ResponseWriter, r *http.Request, u model.User, action string, sc apiScope) {
	if action == "sub" {
		writeJSON(w, http.StatusOK, s.subLinks(u))
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	switch action {
	case "enable", "disable":
		on := action == "enable"
		s.db.Model(&model.User{}).Where("id = ?", u.Id).Update("enabled", on)
		u.Enabled = on
		if !on {
			s.kickUser(u.Name)
		}
		s.auditAs(sc.actor, "user", action, u.Name)
		s.reloadUsers("外部 API " + action + " " + u.Name)
		writeJSON(w, http.StatusOK, s.apiUser(u))
	case "reset":
		if err := s.db.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{
			"total_up": gorm.Expr("total_up + up"), "total_down": gorm.Expr("total_down + down"),
			"up": 0, "down": 0, "enabled": true,
		}).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.auditAs(sc.actor, "user", "reset", u.Name)
		s.reloadUsers("外部 API 重置流量 " + u.Name)
		s.db.First(&u, u.Id)
		writeJSON(w, http.StatusOK, s.apiUser(u))
	case "kick":
		n := s.kickUser(u.Name)
		s.auditAs(sc.actor, "user", "kick", u.Name)
		writeJSON(w, http.StatusOK, map[string]int{"closed": n})
	case "plan", "renew":
		var req apiUserReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badRequest(w, errors.New("请求体不是合法 JSON: "+err.Error()))
			return
		}
		plan, err := s.apiFindPlan(req, sc.rid)
		if err != nil {
			badRequest(w, err)
			return
		}
		if plan == nil {
			badRequest(w, errors.New("缺少 planId 或 plan"))
			return
		}
		mode := req.Mode
		if mode != "extend" {
			mode = "renew"
		}
		if sc.rid > 0 {
			if err := s.checkResellerPlan(sc.rid, u, *plan); err != nil {
				badRequest(w, err)
				return
			}
		}
		if err := s.applyUserPlan(&u, *plan, mode); err != nil {
			badRequest(w, err)
			return
		}
		s.auditAs(sc.actor, "user", "plan:"+mode+":"+plan.Name, u.Name)
		s.reloadUsers("外部 API 套餐 " + plan.Name + " → " + u.Name)
		writeJSON(w, http.StatusOK, s.apiUser(u))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "接口不存在"})
	}
}

// kickUser 断开某用户的全部连接;测试里没有数据面时返回 0。
func (s *Server) kickUser(name string) int {
	if s.run == nil {
		return 0
	}
	return s.run.KickUser(name)
}
