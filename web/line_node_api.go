package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// lineNodeReq PUT /lines/{id}/node 的请求:改这条线路的一个节点自己的设置(节点页用)。
type lineNodeReq struct {
	Key string        `json:"key"` // 节点键:"s<服务器 id>"(直连)/ "f<入口 id>"(中转入口)
	Opt model.NodeOpt `json:"opt"` // 这个节点的全部设置(整份替换;全是 0 = 恢复默认)
	// Src 只对中转入口:出口看到的来源 IP(model.LineForward.Src)。不带 = 不动;带空串 = 清掉手填的
	Src *string `json:"src"`
}

// handleLineNode 改一个节点的设置。不走整条线路的保存:不重新校验端口和协议参数,渲染出来的配置也不变,
// 保存后数据面不重启,只重新下发节点表(runner.applyLimits);副机随下一次快照推送更新。
func (s *Server) handleLineNode(w http.ResponseWriter, r *http.Request, id uint) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req lineNodeReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		badRequest(w, fmt.Errorf("请求体不是合法 JSON: %w", err))
		return
	}
	var line model.Line
	if err := s.db.First(&line, id).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "线路不存在"})
		return
	}
	if !s.lineNodeKeys(line)[req.Key] {
		badRequest(w, fmt.Errorf("这条线路没有这个节点(线路的部署范围或中转入口可能刚改过,刷新后再试)"))
		return
	}
	if err := req.Opt.Check(); err != nil {
		badRequest(w, err)
		return
	}
	opts := model.ParseNodeOpts(line.NodeOpts)
	if opts == nil {
		opts = map[string]model.NodeOpt{}
	}
	opts[req.Key] = req.Opt
	b, err := json.Marshal(opts)
	if err != nil {
		badRequest(w, err)
		return
	}
	line.NodeOpts = b
	if err := s.normNodeOpts(&line); err != nil { // 顺手把已经不存在的节点的键清掉、空的去掉
		badRequest(w, err)
		return
	}
	cols := []string{"node_opts"}
	if req.Src != nil {
		if !strings.HasPrefix(req.Key, "f") {
			badRequest(w, fmt.Errorf("只有中转入口才有来源 IP"))
			return
		}
		src, err := model.NormForwardSrc(*req.Src)
		if err != nil {
			badRequest(w, fmt.Errorf("来源 IP 不对: %w", err))
			return
		}
		fs := model.ParseForwards(line.Forwards)
		found := false
		for i := range fs {
			if model.ForwardKey(fs[i].Id) == req.Key {
				fs[i].Src, found = src, true
			}
		}
		if found {
			fb, err := json.Marshal(fs)
			if err != nil {
				badRequest(w, err)
				return
			}
			line.Forwards = fb
			cols = append(cols, "forwards")
		}
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&model.Line{}).Where("id = ?", line.Id).Select(cols).Updates(line).Error
	}); err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "line", "node", map[string]interface{}{"line": line.Name, "node": req.Key, "opt": req.Opt})
	s.reloadAll("修改线路 " + line.Name + " 的节点设置") // 配置文本没变:不重启,只重新下发限速和节点表
	writeJSON(w, http.StatusOK, line)
}

// lineNodeRoute 认出 /lines/{id}/node,返回线路 id。
func lineNodeRoute(path string) (uint, bool) {
	path = strings.TrimSuffix(path, "/")
	if !strings.HasSuffix(path, "/node") {
		return 0, false
	}
	idx := strings.LastIndex(path, "/lines/")
	if idx < 0 {
		return 0, false
	}
	id, err := strconv.ParseUint(strings.TrimSuffix(path[idx+len("/lines/"):], "/node"), 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return uint(id), true
}
