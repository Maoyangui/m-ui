package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Maoyangui/m-ui/ha"
)

// ---- 主机容灾:面板接口(会话鉴权) ----
//
//	GET  /api/ha             主机:配置 + 就绪检查 + 各候选的副本;副机 / 临时主机 / 已降级:各自的状态
//	POST /api/ha             主机:保存配置
//	POST /api/ha/pause       主机:暂停自动接管 {minutes}
//	GET  /api/ha/divergence  已降级的原主机:副本之后本机还收了哪些改动

func (s *Server) handleHA(w http.ResponseWriter, r *http.Request) {
	h := s.haSvc()
	if h == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "本机容灾服务未初始化"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.View())
	case http.MethodPost:
		var c ha.Config
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&c); err != nil {
			badRequest(w, err)
			return
		}
		if err := h.SaveConfig(c); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "ha", "config", c)
		writeJSON(w, http.StatusOK, h.View())
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// haAction 一个要登录的容灾动作:只收 method 这一种请求。
type haAction struct {
	method string
	fn     func(http.ResponseWriter, *http.Request, *ha.Service)
}

// handleHASub /api/ha/<动作>:各动作自己决定鉴权(有的要会话,迁回与同伴接口用一次性码或 HMAC)。
func (s *Server) handleHASub(w http.ResponseWriter, r *http.Request) {
	h := s.haSvc()
	if h == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "本机容灾服务未初始化"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	action := strings.TrimPrefix(r.URL.Path, innerBase+"api/ha/")
	if a, ok := s.haSessionActions()[action]; ok {
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
		"divergence": {http.MethodGet, s.haDivergence},
	}
}

// haDivergence 已降级的原主机:接管所用副本之后本机还收了哪些改动(人工核对补录用)。
func (s *Server) haDivergence(w http.ResponseWriter, r *http.Request, h *ha.Service) {
	fb, changes, err := h.Divergence(500)
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"fencedBy": fb, "changes": changes})
}

func (s *Server) haPause(w http.ResponseWriter, r *http.Request, h *ha.Service) {
	var body struct {
		Minutes int `json:"minutes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	if err := h.Pause(body.Minutes); err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "ha", "pause", body.Minutes)
	writeJSON(w, http.StatusOK, h.View())
}
