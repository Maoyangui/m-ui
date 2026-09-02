package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/hub"
	"github.com/fangjunsheng555/m-ui/logger"
)

// ---- 副机端:供主机调用的接口(令牌鉴权,不走会话) ----

func (s *Server) nodeToken() string {
	tok := s.setting("nodeToken")
	if tok == "" {
		b := make([]byte, 24)
		rand.Read(b)
		tok = hex.EncodeToString(b)
		s.run.SetSetting("nodeToken", tok)
	}
	return tok
}

func (s *Server) agentAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(s.setting("nodeMode"), "true") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "本机不是副机(设置里未开启副服务器模式)"})
			return
		}
		tok := r.Header.Get("X-Agent-Token")
		want := s.nodeToken()
		if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(want)) != 1 {
			time.Sleep(200 * time.Millisecond)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "令牌错误"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, s.basePath()+"api/agent/")
	switch action {
	case "info": // 会话鉴权:副机面板展示配对信息
		s.auth(s.handleAgentInfo)(w, r)
	case "rotate":
		s.auth(s.handleAgentRotate)(w, r)
	case "ping":
		s.agentAuth(s.handleAgentPing)(w, r)
	case "apply":
		s.agentAuth(s.handleAgentApply)(w, r)
	case "report":
		s.agentAuth(s.handleAgentReport)(w, r)
	case "external-ips":
		s.agentAuth(s.handleAgentExternalIPs)(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleAgentInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"isNode": strings.EqualFold(s.setting("nodeMode"), "true"),
		"token": s.nodeToken(), "revision": s.setting("hubRevision"), "appliedAt": s.setting("hubAppliedAt"),
		"apiUrl": s.selfApiURL(),
	})
}

func (s *Server) handleAgentRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	s.run.SetSetting("nodeToken", "")
	s.audit(r, "agent", "rotate-token", nil)
	writeJSON(w, http.StatusOK, map[string]string{"token": s.nodeToken()})
}

// selfApiURL 猜测本机面板对外地址,便于复制到主机。
func (s *Server) selfApiURL() string {
	scheme := "http"
	if s.setting("webCertFile") != "" {
		scheme = "https"
	}
	host := s.setting("webDomain")
	if host == "" {
		host = "<本机IP>"
	}
	return scheme + "://" + host + ":" + s.setting("webPort") + s.basePath()
}

func (s *Server) handleAgentPing(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"version": Version, "role": s.role(), "hostname": host,
		"coreRunning": s.run.CoreRunning(), "uptime": s.run.Uptime(),
		"revision": s.setting("hubRevision"), "certDays": s.run.CertInfo().DaysLeft,
	})
}

func (s *Server) handleAgentApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var snap hub.Snapshot
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&snap); err != nil {
		badRequest(w, err)
		return
	}
	if snap.Revision == "" {
		badRequest(w, errors.New("快照缺少修订号"))
		return
	}
	linesChanged, upsChanged, err := hub.ApplySnapshot(s.db, snap)
	if err != nil {
		badRequest(w, err)
		return
	}
	switch {
	case linesChanged:
		s.reloadAll("主机下发配置 " + snap.Revision)
	case upsChanged:
		s.reloadUpstreams("主机下发上游 " + snap.Revision)
	default:
		s.reloadUsers("主机下发用户 " + snap.Revision)
	}
	logger.Info("已应用主机配置 ", snap.Revision, "(线路变化: ", linesChanged, ",上游变化: ", upsChanged, ")")
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1", "revision": snap.Revision})
}

func (s *Server) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	rep := hub.Report{
		Version: Version, Hostname: host, CoreRunning: s.run.CoreRunning(), Uptime: s.run.Uptime(),
		Revision: s.setting("hubRevision"), Onlines: map[string][]string{}, CertDays: s.run.CertInfo().DaysLeft,
		PublicIP: s.setting("publicIp"),
	}
	s.db.Find(&rep.Counters)
	o := s.run.Onlines()
	for _, u := range o.Users {
		rep.Onlines[u] = s.run.OnlineIPs(u)
	}
	// 设备数限制的用户即使本周期无流量,其仍在线的 IP 也要上报
	var limited []model.User
	s.db.Where("device_limit > 0").Find(&limited)
	for _, u := range limited {
		if _, ok := rep.Onlines[u.Name]; !ok {
			if ips := s.run.OnlineIPs(u.Name); len(ips) > 0 {
				rep.Onlines[u.Name] = ips
			}
		}
	}
	rep.OnlineLines = o.Lines
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleAgentExternalIPs(w http.ResponseWriter, r *http.Request) {
	var m map[string][]string
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		badRequest(w, err)
		return
	}
	s.run.SetExternalIPs(m)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}
