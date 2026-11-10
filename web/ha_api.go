package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/totp"
)

// ---- 主机容灾与设为主机:面板接口(会话鉴权,只认管理员会话;代理面板、外部 API 令牌碰不到) ----
//
//	GET  /api/ha                 状态(主机:设置、备用、换主机的进度;副机:主机失联没有、能不能接任);?full=1 附就绪检查
//	POST /api/ha                 主机:保存主机容灾设置 {config…, password, code}(开关、改模式都要再输一次密码)
//	POST /api/ha/pause           主机:暂停接任 {minutes}(0 = 恢复)
//	GET  /api/ha/plan?id=        主机:把这台设为主机前的检查与说明
//	POST /api/ha/set-master      主机:设为主机 {id, password, code}
//	POST /api/ha/takeover        备用:接任为主机 {password, code}
//	POST /api/ha/repin           主机:重新信任一台备用的公钥 {id}
//	POST /api/ha/notice          清掉一条顶部提示 {id}
//
// 设为主机、接任、改主机容灾都要再输一次管理员密码(开了两步验证再加动态码),失败按登录同一套限流。

func (s *Server) handleHA(w http.ResponseWriter, r *http.Request) {
	if haLimited(w, r) {
		return
	}
	h := s.haSvc()
	if h == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "本机容灾服务未初始化"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.haView(h, r.URL.Query().Get("full") == "1"))
	case http.MethodPost:
		var body struct {
			ha.ConfigInput
			Password string `json:"password"`
			Code     string `json:"code"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			badRequest(w, errBadRequest)
			return
		}
		if !s.reauth(w, r, body.Password, body.Code) {
			return
		}
		before := h.Config()
		if err := h.SaveConfig(body.ConfigInput); err != nil {
			badRequest(w, err)
			return
		}
		after := h.Config()
		s.audit(r, "ha", "config", map[string]interface{}{"mode": after.Mode, "from": before.Mode, "standbys": after.Standbys,
			"witness": after.Witness, "dnsNames": after.DnsNames, "dnsToken": body.DnsToken != "" || body.ClearDnsToken})
		if before.Mode != after.Mode {
			logger.Info("主机容灾:", s.actor(r), " 把模式从 ", before.Mode, " 改成 ", after.Mode)
		}
		writeJSON(w, http.StatusOK, s.haView(h, true))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// haView 状态,外加界面要的:开没开两步验证(确认框里要不要动态码)。
func (s *Server) haView(h *ha.Service, full bool) interface{} {
	return struct {
		ha.View
		Totp bool `json:"totp"`
	}{h.View(full), s.setting("totpEnabled") == "true"}
}

var (
	errBadRequest = errors.New("请求格式错误")
	errBadNodeID  = errors.New("服务器 id 无效")
	errReauth     = errors.New("管理员密码不对")
	errReauthCode = errors.New("两步验证码不对")
)

// reauth 敏感操作前再验一次当前管理员的密码(开了两步验证再验动态码);不通过时已经写好响应,返回 false。
// 失败计入登录限流(同一来源 10 分钟 10 次就挡 5 分钟),验证码不能重放。
func (s *Server) reauth(w http.ResponseWriter, r *http.Request, password, code string) bool {
	ip := peerIP(r)
	if s.loginBlocked(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "失败次数过多,请 5 分钟后再试"})
		return false
	}
	var admin model.Admin
	if err := s.db.Where("username = ?", s.actor(r)).First(&admin).Error; err != nil {
		dummyPasswordCheck(password)
		time.Sleep(300 * time.Millisecond)
		s.noteLoginFailure(ip)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": errReauth.Error()})
		return false
	}
	if !checkPassword(password, admin.Password) {
		time.Sleep(300 * time.Millisecond)
		s.noteLoginFailure(ip)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": errReauth.Error()})
		return false
	}
	if s.setting("totpEnabled") == "true" {
		ok, step := totp.Verify(s.setting("totpSecret"), strings.TrimSpace(code), time.Now())
		s.mu.Lock()
		replay := ok && step <= s.lastTotpStep
		if ok && !replay {
			s.lastTotpStep = step
		}
		s.mu.Unlock()
		if !ok || replay {
			time.Sleep(300 * time.Millisecond)
			s.noteLoginFailure(ip)
			writeJSON(w, http.StatusForbidden, map[string]interface{}{"error": errReauthCode.Error(), "totp": true})
			return false
		}
	}
	return true
}

// haAction 一个要登录的容灾动作:只收 method 这一种请求。
type haAction struct {
	method string
	fn     func(http.ResponseWriter, *http.Request, *ha.Service)
}

// handleHASub /api/ha/<动作>:要登录的走会话鉴权;副机之间互相确认的同伴接口走 HMAC(见 ha_peer.go)。
func (s *Server) handleHASub(w http.ResponseWriter, r *http.Request) {
	h := s.haSvc()
	if h == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "本机容灾服务未初始化"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	action := strings.TrimPrefix(r.URL.Path, innerBase+"api/ha/")
	if strings.HasPrefix(action, "peer/") {
		s.handleHAPeer(w, r, h, strings.TrimPrefix(action, "peer/"))
		return
	}
	if a, ok := s.haSessionActions()[action]; ok {
		if haLimited(w, r) {
			return
		}
		s.auth(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != a.method {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
				return
			}
			a.fn(w, r, h)
		})(w, r)
		return
	}
	http.NotFound(w, r)
}

// haSessionActions 要登录的容灾动作。
func (s *Server) haSessionActions() map[string]haAction {
	return map[string]haAction{
		"pause":      {http.MethodPost, s.haPause},
		"plan":       {http.MethodGet, s.haPlan},
		"set-master": {http.MethodPost, s.haSetMaster},
		"takeover":   {http.MethodPost, s.haTakeover},
		"repin":      {http.MethodPost, s.haRepin},
		"notice":     {http.MethodPost, s.haNotice},
		"dns-test":   {http.MethodPost, s.haDNSTest},
	}
}

func decodeSmall(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(v); err != nil {
		badRequest(w, errBadRequest)
		return false
	}
	return true
}

// haPause 主机:暂停接任 minutes 分钟(0 = 恢复)。
func (s *Server) haPause(w http.ResponseWriter, r *http.Request, h *ha.Service) {
	var body struct {
		Minutes int `json:"minutes"`
	}
	if !decodeSmall(w, r, &body) {
		return
	}
	if err := h.Pause(body.Minutes); err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "ha", "pause", body.Minutes)
	writeJSON(w, http.StatusOK, s.haView(h, false))
}

// haPlan 主机:把一台副机设为主机前的检查与说明。
func (s *Server) haPlan(w http.ResponseWriter, r *http.Request, h *ha.Service) {
	id, err := strconv.ParseUint(r.URL.Query().Get("id"), 10, 32)
	if err != nil || id == 0 {
		badRequest(w, errBadNodeID)
		return
	}
	p, err := h.SetMasterPlan(uint(id))
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ha.SetMasterPlan
		Totp bool `json:"totp"`
	}{p, s.setting("totpEnabled") == "true"})
}

// haSetMaster 主机:把一台副机设为主机。要再输一次管理员密码。
func (s *Server) haSetMaster(w http.ResponseWriter, r *http.Request, h *ha.Service) {
	var body struct {
		Id       uint   `json:"id"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !decodeSmall(w, r, &body) {
		return
	}
	if body.Id == 0 {
		badRequest(w, errBadNodeID)
		return
	}
	if !s.reauth(w, r, body.Password, body.Code) {
		return
	}
	st, err := h.SetMaster(body.Id, s.actor(r))
	if err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "ha", "set-master", map[string]interface{}{"id": body.Id, "name": st.TargetName})
	writeJSON(w, http.StatusOK, st)
}

// haTakeover 备用:主机失联时接任为主机。要再输一次本机管理员的密码。
func (s *Server) haTakeover(w http.ResponseWriter, r *http.Request, h *ha.Service) {
	var body struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !decodeSmall(w, r, &body) {
		return
	}
	if !s.reauth(w, r, body.Password, body.Code) {
		return
	}
	if err := h.Takeover(ha.TakeoverOpts{By: "panel"}); err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "ha", "takeover", nil)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// haRepin 主机:忘掉记住的备用公钥(那台重装过 m-ui 后),下一次见到时重新记。
func (s *Server) haRepin(w http.ResponseWriter, r *http.Request, h *ha.Service) {
	var body struct {
		Id uint `json:"id"`
	}
	if !decodeSmall(w, r, &body) {
		return
	}
	if body.Id == 0 {
		badRequest(w, errBadNodeID)
		return
	}
	if err := h.Repin(body.Id); err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "ha", "repin", body.Id)
	writeJSON(w, http.StatusOK, s.haView(h, false))
}

// haNotice 清掉一条顶部提示(id 为空 = 全清)。
func (s *Server) haNotice(w http.ResponseWriter, r *http.Request, h *ha.Service) {
	var body struct {
		Id string `json:"id"`
	}
	if !decodeSmall(w, r, &body) {
		return
	}
	if err := h.DismissNotice(body.Id); err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// haFrozen 设为主机进行中:写操作一律 503(主面板、外部 API、代理面板、Telegram 机器人)。只读请求放行。
func (s *Server) haFrozen(r *http.Request) (bool, string) {
	h := s.haSvc()
	if h == nil || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false, ""
	}
	if !h.Frozen() {
		return false, ""
	}
	return true, h.FrozenText()
}
