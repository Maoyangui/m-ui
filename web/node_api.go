package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
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
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"nodes": out, "revision": s.run.Hub().Revision(), "role": s.role(),
			"masterId": s.settingInt("hubMasterId", 0), "appliedAt": s.setting("hubAppliedAt"),
		})
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
	p.Addr = strings.TrimSpace(p.Addr)
	if p.Ratio < 0 || p.Ratio > 100 {
		return errors.New("倍率需在 0–100 之间(1 = 原样)")
	}
	if p.Ratio == 0 {
		p.Ratio = 1
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
	rest := strings.Split(strings.TrimPrefix(r.URL.Path, innerBase+"api/nodes/"), "/")
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
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
				return
			}
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
		case "resetcert":
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
				return
			}
			// 忘掉记住的证书指纹:副机重签了证书之后用,下次连接重新记住新的
			if err := s.db.Model(&model.Node{}).Where("id = ?", id).Update("cert_fp", "").Error; err != nil {
				badRequest(w, err)
				return
			}
			s.audit(r, "node", "resetcert", node.Name)
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
			"addr": p.Addr, "ratio": p.Ratio,
		}
		if p.ApiUrl != node.ApiUrl || p.Insecure != node.Insecure {
			updates["cert_fp"] = "" // 地址或校验方式变了,记住的证书指纹作废,下次连接重新记
		}
		if node.IsLocal { // 本机:API 地址/令牌/校验无意义,保持原值
			delete(updates, "api_url")
			delete(updates, "insecure")
			updates["enabled"] = true
		}
		if tok := strings.TrimSpace(p.Token); tok != "" {
			updates["token"] = tok // 留空保留原令牌
		}
		// 停用副机:改库之前记下它此刻的配置,改完给它推一份空用户表(见 hub.Decommission)
		disabling := !node.IsLocal && node.Enabled && !p.Enabled
		var before hub.Snapshot
		if disabling {
			if before, err = hub.BuildSnapshot(s.db, s.setting); err != nil {
				badRequest(w, err)
				return
			}
		}
		if err := s.db.Model(&model.Node{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			badRequest(w, err)
			return
		}
		if node.IsLocal && p.Domain != "" && p.Domain != s.setting("webDomain") {
			s.run.SetSetting("webDomain", p.Domain)
		}
		s.audit(r, "node", "update", p.Name)
		resp := map[string]interface{}{"ok": "1"}
		if disabling {
			if e := s.decommission(node, before); e != "" {
				resp["decommissionError"] = e
			}
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodDelete:
		if node.IsLocal {
			badRequest(w, errors.New("不能删除本机"))
			return
		}
		// 改库之前记下它此刻的配置,删完给它推一份空用户表(见 hub.Decommission)
		before, err := hub.BuildSnapshot(s.db, s.setting)
		if err != nil {
			badRequest(w, err)
			return
		}
		var disabled []string
		err = s.db.Transaction(func(tx *gorm.DB) error { // 服务器和引用它的一切一起删
			disabled = s.detachLinesFromNodeTx(tx, id) // 只部署在这台机器上的线路会被停用,不留悬空引用
			if err := tx.Delete(&model.Node{}, id).Error; err != nil {
				return err
			}
			// 用户 / 代理"只要这台机器上的入口"的收窄行也要清掉:留着的话,这些人这条线路上
			// 一台机器都匹配不到,订阅里会悄无声息地少节点。清掉之后按"没有收窄 = 全部服务器"处理。
			for _, t := range []interface{}{&model.TrafficCursor{}, &model.UserLineNode{}, &model.ResellerLineNode{}, &model.ReachCheck{}} {
				if err := tx.Where("node_id = ?", id).Delete(t).Error; err != nil {
					return err
				}
			}
			// 记下的账本纪元随游标一起删:那台机器以后再加回来,只建基线、不重算历史(见 hub.ledgerBaseline)
			return tx.Where("key = ?", hub.LedgerEpochSetting(id)).Delete(&model.Setting{}).Error
		})
		if err != nil {
			badRequest(w, err)
			return
		}
		if s.run != nil { // 测试里没有数据面
			s.run.Reach().Forget(id)
		}
		s.audit(r, "node", "delete", node.Name)
		if len(disabled) > 0 {
			s.reloadAll("删除服务器 " + node.Name)
		}
		resp := map[string]interface{}{"ok": "1", "disabledLines": disabled}
		if e := s.decommission(node, before); e != "" {
			resp["decommissionError"] = e
		}
		writeJSON(w, http.StatusOK, resp)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// decommission 停用 / 删除副机后给它推空用户表,那台立刻停止为任何人服务。没推到(失联、没配地址)返回原因:
// 那台仍按旧配置为所有人服务,得让管理员知道、去那台机器上处理。
func (s *Server) decommission(node model.Node, before hub.Snapshot) string {
	if s.run == nil || node.IsLocal { // 测试里没有数据面
		return ""
	}
	if err := s.run.Hub().Decommission(node, before); err != nil {
		logger.Warning("副机 ", node.Name, " 没收到下线通知: ", err)
		return err.Error()
	}
	return ""
}

// detachLinesFromNode 把线路的"部署到服务器"里那台被删的机器摘掉。
// 摘完没有服务器可去的线路会被停用(空列表在渲染时等于"所有服务器",
// 直接留空会让线路悄悄跑到全部机器上,不是管理员的本意),返回被停用的线路名。
func (s *Server) detachLinesFromNode(nodeID uint) []string {
	return s.detachLinesFromNodeTx(s.db, nodeID)
}

func (s *Server) detachLinesFromNodeTx(db *gorm.DB, nodeID uint) []string {
	var lines []model.Line
	db.Where("node_ids IS NOT NULL AND node_ids <> ''").Find(&lines)
	var disabled []string
	for _, l := range lines {
		var ids []uint
		if json.Unmarshal(l.NodeIds, &ids) != nil {
			continue
		}
		kept := make([]uint, 0, len(ids))
		for _, id := range ids {
			if id != nodeID {
				kept = append(kept, id)
			}
		}
		if len(kept) == len(ids) {
			continue // 与这台机器无关
		}
		upd := map[string]interface{}{}
		if len(kept) == 0 {
			upd["node_ids"] = nil
			upd["enabled"] = false
			disabled = append(disabled, l.Name)
		} else {
			b, _ := json.Marshal(kept)
			upd["node_ids"] = json.RawMessage(b)
		}
		db.Model(&model.Line{}).Where("id = ?", l.Id).Updates(upd)
	}
	return disabled
}
