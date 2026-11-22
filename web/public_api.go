package web

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"

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
//	POST   /api/v1/users/{name|id}/rotate   重置订阅链接:新随机地址 + 新凭据,收回临时共享,旧的立即失效
//	POST   /api/v1/users/{name|id}/plan     续费 / 延期 N 期 {planId|plan(不给 = 当前套餐), mode: renew|extend, periods, dryRun, requestId}(/renew 同)
//	POST   /api/v1/users/{name|id}/change   换套餐 {planId|plan, mode: replace|stack|prorate, periods, usage, force, dryRun, requestId}
//	GET    /api/v1/users/{name|id}/plan-ops 套餐记录(最新的在前)
//	GET    /api/v1/users/{name|id}/sub      订阅地址
//	GET    /api/v1/users/{name|id}/traffic  每天的用量(?days=30,最多 90)
//	GET    /api/v1/overview                 面板概况:用户、在线、今天 / 本月 / 累计流量、线路(主面板另有服务器数)
//	GET    /api/v1/servers                  服务器:在线、版本、在线人数(只有主面板的密钥能看)
//	GET    /api/v1/lines                    线路与在线人数
//	GET    /api/v1/onlines                  在线用户、设备数与所在线路
//
// 密钥与权限:每把密钥按它勾的权限放行(见 api_keys.go 的 apiPermFor),没有权限返回 403;
// 限了来源 IP 的密钥,名单外的来源返回 403。升级前就有的那一把是「默认密钥」,默认全部权限、不限来源。
//
// 套餐操作支持幂等:requestId 或请求头 Idempotency-Key,同一作用域同一个键只执行一次(见 plan_change.go)。

// apiToken 从请求头取令牌:Authorization: Bearer 优先,其次 X-API-Key。
func apiToken(r *http.Request) string {
	got := r.Header.Get("X-API-Key")
	if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
		got = strings.TrimSpace(h[7:])
	}
	return got
}

// apiScope 一次调用的作用域:主面板的密钥 rid=0 看全部;代理的密钥只看自己名下。
type apiScope struct {
	rid   uint
	actor string // 审计里的操作者:api / api:代理名 / api:密钥名 / api:代理名/密钥名
	// perms 这把密钥的权限(见 api_keys.go);nil = 不按密钥限(面板自己的调用,比如 Telegram 机器人)
	perms map[string]bool
	keyId uint // 密钥在表里的 id(记最近使用)
	// 这把密钥限不限来源、允许哪些来源(见 fromAllowed)
	ipAllow bool
	ipList  string
}

func (s *Server) handlePublicAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.apiAuthBlocked(s.sourceIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "失败次数过多,请 5 分钟后再试"})
		return
	}
	sc, ok, known := s.adminAPIAuth(r)
	if !ok {
		if !known {
			s.noteAPIAuthFailure(r, "")
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "外部 API 未开启或令牌错误"})
		return
	}
	if !sc.fromAllowed(s.sourceIP(r)) { // 密钥对、只是来源不在它允许的范围里:不算猜令牌,不计入失败限流
		writeIPDenied(w)
		return
	}
	limitJSONBody(w, r, apiBodyLimit)
	// 副机上的写操作会被主机下一次快照整表覆盖(新号消失、删掉的号复活),调用方却以为成功了;与面板一样只放行只读与踢线
	if s.role() == "node" && r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/kick") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": nodeReadOnly})
		return
	}
	s.publicAPI(w, r, sc)
}

// handleResellerPublicAPI 代理面板上的 /api/v1/:令牌属于哪个代理,就只操作那个代理名下的东西。
func (s *Server) handleResellerPublicAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.apiAuthBlocked(s.sourceIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "失败次数过多,请 5 分钟后再试"})
		return
	}
	_, sc, ok, known := s.resellerAPIAuth(r)
	if !ok {
		if !known {
			s.noteAPIAuthFailure(r, "代理")
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "外部 API 未开启或令牌错误"})
		return
	}
	if !sc.fromAllowed(s.sourceIP(r)) {
		writeIPDenied(w)
		return
	}
	limitJSONBody(w, r, apiBodyLimit)
	s.publicAPI(w, r, sc)
}

func (s *Server) publicAPI(w http.ResponseWriter, r *http.Request, sc apiScope) {
	done, ok, msg := s.haWrite(r) // 正在设为主机:写操作暂停;没冻结的写登记在途
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msg})
		return
	}
	defer done()
	full := strings.TrimSuffix(r.URL.Path, "/")
	idx := strings.LastIndex(full, "/v1/")
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(full[idx+4:], "/")
	if need := apiPermFor(r.Method, parts); !sc.allowed(need) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "这把密钥没有「" + permTitle(need) + "」权限", "permission": need})
		return
	}
	if sc.keyId > 0 {
		s.noteKeyUse(sc.keyId, s.sourceIP(r))
	}
	switch {
	case len(parts) == 1 && parts[0] == "overview" && r.Method == http.MethodGet:
		s.apiOverview(w, sc)
	case len(parts) == 1 && parts[0] == "servers" && r.Method == http.MethodGet:
		s.apiServers(w, sc)
	case len(parts) == 1 && parts[0] == "lines" && r.Method == http.MethodGet:
		s.apiLines(w, sc)
	case len(parts) == 1 && parts[0] == "onlines" && r.Method == http.MethodGet:
		s.apiOnlines(w, sc)
	case len(parts) == 3 && parts[0] == "users" && parts[2] == "traffic" && r.Method == http.MethodGet:
		u, ok := s.apiFindUser(parts[1], sc.rid)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
			return
		}
		s.apiUserTraffic(w, r, u)
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
	// POST /users/lookup、POST /users/batch:单个用户的地址上没有 POST 这个方法,不会和叫 lookup / batch 的用户撞
	case len(parts) == 2 && parts[0] == "users" && parts[1] == "lookup" && r.Method == http.MethodPost:
		s.apiLookupUser(w, r, sc)
	case len(parts) == 2 && parts[0] == "users" && parts[1] == "batch" && r.Method == http.MethodPost:
		s.apiBatchCreate(w, r, sc)
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
				if err := s.deleteUser(u, sc.actor); err != nil { // 没删掉(用户还在、还能连)不能报成功
					badRequest(w, err)
					return
				}
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
	Id             uint                `json:"id"`
	Name           string              `json:"name"`
	Enabled        bool                `json:"enabled"`
	Volume         int64               `json:"volume"`
	Used           int64               `json:"used"`
	Up             int64               `json:"up"`
	Down           int64               `json:"down"`
	TotalUp        int64               `json:"totalUp"`
	TotalDown      int64               `json:"totalDown"`
	Expiry         int64               `json:"expiry"`
	ActivateDays   int                 `json:"activateDays"`
	PlanId         uint                `json:"planId"`
	Expired        bool                `json:"expired"`
	AutoReset      bool                `json:"autoReset"`
	ResetDays      int                 `json:"resetDays"`
	NextReset      int64               `json:"nextReset"`
	DeviceLimit    int                 `json:"deviceLimit"`
	SpeedUp        int                 `json:"speedUp"`
	SpeedDown      int                 `json:"speedDown"`
	Remark         string              `json:"remark"`
	Desc           string              `json:"desc"`
	CreatedAt      int64               `json:"createdAt"`
	OnlineAt       int64               `json:"onlineAt"`
	DisabledReason string              `json:"disabledReason,omitempty"` // 停用原因:manual / quota / expired
	ActiveLimits   []apiLimit          `json:"activeLimits,omitempty"`   // 生效中的规则限速(只读)
	OnlineIPs      []string            `json:"onlineIps"`
	OnlineLines    map[string][]string `json:"onlineLines,omitempty"` // 源 IP → 线路(带服务器后缀)
	LineIds        []uint              `json:"lineIds"`
	LineRefs       []model.LineRef     `json:"lineRefs"`
	ExtIds         []uint              `json:"extIds"`
	SubLink        string              `json:"subLink"`
	SubClash       string              `json:"subClash"`
	SubJSON        string              `json:"subJson"` // sing-box 远程配置地址
}

// apiSnapshot 是列表渲染时"只算一次"的公共数据:在线 IP、线路映射、订阅前缀。
// 逐个用户去问数据面会把限速器与 Hub 的锁抢上几百次(用户一多就拖慢数据面)。
// apiLimit 一条生效中的规则限速。
type apiLimit struct {
	Rule     string `json:"rule"`
	UpMbps   int    `json:"upMbps"`
	DownMbps int    `json:"downMbps"`
	Since    int64  `json:"since"`
	Until    int64  `json:"until"` // 0 = 时段规则,随窗口结束
	Reason   string `json:"reason"`
}

// userLimitMap 用户 id → 生效中的规则限速。
func (s *Server) userLimitMap() map[uint][]apiLimit {
	var states []model.LimitState
	s.db.Where("until = 0 OR until > ?", time.Now().Unix()).Order("since desc").Find(&states)
	out := map[uint][]apiLimit{}
	for _, st := range states {
		out[st.UserId] = append(out[st.UserId], apiLimit{Rule: st.RuleName, UpMbps: st.UpMbps, DownMbps: st.DownMbps, Since: st.Since, Until: st.Until, Reason: st.Reason})
	}
	return out
}

type apiSnapshot struct {
	limitsBy                map[uint][]apiLimit
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
		lineIdsBy: map[uint][]uint{}, extIdsBy: s.userExtMap(), refsBy: s.userLineRefMap(), limitsBy: s.userLimitMap(),
	}
	for uid, refs := range snap.refsBy {
		snap.lineIdsBy[uid] = refIDs(refs)
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
		TotalUp: u.TotalUp, TotalDown: u.TotalDown, Expiry: u.Expiry, ActivateDays: u.ActivateDays, PlanId: u.PlanId, Expired: u.Expiry > 0 && u.Expiry < now,
		AutoReset: u.AutoReset, ResetDays: u.ResetDays, NextReset: u.NextReset,
		DeviceLimit: u.DeviceLimit, SpeedUp: u.SpeedUp, SpeedDown: u.SpeedDown, Remark: u.Remark, Desc: u.Desc,
		CreatedAt: u.CreatedAt, OnlineAt: u.OnlineAt, DisabledReason: u.DisabledReason, ActiveLimits: snap.limitsBy[u.Id],
		OnlineIPs:   mergeIPs(snap.localIPs[u.Name], snap.remoteIPs[u.Name]),
		OnlineLines: s.onlineLines(u.Name, snap.localName, snap.localLines, snap.remoteLines),
		LineIds:     ids, LineRefs: refs, ExtIds: eids, SubLink: links["link"], SubClash: links["clash"], SubJSON: links["json"]}
}

// apiListUsers GET /users:按 id 升序。筛选:q(名字 / 备注)、enabled、state(active / expiring / expired / exhausted / disabled)、
// days(state=expiring 时看几天内到期,默认 7)、planId、online;分页:limit(1–1000)、offset。
// 响应头 X-Total-Count 是筛完、分页之前的总数(续费提醒、对账这类要翻页的机器人靠它知道翻没翻完)。
func (s *Server) apiListUsers(w http.ResponseWriter, r *http.Request, sc apiScope) {
	qs := r.URL.Query()
	now := time.Now().Unix()
	q := s.db.Order("id asc")
	if sc.rid > 0 {
		q = q.Where("reseller_id = ?", sc.rid)
	}
	if kw := strings.TrimSpace(qs.Get("q")); kw != "" {
		q = q.Where("(name LIKE ? OR remark LIKE ?)", "%"+kw+"%", "%"+kw+"%")
	}
	if v := qs.Get("enabled"); v == "true" || v == "false" {
		q = q.Where("enabled = ?", v == "true")
	}
	switch state := strings.ToLower(strings.TrimSpace(qs.Get("state"))); state {
	case "":
	case "active": // 启用、没到期、没用完
		q = q.Where("enabled = ? AND (expiry = 0 OR expiry >= ?) AND (volume = 0 OR up + down < volume)", true, now)
	case "expiring": // 启用着、days 天内到期(还没到)
		days := 7
		if v := strings.TrimSpace(qs.Get("days")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 3650 {
				badRequest(w, errors.New("days 填 1–3650 的整数"))
				return
			}
			days = n
		}
		q = q.Where("enabled = ? AND expiry >= ? AND expiry <= ?", true, now, now+int64(days)*86400)
	case "expired":
		q = q.Where("expiry > 0 AND expiry < ?", now)
	case "exhausted":
		q = q.Where("volume > 0 AND up + down >= volume")
	case "disabled":
		q = q.Where("enabled = ?", false)
	default:
		badRequest(w, errors.New("state 只能是 active、expiring、expired、exhausted、disabled"))
		return
	}
	if v := strings.TrimSpace(qs.Get("planId")); v != "" {
		id, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			badRequest(w, errors.New("planId 要是数字(0 = 没有套餐的)"))
			return
		}
		q = q.Where("COALESCE(plan_id, 0) = ?", id)
	}
	limit, offset := 0, 0
	if v := strings.TrimSpace(qs.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			badRequest(w, errors.New("limit 填 1–1000 的整数"))
			return
		}
		limit = n
	}
	if v := strings.TrimSpace(qs.Get("offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			badRequest(w, errors.New("offset 填不小于 0 的整数"))
			return
		}
		offset = n
	}
	var users []model.User
	q.Find(&users)
	snap := s.apiSnapshot() // 整表算一次
	// 在不在线要看数据面(本机 + 各副机的报告),库里筛不了:取出来再筛
	if v := qs.Get("online"); v == "true" || v == "false" {
		want := v == "true"
		kept := users[:0]
		for _, u := range users {
			if (len(snap.localIPs[u.Name])+len(snap.remoteIPs[u.Name]) > 0) == want {
				kept = append(kept, u)
			}
		}
		users = kept
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(len(users)))
	if offset > len(users) {
		offset = len(users)
	}
	users = users[offset:]
	if limit > 0 && limit < len(users) {
		users = users[:limit]
	}
	out := make([]apiUserView, 0, len(users))
	for _, u := range users {
		out = append(out, s.apiUserWith(u, snap))
	}
	writeJSON(w, http.StatusOK, out)
}

// apiUserReq 创建/修改用户的请求体;指针字段缺省表示"不改动"。
type apiUserReq struct {
	Name         *string  `json:"name"`
	Enabled      *bool    `json:"enabled"`
	PlanId       *uint    `json:"planId"`
	Plan         *string  `json:"plan"` // 套餐名,与 planId 二选一
	Mode         string   `json:"mode"` // 套用套餐的方式:renew(默认,清零用量)| extend(保留用量)
	VolumeGB     *float64 `json:"volumeGb"`
	Volume       *int64   `json:"volume"`       // 字节,0=不限;与 volumeGb 二选一
	Days         *int     `json:"days"`         // 创建:自现在起 N 天;修改:在原到期(未过期时)基础上再加 N 天
	Expiry       *int64   `json:"expiry"`       // 到期时间戳(秒),0=不限;优先级高于 days
	ActivateDays *int     `json:"activateDays"` // 首次连接后 N 天到期;填了就不看 expiry / days
	DeviceLimit  *int     `json:"deviceLimit"`
	SpeedUp      *int     `json:"speedUp"`
	SpeedDown    *int     `json:"speedDown"`
	AutoReset    *bool    `json:"autoReset"`
	ResetDays    *int     `json:"resetDays"`
	Remark       *string  `json:"remark"`
	Desc         *string  `json:"desc"`
	// Prefix 建号时没给 name:名字自动起成「前缀-8 位随机字符」(不给前缀用 u);批量建号同理
	Prefix   *string          `json:"prefix,omitempty"`
	LineIds  *[]uint          `json:"lineIds"`
	LineRefs *[]model.LineRef `json:"lineRefs"` // 线路 × 服务器;给了它就以它为准
	ExtIds   *[]uint          `json:"extIds"`
	// RequestId 幂等键(也可以用请求头 Idempotency-Key):建号(POST /users)认;修改时同时带了套餐(等同一次续费)才认
	RequestId string `json:"requestId"`
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

// errDaysTooMany 请求里给的天数(首次连接后天数、重置周期、days、套餐天数、批量延期)超过上限:换算成秒会溢出。
var errDaysTooMany = fmt.Errorf("天数不能超过 %d(约 1 万年)", model.MaxDays)

// applyReq 把请求里显式给出的字段写到用户上。
func applyReq(u *model.User, req apiUserReq, now int64, creating bool) error {
	for _, d := range []*int{req.ActivateDays, req.Days, req.ResetDays} {
		if d != nil && *d > model.MaxDays {
			return errDaysTooMany
		}
	}
	if req.Enabled != nil { // 显式给了启停就是手动决定
		u.Enabled, u.DisabledReason = *req.Enabled, ""
		if !u.Enabled {
			u.DisabledReason = model.DisabledManual
		}
	}
	if req.Volume != nil {
		u.Volume = *req.Volume
	} else if req.VolumeGB != nil {
		u.Volume = model.GBToBytes(*req.VolumeGB)
	}
	if req.ActivateDays != nil {
		if *req.ActivateDays < 0 {
			return errors.New("首次连接后的天数不能为负")
		}
		u.ActivateDays = *req.ActivateDays
		if u.ActivateDays > 0 {
			u.Expiry = 0
		}
	}
	if req.ActivateDays != nil && *req.ActivateDays > 0 {
		// 这次明确要首次连接才计时:expiry / days 不看
	} else if req.Expiry != nil {
		u.Expiry, u.ActivateDays = *req.Expiry, 0 // 明确给了到期时间:不再等首次连接
	} else if req.Days != nil && u.ActivateDays > 0 && *req.Days > 0 {
		u.ActivateDays = model.SumDays(u.ActivateDays, *req.Days) // 还没开始计时的用户:加到等待计时的天数上
	} else if req.Days != nil {
		u.ActivateDays = 0
		base := now
		if !creating && u.Expiry > now {
			base = u.Expiry
		}
		if *req.Days <= 0 {
			u.Expiry = 0
		} else {
			u.Expiry = model.AddDays(base, *req.Days)
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
		u.NextReset = model.AddDays(now, u.ResetDays)
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
	if req.Enabled == nil { // 没碰启停:超量 / 到期被停的人,改回范围内就自动恢复
		autoEnable(u, now)
	}
	return nil
}

// updateUserFields 返回用户资料更新所允许写入的字段。
// 用量字段刻意不从请求前读出的 User 结构体写回：数据面可能在请求期间
// 增加 up/down，直接 Updates(struct) 会把那一小段流量覆盖掉。renew 的
// 结算项由调用方以 SQL 表达式原子完成。
func updateUserFields(u model.User, renew bool) map[string]interface{} {
	updates := map[string]interface{}{
		"name":            u.Name,
		"enabled":         u.Enabled,
		"disabled_reason": u.DisabledReason,
		"volume":          u.Volume,
		"expiry":          u.Expiry,
		"activate_days":   u.ActivateDays,
		"plan_id":         u.PlanId,
		"device_limit":    u.DeviceLimit,
		"speed_up":        u.SpeedUp,
		"speed_down":      u.SpeedDown,
		"auto_reset":      u.AutoReset,
		"reset_days":      u.ResetDays,
		"next_reset":      u.NextReset,
		"remark":          u.Remark,
		"desc":            u.Desc,
	}
	if renew {
		// total_* + up/down 必须在数据库内完成，和统计写入并发时不会丢本轮流量。
		updates["total_up"] = gorm.Expr("total_up + up")
		updates["total_down"] = gorm.Expr("total_down + down")
		updates["up"], updates["down"] = int64(0), int64(0)
	}
	return updates
}

// apiCreateUser POST /users。name 可以不给(自动起名,见 apiAutoName);带了幂等键(requestId / Idempotency-Key)时
// 同一个键只建一次:重试返回当时建的那个用户(响应头 Idempotent-Replayed: true),同键不同内容返回 409。
func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request, sc apiScope) {
	var req apiUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, errors.New("请求体不是合法 JSON: "+err.Error()))
		return
	}
	key, err := planRequestID(r, req.RequestId)
	if err != nil {
		writePlanErr(w, err)
		return
	}
	hash := ""
	if key != "" {
		canon := req
		canon.RequestId = ""
		hash = planHash("create", canon)
		// 建过了:返回当时建的那个用户(重试安全)。并发的两次同键请求靠下面事务里的主键挡住其中一次
		if u, found, err := s.apiCreateReplay(sc.rid, key, hash); err != nil {
			writePlanErr(w, err)
			return
		} else if found {
			w.Header().Set("Idempotent-Replayed", "true")
			writeJSON(w, http.StatusOK, s.apiUser(u))
			return
		}
	}
	u, plan, err := s.apiCreateOne(req, sc, key, hash)
	if err != nil {
		if key != "" { // 撞了主键 = 同一个键的另一次请求刚建完:按重放回
			if prev, found, e2 := s.apiCreateReplay(sc.rid, key, hash); e2 == nil && found {
				w.Header().Set("Idempotent-Replayed", "true")
				writeJSON(w, http.StatusOK, s.apiUser(prev))
				return
			}
		}
		writePlanErr(w, err)
		return
	}
	action := "create"
	if plan != nil {
		action += ":plan:" + plan.Name
	}
	s.auditAs(sc.actor, "user", action, u.Name)
	s.reloadUsers("外部 API 新增用户 " + u.Name)
	writeJSON(w, http.StatusOK, s.apiUser(u))
}

// apiCreateReplay 这个幂等键是不是已经建过用户:建过且内容相同返回那个用户;内容不同、或者那个用户已经删了,返回 409。
func (s *Server) apiCreateReplay(rid uint, key, hash string) (model.User, bool, error) {
	var rec model.ApiCreate
	if err := s.db.Where("reseller_id = ? AND request_id = ?", rid, key).Limit(1).Find(&rec).Error; err != nil {
		return model.User{}, false, err
	}
	if rec.RequestId == "" {
		return model.User{}, false, nil
	}
	if rec.ReqHash != hash {
		return model.User{}, false, opErr(http.StatusConflict, "idempotency_conflict", errPlanIdemConflict)
	}
	var u model.User
	if err := s.db.Where("id = ?", rec.UserId).Limit(1).Find(&u).Error; err != nil {
		return model.User{}, false, err
	}
	if u.Id == 0 {
		return model.User{}, false, opErr(http.StatusConflict, "idempotency_gone", errors.New("这个 requestId 建的用户已经删除:换一个 requestId 重新建"))
	}
	return u, true, nil
}

// apiNamePrefixRe 自动起名的前缀:字母数字开头,可带 . _ -,最长 24。
var apiNamePrefixRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,23}$`)

// apiAutoName 没给 name 时起一个:「前缀-8 位随机小写字母数字」,和现有用户不重名。
func (s *Server) apiAutoName(prefix *string) (string, error) {
	p := "u"
	if prefix != nil && strings.TrimSpace(*prefix) != "" {
		p = strings.TrimSpace(*prefix)
		if !apiNamePrefixRe.MatchString(p) {
			return "", errors.New("prefix 要以字母或数字开头,只能含字母、数字和 . _ -,最长 24 个字符")
		}
	}
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789" // 去掉了容易看错的 l o 0 1
	for try := 0; try < 8; try++ {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		for i := range b {
			b[i] = alphabet[int(b[i])%len(alphabet)]
		}
		name := p + "-" + string(b)
		var n int64
		if err := s.db.Model(&model.User{}).Where("name = ?", name).Count(&n).Error; err != nil {
			return "", err
		}
		if n == 0 {
			return name, nil
		}
	}
	return "", errors.New("自动起名连续重名,请重试")
}

// apiCreateOne 建一个用户并落库(不写审计、不重载数据面、不回响应:单个建号和批量建号共用)。
// key 非空时在同一笔事务里记下幂等键(主键相同的另一次请求会在这里失败、整体回滚)。
func (s *Server) apiCreateOne(req apiUserReq, sc apiScope, key, hash string) (model.User, *model.Plan, error) {
	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if name == "" {
		auto, err := s.apiAutoName(req.Prefix)
		if err != nil {
			return model.User{}, nil, err
		}
		name = auto
	}
	now := time.Now().Unix()
	u := model.User{Name: name, Enabled: true, CreatedAt: now}
	if err := s.validateUser(&u); err != nil {
		return model.User{}, nil, err
	}
	plan, err := s.apiFindPlan(req, sc.rid)
	if err != nil {
		return model.User{}, nil, err
	}
	var refs []model.LineRef
	if plan != nil {
		refs = applyPlanRefs(&u, *plan, "new", now)
	}
	if err := applyReq(&u, req, now, true); err != nil {
		return model.User{}, nil, err
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
		s.resellerUserMu.Lock()
		defer s.resellerUserMu.Unlock()
		if err := s.prepareResellerUser(sc.rid, &u, refs); err != nil {
			return model.User{}, nil, err
		}
	}
	u.Credentials = generateCredentials(u.Name)
	s.applySubTokenPolicy(&u) // 外部 API 建号也按设置来
	disabled := !u.Enabled    // Create 之后 gorm 会把默认值 true 回填进结构体,先记下本意
	refs = s.normalizeRefs(refs)
	if err := s.checkRefHides(refs); err != nil {
		return model.User{}, nil, err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error { // 用户与分配一起落库:任一步失败整体回滚并报错
		if err := tx.Create(&u).Error; err != nil {
			return err
		}
		if key != "" {
			if err := tx.Create(&model.ApiCreate{ResellerId: sc.rid, RequestId: key, ReqHash: hash, UserId: u.Id, CreatedAt: now}).Error; err != nil {
				return err
			}
			tx.Where("created_at < ?", now-90*86400).Delete(&model.ApiCreate{}) // 顺手清掉三个月以前的
		}
		if err := ensureDisabled(tx, &u, disabled); err != nil {
			return err
		}
		if err := writeUserLineRefs(tx, u.Id, refs); err != nil {
			return err
		}
		if req.ExtIds != nil && sc.rid == 0 { // 外部节点只有主面板能分配
			return writeUserExts(tx, u.Id, *req.ExtIds, nil)
		}
		return nil
	})
	if err != nil {
		return model.User{}, nil, err
	}
	return u, plan, nil
}

// apiBatchCreate POST /users/batch {count, prefix, …和 POST /users 一样的字段(name 不认)}:一次建 count 个(1–100),
// 名字自动起成「前缀-随机」。逐个建:中途有一个建不了(代理的用户数 / 额度到顶之类)就停,已经建好的照样返回。
// 返回 {users: [用户对象…], requested, created[, error]};一个都没建成是 400。
func (s *Server) apiBatchCreate(w http.ResponseWriter, r *http.Request, sc apiScope) {
	var req struct {
		apiUserReq
		Count int `json:"count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, errors.New("请求体不是合法 JSON: "+err.Error()))
		return
	}
	if req.Count < 1 || req.Count > 100 {
		badRequest(w, errors.New("count 填 1–100"))
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		badRequest(w, errors.New("批量建号的名字是自动起的:不要给 name,用 prefix 指定前缀"))
		return
	}
	if req.RequestId != "" || strings.TrimSpace(r.Header.Get("Idempotency-Key")) != "" {
		badRequest(w, errors.New("批量建号不认幂等键:要重试安全就逐个调 POST /users 并各带一个 requestId"))
		return
	}
	req.Name = nil
	users := make([]model.User, 0, req.Count)
	var stop error
	var plan *model.Plan
	for i := 0; i < req.Count; i++ {
		u, p, err := s.apiCreateOne(req.apiUserReq, sc, "", "")
		if err != nil {
			stop = err
			break
		}
		users, plan = append(users, u), p
	}
	if len(users) == 0 {
		writePlanErr(w, stop)
		return
	}
	action := "create"
	if plan != nil {
		action += ":plan:" + plan.Name
	}
	names := make([]string, len(users))
	for i, u := range users {
		names[i] = u.Name
	}
	s.auditAs(sc.actor, "user", action, strings.Join(names, ", "))
	s.reloadUsers(fmt.Sprintf("外部 API 批量新增 %d 个用户", len(users)))
	snap := s.apiSnapshot()
	out := make([]apiUserView, 0, len(users))
	for _, u := range users {
		out = append(out, s.apiUserWith(u, snap))
	}
	res := map[string]interface{}{"users": out, "requested": req.Count, "created": len(users)}
	if stop != nil {
		res["error"] = stop.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

// apiLookupUser POST /users/lookup {"sub": "<订阅地址,或地址的最后一段>"}:按订阅地址找到是哪个用户(机器人让用户发订阅地址来绑定账号时用)。
// 地址放在请求体里,不进网址和访问日志。只认用户自己的订阅地址(用户名或订阅令牌);临时共享的地址不认 ——
// 拿着别人分享的地址不能把那个账号绑成自己的。代理的密钥只找得到自己名下的。
func (s *Server) apiLookupUser(w http.ResponseWriter, r *http.Request, sc apiScope) {
	var req struct {
		Sub string `json:"sub"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req); err != nil {
		badRequest(w, errors.New("请求体不是合法 JSON: "+err.Error()))
		return
	}
	key := subKeyFromAddress(req.Sub)
	if key == "" {
		badRequest(w, errors.New("缺少 sub(订阅地址,或地址的最后一段)"))
		return
	}
	q := func() *gorm.DB {
		if sc.rid > 0 {
			return s.db.Where("reseller_id = ?", sc.rid)
		}
		return s.db
	}
	var u model.User
	// 和订阅服务认地址的顺序一样:先按用户名(主面板的、没有单独订阅令牌的),再按订阅令牌
	if err := q().Where("name = ? AND COALESCE(reseller_id, 0) = 0 AND COALESCE(sub_token, '') = ''", key).Limit(1).Find(&u).Error; err != nil {
		badRequest(w, err)
		return
	}
	if u.Id == 0 {
		if err := q().Where("sub_token = ?", key).Limit(1).Find(&u).Error; err != nil {
			badRequest(w, err)
			return
		}
	}
	if u.Id == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "没有这个订阅地址对应的用户"})
		return
	}
	writeJSON(w, http.StatusOK, s.apiUser(u))
}

// subKeyFromAddress 从订阅地址里取出认人的那一段:整条地址取路径的最后一段(去掉 ?format= 之类的参数、/qr 这类尾巴);
// 本来就只给了那一段的原样用。取不出来返回空。
func subKeyFromAddress(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 2048 {
		return ""
	}
	if i := strings.IndexAny(v, "?#"); i >= 0 {
		v = v[:i]
	}
	if i := strings.Index(v, "://"); i >= 0 {
		v = v[i+3:]
		j := strings.Index(v, "/")
		if j < 0 {
			return "" // 只有主机名,没有路径
		}
		v = v[j:]
	}
	v = strings.TrimRight(v, "/")
	for _, tail := range []string{"/qr", "/logo"} {
		v = strings.TrimSuffix(v, tail)
	}
	if i := strings.LastIndex(v, "/"); i >= 0 {
		v = v[i+1:]
	}
	if u, err := url.PathUnescape(v); err == nil {
		v = u
	}
	return strings.TrimSpace(v)
}

// apiUpdateUser PATCH / PUT /users/{name|id}:只改请求里给出的字段。同时带了 planId / plan 的等同一次续费,
// 交给 apiUpdateUserPlan 走套餐操作的执行器(记套餐记录、认幂等键);没带套餐的照旧在这里改。
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
	if plan != nil {
		s.apiUpdateUserPlan(w, r, u, sc, req, *plan)
		return
	}
	oldName := u.Name
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
	}
	if sc.rid > 0 {
		check := refs
		if check == nil { // 没改线路就按现有的校验(已与当前授权取交集,收回的线路不算越权)
			check = s.userLineRefs(u.Id)
		}
		if err := s.checkResellerUser(sc.rid, u.Id, &u, check); err != nil {
			badRequest(w, err)
			return
		}
	}
	if refs != nil {
		refs = s.normalizeRefs(refs)
		if err := s.checkRefHides(refs); err != nil {
			badRequest(w, err)
			return
		}
	}
	err = s.db.Transaction(func(tx *gorm.DB) error { // 用户字段与分配一个事务
		if err := tx.Model(&model.User{}).Where("id = ?", u.Id).
			Updates(updateUserFields(u, false)).Error; err != nil {
			return err
		}
		if err := moveUserStats(tx, oldName, u.Name); err != nil {
			return err
		}
		if refs != nil {
			if err := writeUserLineRefs(tx, u.Id, refs); err != nil {
				return err
			}
		}
		if req.ExtIds != nil && sc.rid == 0 {
			return writeUserExts(tx, u.Id, *req.ExtIds, nil)
		}
		return nil
	})
	if err != nil {
		badRequest(w, err)
		return
	}
	s.auditAs(sc.actor, "user", "update", u.Name)
	s.reloadUsers("外部 API 修改用户 " + u.Name)
	s.writeFreshUser(w, u.Id, false) // 返回数据库中的最新用量，避免把请求开始时的旧快照回传给调用方
}

func (s *Server) apiUserAction(w http.ResponseWriter, r *http.Request, u model.User, action string, sc apiScope) {
	if action == "sub" {
		writeJSON(w, http.StatusOK, s.subLinks(u))
		return
	}
	if action == "plan-ops" { // 套餐记录:只读
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return
		}
		s.writePlanOps(w, r, u, sc.rid)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	switch action {
	case "enable", "disable":
		on := action == "enable"
		if _, err := s.setEnabled(s.db, []uint{u.Id}, on); err != nil {
			badRequest(w, err)
			return
		}
		u.Enabled, u.DisabledReason = on, ""
		if !on {
			u.DisabledReason = model.DisabledManual
			s.kickUser(u.Name)
		}
		s.auditAs(sc.actor, "user", action, u.Name)
		s.reloadUsers("外部 API " + action + " " + u.Name)
		writeJSON(w, http.StatusOK, s.apiUser(u))
	case "reset":
		if err := s.resetUsage(s.db, u.Id); err != nil {
			badRequest(w, err)
			return
		}
		s.auditAs(sc.actor, "user", "reset", u.Name)
		s.reloadUsers("外部 API 重置流量 " + u.Name)
		s.db.First(&u, u.Id)
		writeJSON(w, http.StatusOK, s.apiUser(u))
	case "kick":
		res := s.kickUserAll(u.Name)
		if sc.rid > 0 {
			res = res.Scrubbed()
		}
		s.auditAs(sc.actor, "user", "kick", u.Name)
		writeJSON(w, http.StatusOK, res)
	case "rotate": // 重置订阅链接:返回带新订阅地址的用户对象
		nu, err := s.rotateUser(u)
		if err != nil {
			badRequest(w, err)
			return
		}
		s.auditAs(sc.actor, "user", "rotate", u.Name)
		writeJSON(w, http.StatusOK, s.apiUser(nu))
	case "plan", "renew": // 续费 / 延期 N 期;不给套餐就续当前套餐
		s.apiRenew(w, r, u, sc)
	case "change": // 换套餐:replace / stack / prorate
		s.apiChange(w, r, u, sc)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "接口不存在"})
	}
}

// kickUser 断开某用户在本机的全部连接;测试里没有数据面时返回 0。
func (s *Server) kickUser(name string) int {
	if s.run == nil {
		return 0
	}
	return s.run.KickUser(name)
}

// kickUserAll 本机 + 所有副机;测试里没有数据面时返回空结果。
func (s *Server) kickUserAll(name string) hub.KickResult {
	if s.run == nil {
		return hub.KickResult{}
	}
	return s.run.KickUserAll(name)
}
