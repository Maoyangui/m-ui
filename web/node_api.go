package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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
		// 停用 / 删除时没送到的下线通知(主机一直在重试):停用的那台在它那一行标出来,已经删掉的单独列
		var pending []model.NodeDecommission
		s.db.Select("node_id, name, created_at, attempts, last_try, last_error").Order("node_id").Find(&pending)
		decom := map[uint]model.NodeDecommission{}
		for _, d := range pending {
			decom[d.NodeId] = d
		}
		type row struct {
			model.Node
			HasToken     bool                    `json:"hasToken"`
			Status       interface{}             `json:"status"`
			Decommission *model.NodeDecommission `json:"decommission,omitempty"`
		}
		out := make([]row, 0, len(nodes))
		for _, n := range nodes {
			rr := row{Node: n, HasToken: n.Token != ""}
			if st, ok := statuses[n.Id]; ok {
				rr.Status = st
			}
			if d, ok := decom[n.Id]; ok {
				rr.Decommission = &d
				delete(decom, n.Id)
			}
			out = append(out, rr)
		}
		gone := make([]model.NodeDecommission, 0, len(decom))
		for _, d := range pending {
			if _, ok := decom[d.NodeId]; ok {
				gone = append(gone, d)
			}
		}
		// 副机不生成快照,Hub 里的修订号是空的:用最近应用的那份(概况「与主机同步」卡、服务器页的修订号)
		rev := s.run.Hub().Revision()
		if rev == "" && s.run.IsNode() {
			rev = s.setting("hubRevision")
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"nodes": out, "revision": rev, "role": s.role(), "now": time.Now().Unix(), // 服务器时间:前端按它判断离线防抖,不受浏览器时钟偏差影响
			"masterId": s.settingInt("hubMasterId", 0), "appliedAt": s.setting("hubAppliedAt"),
			"decommissions": gone,
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
		n.PublicIP, n.PublicIP6 = "", "" // 探测值由服务器自己上报,不收客户端填的
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
	p.Addr = model.BareHost(p.Addr) // 填成 [v6] 的去掉方括号,分享链接里会自己加
	if p.Addr != "" && !model.ValidAddr(p.Addr) {
		return errors.New("连接地址只填 IP 或域名,不带端口、http:// 和空格")
	}
	p.AddrFamily = model.NormAddrFamily(p.AddrFamily)
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
			"addr": p.Addr, "addr_family": p.AddrFamily, "ratio": p.Ratio,
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
			if tok != node.Token {
				// 换了令牌 = 重新配对,多半是换了一台服务器接着用(域名、API 地址不变):旧机器的证书指纹作废,
				// 新机器第一次被认可(令牌对、返回 200)时重新记住,不用再去点「重置指纹」
				updates["cert_fp"] = ""
			}
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
		enabling := !node.IsLocal && !node.Enabled && p.Enabled
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.Node{}).Where("id = ?", id).Updates(updates).Error; err != nil {
				return err
			}
			if enabling { // 重新启用:还没送到的下线通知作废,定时同步推回完整快照
				return tx.Where("node_id = ?", id).Delete(&model.NodeDecommission{}).Error
			}
			if !node.IsLocal && !p.Enabled { // 停用着改了地址 / 令牌:还没送到的下线通知按新的地址、令牌接着重试
				rec := map[string]interface{}{"name": p.Name, "api_url": p.ApiUrl, "insecure": p.Insecure}
				if tok, ok := updates["token"]; ok {
					rec["token"] = tok
				}
				if _, reset := updates["cert_fp"]; reset {
					rec["cert_fp"] = ""
				}
				return tx.Model(&model.NodeDecommission{}).Where("node_id = ?", id).Updates(rec).Error
			}
			return nil
		}); err != nil {
			badRequest(w, err)
			return
		}
		if node.IsLocal && p.Domain != "" && p.Domain != s.setting("webDomain") {
			s.run.SetSetting("webDomain", p.Domain)
		}
		s.audit(r, "node", "update", p.Name)
		s.syncRelays() // 地址、域名变了,用这台服务器上的线路当上游的跟着改
		resp := map[string]interface{}{"ok": "1"}
		if disabling {
			if e := s.decommission(node, before); e != "" {
				resp["decommissionError"] = e
			}
			s.haForget(node, false)
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodDelete:
		if node.IsLocal {
			badRequest(w, errors.New("不能删除本机"))
			return
		}
		var relay model.Upstream
		if s.db.Where("from_node = ?", id).First(&relay).Error == nil {
			badRequest(w, fmt.Errorf("上游「%s」用着这台服务器上的线路,先删掉或改掉它", relay.Name))
			return
		}
		// 改库之前记下它此刻的配置,删完给它推一份空用户表(见 hub.Decommission)
		before, err := hub.BuildSnapshot(s.db, s.setting)
		if err != nil {
			badRequest(w, err)
			return
		}
		var disabled, revokedUsers, revokedResellers []string
		err = s.db.Transaction(func(tx *gorm.DB) error { // 服务器和引用它的一切一起删
			disabled = s.detachLinesFromNodeTx(tx, id) // 只部署在这台机器上的线路会被停用,不留悬空引用
			if err := tx.Delete(&model.Node{}, id).Error; err != nil {
				return err
			}
			// 用户 / 代理在某条线路上只收窄到这台机器的:收窄行一清就退回"该线路的全部服务器",范围静默扩大到
			// 别的机器。这条分配 / 授权改为撤掉,并列给管理员看(审计 M062)
			var err error
			if revokedUsers, revokedResellers, err = revokeSoleNodeScopes(tx, id); err != nil {
				return err
			}
			// 剩下的收窄行(还收窄到别的机器)只去掉这一台;它和游标、连通检测记录一起清
			for _, t := range []interface{}{&model.TrafficCursor{}, &model.UserLineNode{}, &model.ResellerLineNode{}, &model.ReachCheck{}} {
				if err := tx.Where("node_id = ?", id).Delete(t).Error; err != nil {
					return err
				}
			}
			// 记下的账本纪元随游标一起删:那台机器以后再加回来,只建基线、不重算历史(见 hub.MergeCounters)
			if err := tx.Where("key = ?", hub.LedgerEpochSetting(id)).Delete(&model.Setting{}).Error; err != nil {
				return err
			}
			// 顺手清掉老版本留下的孤儿游标与纪元(删副机时和在途并账竞态写回去的):同一台机器重新加回时不再被当成重复账本
			return hub.SweepOrphanLedgers(tx)
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
		resp := map[string]interface{}{"ok": "1", "disabledLines": disabled, "revokedUsers": revokedUsers, "revokedResellers": revokedResellers}
		if e := s.decommission(node, before); e != "" {
			resp["decommissionError"] = e
		}
		s.haForget(node, true)
		writeJSON(w, http.StatusOK, resp)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// haForget 停用 / 删除副机后:让它删掉手里的数据副本(删除时再让它离开集群:清纪元、删备用密钥),换集群密钥
// (它手里的旧密钥不能再拿来冒充同伴投票);它要是失联的原主机,不再等它回来。那台当时连不上也不要紧:
// 请求记进待办,主机每轮重试直到它确认。没进过纪元什么都不做。
func (s *Server) haForget(node model.Node, deleted bool) {
	h := s.haSvc()
	if h == nil || h.Epoch() == 0 || node.IsLocal {
		return
	}
	h.ForgetNode(node.Id)
	h.RotateClusterKey()
	h.QueueDrop(node, deleted)
}

// revokeSoleNodeScopes 删除服务器时:用户 / 代理在某条线路上只收窄到这台的,撤掉那条分配 / 授权,返回受影响的名字。
// 收窄里还有别的机器的不动(调用方随后只删指向这台的收窄行)。
func revokeSoleNodeScopes(tx *gorm.DB, nodeID uint) (users, resellers []string, err error) {
	var ul []model.UserLineNode
	if err = tx.Raw(`SELECT a.user_id, a.line_id, a.node_id FROM user_line_nodes a WHERE a.node_id = ? AND NOT EXISTS
		(SELECT 1 FROM user_line_nodes b WHERE b.user_id = a.user_id AND b.line_id = a.line_id AND b.node_id <> a.node_id)`, nodeID).Scan(&ul).Error; err != nil {
		return
	}
	var uids []uint
	for _, x := range ul {
		if err = tx.Where("user_id = ? AND line_id = ?", x.UserId, x.LineId).Delete(&model.UserLine{}).Error; err != nil {
			return
		}
		uids = append(uids, x.UserId)
	}
	var rl []model.ResellerLineNode
	if err = tx.Raw(`SELECT a.reseller_id, a.line_id, a.node_id FROM reseller_line_nodes a WHERE a.node_id = ? AND NOT EXISTS
		(SELECT 1 FROM reseller_line_nodes b WHERE b.reseller_id = a.reseller_id AND b.line_id = a.line_id AND b.node_id <> a.node_id)`, nodeID).Scan(&rl).Error; err != nil {
		return
	}
	var rids []uint
	for _, x := range rl {
		if err = tx.Where("reseller_id = ? AND line_id = ?", x.ResellerId, x.LineId).Delete(&model.ResellerLine{}).Error; err != nil {
			return
		}
		rids = append(rids, x.ResellerId)
	}
	if len(uids) > 0 {
		if err = tx.Model(&model.User{}).Where("id IN ?", uids).Order("name").Pluck("name", &users).Error; err != nil {
			return
		}
	}
	if len(rids) > 0 {
		err = tx.Model(&model.Reseller{}).Where("id IN ?", rids).Order("name").Pluck("name", &resellers).Error
	}
	return
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
