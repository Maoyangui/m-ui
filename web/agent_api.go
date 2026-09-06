package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/selfupdate"
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
	action := strings.TrimPrefix(r.URL.Path, innerBase+"api/agent/")
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
	case "upstream-test": // 主机让本机立刻测一条上游(面板的"测试"按钮派发过来)
		s.agentAuth(s.handleAgentUpstreamTest)(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleAgentInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"isNode": strings.EqualFold(s.setting("nodeMode"), "true"),
		"token":  s.nodeToken(), "revision": s.setting("hubRevision"), "appliedAt": s.setting("hubAppliedAt"),
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
		host = s.run.PublicHost()
	}
	if host == "" {
		host = "<本机IP>"
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, host, s.settingInt("webPort", 2053), s.basePath())
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
	if snap.MinNode != "" && selfupdate.Newer(snap.MinNode, Version) {
		// 主机的快照里有本机不认识的字段(比如停用原因、代理额度标志):应用了也是错的,直接拒绝并说清楚
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("副机版本 v%s 低于主机要求的 v%s,请先升级这台副机", Version, snap.MinNode)})
		return
	}
	revoked := hub.RevokedShares(s.db, snap)
	rotated := hub.RotatedUsers(s.db, snap)
	linesChanged, upsChanged, err := hub.ApplySnapshot(s.db, snap)
	if err != nil {
		badRequest(w, err)
		return
	}
	// ================= D. 本机账本只增不删:主机已经不认识的用户,它的计数器没人再回收,清掉 =================
	// 主机那边的游标还留着;同名用户以后再建,计数从 0 起小于游标,会被当成回绕重认,不会多算
	s.db.Exec("DELETE FROM agent_counters WHERE user_name NOT IN (SELECT name FROM users)")
	switch {
	case linesChanged:
		s.reloadAll("主机下发配置 " + snap.Revision)
	case upsChanged:
		s.reloadUpstreams("主机下发上游 " + snap.Revision)
	default:
		s.reloadUsers("主机下发用户 " + snap.Revision)
	}
	if len(revoked) > 0 || len(rotated) > 0 {
		go func() { // 先把凭据热更新掉再断线,免得借用者 / 旧凭据在空档里重连
			if err := s.run.ReloadUsers(); err != nil {
				logger.Warning("撤下旧凭据失败: ", err)
				return
			}
			for _, name := range revoked {
				if n := s.run.KickShare(name); n > 0 {
					logger.Info("临时共享已取消,断开 ", name, " 的 ", n, " 条连接")
				}
			}
			for _, name := range rotated {
				if n := s.run.KickUser(name); n > 0 {
					logger.Info("订阅链接已重置,断开 ", name, " 旧凭据上的 ", n, " 条连接")
				}
			}
		}()
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
		Conns:    s.recentConns(50),
	}
	rep.OnlineLinesByIP = s.run.OnlineIPLines()
	rep.Groups = s.run.GroupState()
	if rl := s.run.ReloadStatus(); rl.At > 0 {
		rep.Reload = &hub.ReloadState{At: rl.At, Op: rl.Op, OK: rl.OK, Error: rl.Error}
	}
	for _, h := range s.run.UpstreamHealthLocal() { // 本机线路用到的那些上游,量出来的结果交给主机汇总
		rep.Upstreams = append(rep.Upstreams, hub.UpstreamHealth{Id: h.Id, Name: h.Name, OK: h.OK,
			DelayMs: h.DelayMs, Method: h.Method, Error: h.Error, CheckedAt: h.CheckedAt, Fails: h.Fails})
	}
	s.db.Find(&rep.Counters)
	o := s.run.Onlines()
	allIPs := s.run.OnlineIPsAll() // 一次锁拿全量,不按用户逐个抢数据面的锁
	for _, u := range o.Users {
		rep.Onlines[u] = allIPs[u]
	}
	// 本周期没流量但仍在线(空闲窗口内)的 IP 也要上报:用户自己的设备数限制、代理设备池都要靠它跨机并集判定
	for u, ips := range allIPs {
		if _, ok := rep.Onlines[u]; !ok && len(ips) > 0 {
			rep.Onlines[u] = ips
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

// handleAgentUpstreamTest 主机派发过来的单条上游测试:在本机实测并返回结果。
// 上游通不通要在真正跑这条线路的机器上量,面板的"测试"按钮因此要派发到各机而不是只测主机。
func (s *Server) handleAgentUpstreamTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var body struct {
		Id uint `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	var up model.Upstream
	if err := s.db.First(&up, body.Id).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "上游不存在"})
		return
	}
	ok, ms, meth, errStr := s.run.CheckUpstream(up)
	writeJSON(w, http.StatusOK, hub.UpstreamHealth{Id: up.Id, Name: up.Name, OK: ok, DelayMs: ms,
		Method: meth, Error: errStr, CheckedAt: time.Now().Unix()})
}
