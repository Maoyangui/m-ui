package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"

	"gorm.io/gorm"
)

// 线路批量设置:勾选若干线路后一次性 启用 / 停用 / 换上游 / 改部署服务器。
//
// 语义和逐条编辑完全一致,只是把校验、落库、干跑、重载合成一批:
//   - 任何一条不通过(上游不存在、改部署后端口在某台服务器上撞了)整批不保存;
//   - 全部通过后在一个事务里写入并整体干跑一次 sing-box 配置;
//   - 数据面只重载一次,而不是每条线路一次。

type lineBatchReq struct {
	Ids        []uint  `json:"ids"`
	Action     string  `json:"action"`     // enable | disable | upstream | nodes
	UpstreamId *uint   `json:"upstreamId"` // action=upstream:0 = 直连
	NodeIds    *[]uint `json:"nodeIds"`    // action=nodes:空 = 全部服务器
}

func (s *Server) handleLineBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req lineBatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	if len(req.Ids) == 0 {
		badRequest(w, errors.New("没有选中线路"))
		return
	}
	var lines []model.Line
	s.db.Where("id IN ?", req.Ids).Order("sort asc, id asc").Find(&lines)
	if len(lines) != len(uniqueIDs(req.Ids)) {
		badRequest(w, errors.New("有线路不存在,请刷新后重试"))
		return
	}

	var cols []string
	switch req.Action {
	case "enable", "disable":
		for i := range lines {
			lines[i].Enabled = req.Action == "enable"
		}
		cols = []string{"enabled"}
	case "upstream":
		if req.UpstreamId == nil {
			badRequest(w, errors.New("缺少 upstreamId"))
			return
		}
		if *req.UpstreamId > 0 {
			var n int64
			s.db.Model(&model.Upstream{}).Where("id = ?", *req.UpstreamId).Count(&n)
			if n == 0 {
				badRequest(w, errors.New("上游不存在"))
				return
			}
		}
		for i := range lines {
			lines[i].UpstreamId = *req.UpstreamId
		}
		cols = []string{"upstream_id"}
	case "nodes":
		if req.NodeIds == nil {
			badRequest(w, errors.New("缺少 nodeIds"))
			return
		}
		var raw json.RawMessage
		if len(*req.NodeIds) > 0 {
			var nodes []model.Node
			s.db.Where("id IN ?", *req.NodeIds).Find(&nodes)
			if len(nodes) != len(uniqueIDs(*req.NodeIds)) {
				badRequest(w, errors.New("有服务器不存在,请刷新后重试"))
				return
			}
			raw, _ = json.Marshal(uniqueIDs(*req.NodeIds))
		}
		for i := range lines {
			lines[i].NodeIds = raw
		}
		// 端口只在同一台服务器上才会撞:按改完之后的部署范围,和批外的线路、批内的彼此都比一遍
		if err := s.checkBatchPorts(lines); err != nil {
			badRequest(w, err)
			return
		}
		cols = []string{"node_ids"}
	default:
		badRequest(w, errors.New("未知操作: "+req.Action))
		return
	}

	var nodeCert render.NodeCert
	if s.run != nil { // 事务里不能再走连接池,先取好;测试里没有数据面
		nodeCert = s.run.NodeCert()
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		for _, l := range lines {
			if err := tx.Model(&model.Line{}).Where("id = ?", l.Id).Select(cols).Updates(l).Error; err != nil {
				return err
			}
		}
		return validateFullConfig(tx, nodeCert)
	})
	if err != nil {
		badRequest(w, err)
		return
	}
	names := make([]string, 0, len(lines))
	for _, l := range lines {
		names = append(names, l.Name)
	}
	s.audit(r, "line", "batch:"+req.Action, names)
	s.reloadAll("批量设置线路 " + req.Action + " ×" + strconv.Itoa(len(lines)))
	writeJSON(w, http.StatusOK, map[string]interface{}{"affected": len(lines)})
}

// checkBatchPorts 改完部署范围后,同端口的线路在任何一台服务器上都不能有两条。
// 批内线路按新范围比,批外线路按库里的范围比。
func (s *Server) checkBatchPorts(batch []model.Line) error {
	inBatch := map[uint]bool{}
	for _, l := range batch {
		inBatch[l.Id] = true
	}
	var nodes []model.Node
	s.db.Select("id, name").Order("sort asc, id asc").Find(&nodes)
	var others []model.Line
	s.db.Select("id, name, port, node_ids").Find(&others)
	for i, a := range batch {
		for _, o := range others {
			if o.Port != a.Port || inBatch[o.Id] {
				continue
			}
			if where := linesSharedNode(a, o, nodes); where != "" {
				return fmt.Errorf("线路「%s」改到该范围后端口 %d 会和「%s」在 %s 上冲突", a.Name, a.Port, o.Name, where)
			}
		}
		for _, b := range batch[i+1:] {
			if b.Port != a.Port {
				continue
			}
			if where := linesSharedNode(a, b, nodes); where != "" {
				return fmt.Errorf("线路「%s」和「%s」端口都是 %d,不能同时部署到 %s", a.Name, b.Name, a.Port, where)
			}
		}
	}
	return nil
}

func uniqueIDs(ids []uint) []uint {
	seen := map[uint]bool{}
	out := make([]uint, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
