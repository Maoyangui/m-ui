package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
	"github.com/Maoyangui/m-ui/upstream"
)

// ---- 外部节点(分享链接 / 外部订阅)----

func (s *Server) handleExts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var exts []model.ExtNode
		s.db.Order("sort asc, id asc").Find(&exts)
		type row struct {
			model.ExtNode
			UserCount int64 `json:"userCount"`
		}
		out := make([]row, 0, len(exts))
		for _, e := range exts {
			e.Cache = "" // 列表不返回抓取内容
			var n int64
			s.db.Model(&model.UserExt{}).Where("ext_id = ?", e.Id).Count(&n)
			out = append(out, row{ExtNode: e, UserCount: n})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var e model.ExtNode
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.validateExt(&e); err != nil {
			badRequest(w, err)
			return
		}
		e.Id, e.Cache, e.LastFetch, e.LastError = 0, "", 0, ""
		if e.Type == "link" {
			e.NodeCount = 1
		}
		if err := s.db.Create(&e).Error; err != nil {
			badRequest(w, err)
			return
		}
		if !e.Enabled {
			s.db.Model(&model.ExtNode{}).Where("id = ?", e.Id).Update("enabled", false)
		}
		s.audit(r, "ext", "create", e.Name)
		if e.Type == "sub" {
			go s.run.RefreshExt(e.Id)
		}
		e.Cache = ""
		writeJSON(w, http.StatusOK, e)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

func (s *Server) validateExt(e *model.ExtNode) error {
	e.Name = strings.TrimSpace(e.Name)
	e.Value = strings.TrimSpace(e.Value)
	e.Prefix = strings.Trim(e.Prefix, "\r\n\t") // 保留用户有意留的空格,如 "[外] "
	if e.Name == "" {
		return errors.New("名称不能为空")
	}
	switch e.Type {
	case "link":
		if _, err := upstream.ParseLink(e.Value); err != nil {
			return errors.New("分享链接无法解析: " + err.Error())
		}
	case "sub":
		if !strings.HasPrefix(e.Value, "http://") && !strings.HasPrefix(e.Value, "https://") {
			return errors.New("外部订阅地址必须以 http:// 或 https:// 开头")
		}
	default:
		return errors.New("类型必须是 link(分享链接)或 sub(外部订阅)")
	}
	dup := s.db.Model(&model.ExtNode{}).Where("name = ?", e.Name)
	if e.Id != 0 {
		dup = dup.Where("id != ?", e.Id)
	}
	var n int64
	dup.Count(&n)
	if n > 0 {
		return errors.New("名称已存在")
	}
	return nil
}

func (s *Server) handleExtItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.Split(strings.TrimPrefix(r.URL.Path, innerBase+"api/exts/"), "/")
	id64, err := strconv.ParseUint(rest[0], 10, 64)
	if err != nil {
		badRequest(w, errors.New("id 无效"))
		return
	}
	id := uint(id64)
	var e model.ExtNode
	if err := s.db.First(&e, id).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "外部节点不存在"})
		return
	}
	if len(rest) == 2 {
		switch rest[1] {
		case "refresh":
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
				return
			}
			res, err := s.run.RefreshExt(id)
			if err != nil {
				badRequest(w, err)
				return
			}
			s.audit(r, "ext", "refresh", e.Name)
			writeJSON(w, http.StatusOK, res)
		case "preview": // 解析结果预览(节点名列表)
			it := ext.Parse(e.Value)
			if e.Type == "sub" {
				it = ext.Parse(e.Cache)
			}
			names := make([]string, 0, len(it.Clash))
			for _, p := range it.Clash {
				n, _ := p["name"].(string)
				names = append(names, n)
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"links": len(it.Links), "clash": len(it.Clash), "names": names})
		default:
			http.NotFound(w, r)
		}
		return
	}
	switch r.Method {
	case http.MethodPut:
		var p model.ExtNode
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		p.Id = id
		if err := s.validateExt(&p); err != nil {
			badRequest(w, err)
			return
		}
		updates := map[string]interface{}{"name": p.Name, "type": p.Type, "value": p.Value, "prefix": p.Prefix, "enabled": p.Enabled, "sort": p.Sort, "remark": p.Remark}
		if p.Type != e.Type || p.Value != e.Value {
			updates["cache"], updates["last_fetch"], updates["last_error"] = "", 0, ""
			updates["node_count"] = 0
			if p.Type == "link" {
				updates["node_count"] = 1
			}
		}
		if err := s.db.Model(&model.ExtNode{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "ext", "update", p.Name)
		if p.Type == "sub" && (p.Value != e.Value || e.Cache == "") {
			go s.run.RefreshExt(id)
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	case http.MethodDelete:
		s.db.Delete(&model.ExtNode{}, id)
		s.db.Where("ext_id = ?", id).Delete(&model.UserExt{})
		s.audit(r, "ext", "delete", e.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// setUserExts 全量替换用户的外部节点分配。
func (s *Server) setUserExts(userID uint, extIds []uint) {
	s.db.Where("user_id = ?", userID).Delete(&model.UserExt{})
	for _, eid := range extIds {
		s.db.Create(&model.UserExt{UserId: userID, ExtId: eid})
	}
}
