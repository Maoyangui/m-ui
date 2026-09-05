package web

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
)

// 线路分配的"线路 × 服务器"表示。
//
// 一条线路部署在 A、B 两台服务器上时,对用户来说其实是两个入口;分配可以细到"只给 A 上的那个"。
// 接口里统一用 LineRef{lineId, nodeIds}(nodeIds 空 = 该线路的全部服务器,包括以后新加的);
// 库里是 user_lines(拿到这条线路)+ user_line_nodes(收窄到哪些服务器,没有行 = 全部)两张表,
// 代理授权同理(reseller_lines + reseller_line_nodes),套餐存在 LineIds + LineNodes 两个 JSON 字段里。
// 老接口只传 lineIds 的,一律视为全部服务器,行为和以前完全一样。

// lineRefsOf 取请求里的分配:给了 lineRefs 就用它,否则 lineIds → 全部服务器。
func lineRefsOf(ids []uint, refs []model.LineRef) []model.LineRef {
	if refs != nil {
		return refs
	}
	out := make([]model.LineRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, model.LineRef{LineId: id})
	}
	return out
}

// refIDs 只要线路 id(老字段 lineIds 仍然返回,兼容外部程序)。
func refIDs(refs []model.LineRef) []uint {
	out := make([]uint, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.LineId)
	}
	return out
}

// lineNodeIDs 该线路部署到的服务器 id(NodeIds 空 = 全部服务器)。
func lineNodeIDs(line model.Line, nodes []model.Node) []uint {
	var out []uint
	for _, n := range nodes {
		if render.LineOnNode(line, n.Id) {
			out = append(out, n.Id)
		}
	}
	return out
}

// normalizeRefs 整理一份分配:同一线路合并、服务器去重并只留该线路真的部署到的、
// 覆盖了全部服务器就收成"全部"(以后新加服务器自动包含);不存在的线路原样保留交给校验点名。
func (s *Server) normalizeRefs(refs []model.LineRef) []model.LineRef {
	if len(refs) == 0 {
		return nil
	}
	var lines []model.Line
	s.db.Select("id, node_ids").Find(&lines)
	var nodes []model.Node
	s.db.Select("id").Order("sort asc, id asc").Find(&nodes)
	lineBy := map[uint]model.Line{}
	for _, l := range lines {
		lineBy[l.Id] = l
	}
	type acc struct {
		all bool
		set map[uint]bool
	}
	merged := map[uint]*acc{}
	var order []uint
	for _, r := range refs {
		a := merged[r.LineId]
		if a == nil {
			a = &acc{set: map[uint]bool{}}
			merged[r.LineId] = a
			order = append(order, r.LineId)
		}
		if len(r.NodeIds) == 0 {
			a.all = true // 任一处写了"全部",整体就是全部
			continue
		}
		for _, n := range r.NodeIds {
			a.set[n] = true
		}
	}
	out := make([]model.LineRef, 0, len(order))
	for _, lid := range order {
		a := merged[lid]
		ref := model.LineRef{LineId: lid}
		if !a.all && len(a.set) > 0 {
			line, ok := lineBy[lid]
			var keep []uint
			if !ok { // 不存在的线路:原样保留,让校验去报
				for n := range a.set {
					keep = append(keep, n)
				}
				sort.Slice(keep, func(i, j int) bool { return keep[i] < keep[j] })
				ref.NodeIds = keep
			} else {
				deployed := lineNodeIDs(line, nodes)
				for _, n := range deployed {
					if a.set[n] {
						keep = append(keep, n)
					}
				}
				// 勾满了 = 全部;一台都对不上(勾的服务器没部署这条线路)也按全部,不让用户拿到空入口
				if len(keep) > 0 && len(keep) < len(deployed) {
					ref.NodeIds = keep
				}
			}
		}
		out = append(out, ref)
	}
	return out
}

// setUserLineRefs 落库用户的分配(两张表整体替换)。
func (s *Server) setUserLineRefs(userID uint, refs []model.LineRef) {
	refs = s.normalizeRefs(refs)
	s.db.Where("user_id = ?", userID).Delete(&model.UserLine{})
	s.db.Where("user_id = ?", userID).Delete(&model.UserLineNode{})
	for _, r := range refs {
		s.db.Create(&model.UserLine{UserId: userID, LineId: r.LineId})
		for _, n := range r.NodeIds {
			s.db.Create(&model.UserLineNode{UserId: userID, LineId: r.LineId, NodeId: n})
		}
	}
}

// userLineRefMap 整表一次查出:用户 → 分配(含服务器范围)。
func (s *Server) userLineRefMap() map[uint][]model.LineRef {
	var links []model.UserLine
	s.db.Order("user_id asc, line_id asc").Find(&links)
	var scopes []model.UserLineNode
	s.db.Order("user_id asc, line_id asc, node_id asc").Find(&scopes)
	type key struct{ u, l uint }
	nodesBy := map[key][]uint{}
	for _, sc := range scopes {
		k := key{sc.UserId, sc.LineId}
		nodesBy[k] = append(nodesBy[k], sc.NodeId)
	}
	out := map[uint][]model.LineRef{}
	for _, l := range links {
		out[l.UserId] = append(out[l.UserId], model.LineRef{LineId: l.LineId, NodeIds: nodesBy[key{l.UserId, l.LineId}]})
	}
	return out
}

// userLineRefs 单个用户的分配。
func (s *Server) userLineRefs(userID uint) []model.LineRef {
	var links []model.UserLine
	s.db.Where("user_id = ?", userID).Order("line_id asc").Find(&links)
	var scopes []model.UserLineNode
	s.db.Where("user_id = ?", userID).Order("line_id asc, node_id asc").Find(&scopes)
	nodesBy := map[uint][]uint{}
	for _, sc := range scopes {
		nodesBy[sc.LineId] = append(nodesBy[sc.LineId], sc.NodeId)
	}
	out := make([]model.LineRef, 0, len(links))
	for _, l := range links {
		out = append(out, model.LineRef{LineId: l.LineId, NodeIds: nodesBy[l.LineId]})
	}
	return out
}

// setResellerLineRefs / resellerLineRefs 代理授权的两张表。
func (s *Server) setResellerLineRefs(id uint, refs []model.LineRef) {
	refs = s.normalizeRefs(refs)
	s.db.Where("reseller_id = ?", id).Delete(&model.ResellerLine{})
	s.db.Where("reseller_id = ?", id).Delete(&model.ResellerLineNode{})
	for _, r := range refs {
		s.db.Create(&model.ResellerLine{ResellerId: id, LineId: r.LineId})
		for _, n := range r.NodeIds {
			s.db.Create(&model.ResellerLineNode{ResellerId: id, LineId: r.LineId, NodeId: n})
		}
	}
}

func (s *Server) resellerLineRefs(id uint) []model.LineRef {
	var links []model.ResellerLine
	s.db.Where("reseller_id = ?", id).Order("line_id asc").Find(&links)
	var scopes []model.ResellerLineNode
	s.db.Where("reseller_id = ?", id).Order("line_id asc, node_id asc").Find(&scopes)
	nodesBy := map[uint][]uint{}
	for _, sc := range scopes {
		nodesBy[sc.LineId] = append(nodesBy[sc.LineId], sc.NodeId)
	}
	out := make([]model.LineRef, 0, len(links))
	for _, l := range links {
		out = append(out, model.LineRef{LineId: l.LineId, NodeIds: nodesBy[l.LineId]})
	}
	return out
}

// checkRefsGranted 用户的分配必须落在代理的授权范围内:线路要授权过;授权收窄到了具体服务器时,
// 用户只能拿其中的服务器,不能拿"全部"。
func (s *Server) checkRefsGranted(rid uint, refs []model.LineRef) error {
	granted := map[uint]map[uint]bool{} // nil 值 = 该线路全部服务器
	for _, g := range s.resellerLineRefs(rid) {
		if len(g.NodeIds) == 0 {
			granted[g.LineId] = nil
			continue
		}
		set := map[uint]bool{}
		for _, n := range g.NodeIds {
			set[n] = true
		}
		granted[g.LineId] = set
	}
	for _, r := range s.normalizeRefs(refs) {
		set, ok := granted[r.LineId]
		if !ok {
			return errors.New("含未授权的线路")
		}
		if set == nil {
			continue
		}
		if len(r.NodeIds) == 0 {
			return errors.New("线路 #" + strconv.Itoa(int(r.LineId)) + " 只授权了部分服务器,不能分配全部")
		}
		for _, n := range r.NodeIds {
			if !set[n] {
				return errors.New("含未授权的服务器")
			}
		}
	}
	return nil
}

// planRefs 套餐指定的分配(LineIds + LineNodes);套餐不改线路时返回 nil。
func planRefs(p model.Plan) []model.LineRef {
	var ids []uint
	if len(p.LineIds) > 0 {
		_ = json.Unmarshal(p.LineIds, &ids)
	}
	if len(ids) == 0 {
		return nil
	}
	nodes := map[string][]uint{}
	if len(p.LineNodes) > 0 {
		_ = json.Unmarshal(p.LineNodes, &nodes)
	}
	out := make([]model.LineRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, model.LineRef{LineId: id, NodeIds: nodes[strconv.Itoa(int(id))]})
	}
	return out
}

// validatePlanRefs 套餐里的服务器范围只能写给套餐真的带的线路。
func validatePlanRefs(p *model.Plan) error {
	if len(p.LineNodes) == 0 {
		return nil
	}
	nodes := map[string][]uint{}
	if err := json.Unmarshal(p.LineNodes, &nodes); err != nil {
		return errors.New("线路服务器范围格式错误")
	}
	var ids []uint
	if len(p.LineIds) > 0 {
		_ = json.Unmarshal(p.LineIds, &ids)
	}
	has := map[string]bool{}
	for _, id := range ids {
		has[strconv.Itoa(int(id))] = true
	}
	for k := range nodes {
		if !has[k] {
			return errors.New("服务器范围里有套餐没带的线路 #" + k)
		}
	}
	return nil
}

// lineRefsJSON 把分配写回套餐的两个字段。
func lineRefsJSON(refs []model.LineRef) (lineIds, lineNodes json.RawMessage) {
	if len(refs) == 0 {
		return nil, nil
	}
	ids := refIDs(refs)
	nodes := map[string][]uint{}
	for _, r := range refs {
		if len(r.NodeIds) > 0 {
			nodes[strconv.Itoa(int(r.LineId))] = r.NodeIds
		}
	}
	lineIds, _ = json.Marshal(ids)
	if len(nodes) > 0 {
		lineNodes, _ = json.Marshal(nodes)
	}
	return lineIds, lineNodes
}
