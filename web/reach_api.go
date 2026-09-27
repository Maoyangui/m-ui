package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Maoyangui/m-ui/reach"
)

// handleReach GET:大陆连通检测的整体状态与每台服务器最近的结果。副机上只回一个标记,界面据此显示"由主机检测"。
func (s *Server) handleReach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	if s.run.IsNode() {
		writeJSON(w, http.StatusOK, map[string]interface{}{"node": true})
		return
	}
	writeJSON(w, http.StatusOK, s.run.Reach().View())
}

// handleReachRun POST {nodeId}:手动检测一台;nodeId 为 0 或不传 = 全部服务器。立即返回,结果轮询 GET 看。
func (s *Server) handleReachRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var in struct {
		NodeId uint `json:"nodeId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		badRequest(w, err)
		return
	}
	var ids []uint
	if in.NodeId > 0 {
		ids = []uint{in.NodeId}
	}
	if err := s.run.Reach().Run(ids); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, reach.ErrBusy) {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r, "reach", "run", in.NodeId)
	writeJSON(w, http.StatusAccepted, map[string]string{"ok": "1"})
}
