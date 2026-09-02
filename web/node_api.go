package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/fangjunsheng555/m-ui/database/model"
)

// ---- 主机端:入口服务器管理 ----

type nodePayload struct {
	model.Node
	Token string `json:"token"`
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var nodes []model.Node
		s.db.Order("sort asc, id asc").Find(&nodes)
		statuses := s.run.Hub().Statuses()
		type row struct {
			model.Node
			HasToken bool        `json:"hasToken"`
			Status   interface{} `json:"status"`
		}
		out := make([]row, 0, len(nodes))
		for _, n := range nodes {
			rr := row{Node: n, HasToken: n.Token != ""}
			if st, ok := statuses[n.Id]; ok {
				rr.Status = st
			}
			out = append(out, rr)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"nodes": out, "revision": s.run.Hub().Revision()})
	case http.MethodPost:
		var p nodePayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.validateNode(&p); err != nil {
			badRequest(w, err)
			return
		}
		n := p.Node
		n.Id, n.IsLocal, n.Token = 0, false, strings.TrimSpace(p.Token)
		if err := s.db.Create(&n).Error; err != nil {
			badRequest(w, err)
			return
		}
		if !p.Enabled { // gorm default:true 会吞掉 Create 时的 false
			s.db.Model(&model.Node{}).Where("id = ?", n.Id).Update("enabled", false)
			n.Enabled = false
		}
		s.audit(r, "node", "create", n.Name)
		writeJSON(w, http.StatusOK, n)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

func (s *Server) validateNode(p *nodePayload) error {
	p.Name = strings.TrimSpace(p.Name)
	p.Domain = strings.TrimSpace(p.Domain)
	p.ApiUrl = strings.TrimSpace(p.ApiUrl)
	if p.Name == "" {
		return errors.New("名称不能为空")
	}
	if p.ApiUrl != "" && !strings.HasPrefix(p.ApiUrl, "http://") && !strings.HasPrefix(p.ApiUrl, "https://") {
		return errors.New("API 地址需以 http:// 或 https:// 开头,如 https://tw.example.com:2053/ad/")
	}
	dup := s.db.Model(&model.Node{}).Where("name = ?", p.Name)
	if p.Id != 0 {
		dup = dup.Where("id != ?", p.Id)
	}
	var n int64
	dup.Count(&n)
	if n > 0 {
		return errors.New("名称已存在")
	}
	return nil
}

func (s *Server) handleNodeItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.Split(strings.TrimPrefix(r.URL.Path, s.basePath()+"api/nodes/"), "/")
	id64, err := strconv.ParseUint(rest[0], 10, 64)
	if err != nil {
		badRequest(w, errors.New("id 无效"))
		return
	}
	id := uint(id64)
	var node model.Node
	if err := s.db.First(&node, id).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "服务器不存在"})
		return
	}
	if len(rest) == 2 {
		switch rest[1] {
		case "test":
			if node.IsLocal {
				writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "local": true, "coreRunning": s.run.CoreRunning(), "version": Version})
				return
			}
			out, err := s.run.Hub().Ping(node)
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			out["ok"] = true
			writeJSON(w, http.StatusOK, out)
		case "push":
			if node.IsLocal {
				badRequest(w, errors.New("本机无需推送"))
				return
			}
			if err := s.run.Hub().PushNow(node); err != nil {
				badRequest(w, err)
				return
			}
			s.audit(r, "node", "push", node.Name)
			writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
		default:
			http.NotFound(w, r)
		}
		return
	}
	switch r.Method {
	case http.MethodPut:
		var p nodePayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		p.Id = id
		if err := s.validateNode(&p); err != nil {
			badRequest(w, err)
			return
		}
		updates := map[string]interface{}{
			"name": p.Name, "domain": p.Domain, "api_url": p.ApiUrl, "insecure": p.Insecure, "enabled": p.Enabled, "sort": p.Sort,
		}
		if tok := strings.TrimSpace(p.Token); tok != "" {
			updates["token"] = tok // 留空保留原令牌
		}
		if err := s.db.Model(&model.Node{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			badRequest(w, err)
			return
		}
		if node.IsLocal && p.Domain != "" && p.Domain != s.setting("webDomain") {
			s.run.SetSetting("webDomain", p.Domain)
		}
		s.audit(r, "node", "update", p.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	case http.MethodDelete:
		if node.IsLocal {
			badRequest(w, errors.New("不能删除本机"))
			return
		}
		s.db.Delete(&model.Node{}, id)
		s.db.Where("node_id = ?", id).Delete(&model.TrafficCursor{})
		s.audit(r, "node", "delete", node.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}
