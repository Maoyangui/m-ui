package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
)

// 概况用的几处小补充(都是可选字段,前端没有它们时照旧显示):
// 副机收主机推来的配置失败的原因、代理名下用户 → 代理名(在线名单与流量排行的链接)、副机本机部署的线路数。

// applyRecorder 记下 apply 接口回给主机的状态码与错误原文(只缓存错误回应的正文,很短)。
type applyRecorder struct {
	http.ResponseWriter
	code int
	body bytes.Buffer
}

func (a *applyRecorder) WriteHeader(code int) {
	a.code = code
	a.ResponseWriter.WriteHeader(code)
}

func (a *applyRecorder) Write(b []byte) (int, error) {
	if a.code == 0 {
		a.code = http.StatusOK
	}
	if a.code >= 400 && a.body.Len() < 4096 {
		a.body.Write(b)
	}
	return a.ResponseWriter.Write(b)
}

// applyState 副机最近一次没应用成主机推来的配置的原因与时间(之后应用成功就清掉)。
// 版本低于主机要求被拒收、数据面应用失败都算:这时主机照样在联系本机,不记下来概况会显示成"一切正常"。
type applyState struct {
	mu  sync.Mutex
	err string
	at  int64
}

func (a *applyState) note(code int, body []byte) {
	if code == 0 { // 没回应(主机已经放弃了这次请求):不算
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if code < 400 {
		a.err, a.at = "", 0
		return
	}
	var e struct {
		Error string `json:"error"`
	}
	msg := http.StatusText(code)
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	a.err, a.at = msg, time.Now().Unix()
}

func (a *applyState) get() (string, int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err, a.at
}

// handleAgentApplyRecorded 照常处理主机的推送,并记下这次的成败(概况「与主机同步」卡与「需要处理」用)。
func (s *Server) handleAgentApplyRecorded(w http.ResponseWriter, r *http.Request) {
	rec := &applyRecorder{ResponseWriter: w}
	s.handleAgentApply(rec, r)
	s.applyRes.note(rec.code, rec.body.Bytes())
}

// resellerOwners 代理名下用户 → 代理名。60 秒缓存:在线名单每 5 秒一次,不随它查库。
func (s *Server) resellerOwners() map[string]string {
	s.ownersMu.Lock()
	defer s.ownersMu.Unlock()
	if s.owners != nil && time.Since(s.ownersAt) < 60*time.Second {
		return s.owners
	}
	type row struct{ User, Reseller string }
	var rows []row
	s.db.Raw(`SELECT u.name AS user, r.name AS reseller FROM users u JOIN resellers r ON r.id = u.reseller_id WHERE u.reseller_id > 0`).Scan(&rows)
	m := make(map[string]string, len(rows))
	for _, x := range rows {
		m[x.User] = x.Reseller
	}
	s.owners, s.ownersAt = m, time.Now()
	return m
}

// onlineOwners 在线名单里属于代理的那些人 → 代理名(主面板的用户页只列直属用户,概况据此把链接指到代理)。
func (s *Server) onlineOwners(users []string) map[string]string {
	all := s.resellerOwners()
	out := map[string]string{}
	for _, u := range users {
		if r, ok := all[u]; ok {
			out[u] = r
		}
	}
	return out
}

// localLineCounts 副机本机部署了几条线路(启用的几条)。副机库里有主机下发的全部线路,只渲染部署到本机的。
func (s *Server) localLineCounts() (total, enabled int) {
	var lines []model.Line
	s.db.Select("id, node_ids, enabled").Find(&lines)
	self := s.run.LocalNodeId()
	for _, l := range lines {
		if render.LineOnNode(l, self) {
			total++
			if l.Enabled {
				enabled++
			}
		}
	}
	return total, enabled
}
