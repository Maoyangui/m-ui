package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
	"github.com/Maoyangui/m-ui/safe"
	"github.com/Maoyangui/m-ui/upstream"

	"gorm.io/gorm"
)

// ---- 外部节点(分享链接 / 外部订阅)----

func (s *Server) handleExts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var exts []model.ExtNode
		s.db.Order("sort asc, id asc").Find(&exts)
		type row struct {
			model.ExtNode
			UserCount    int64 `json:"userCount"`    // 分给了几个用户
			NoGroupCount int64 `json:"noGroupCount"` // 其中关了「单独成组」的几个(编辑框里改默认值时,算有几个用户会跟着变)
		}
		// 每个订阅的分配数一条分组查询拿齐(列表每几秒轮询一次,不再每个订阅各查一次)
		type cnt struct {
			ExtId   uint
			N       int64
			NoGroup int64
		}
		var cs []cnt
		s.db.Model(&model.UserExt{}).Select("ext_id, COUNT(*) AS n, SUM(CASE WHEN no_group THEN 1 ELSE 0 END) AS no_group").Group("ext_id").Scan(&cs)
		by := make(map[uint]cnt, len(cs))
		for _, c := range cs {
			by[c.ExtId] = c
		}
		out := make([]row, 0, len(exts))
		for _, e := range exts {
			e.Cache = "" // 列表不返回抓取内容
			// 隐藏 / 改名记录也不返回:列表每几秒轮询一次,这些(最多几千条)只在展开时用,展开接口会给
			e.Hidden, e.Renames, e.HiddenKeys, e.RenameKeys = nil, nil, nil, nil
			c := by[e.Id]
			out = append(out, row{ExtNode: e, UserCount: c.N, NoGroupCount: c.NoGroup})
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
		e.LastTry, e.ContentSig, e.FetchAgent, e.PrefsRev, e.NodeCount, e.NodeTotal = 0, "", "", 0, 0, 0
		if e.Type != "sub" {
			e.NoGroup = false // 分享链接只有一个节点,不成组,没有这个开关
		}
		if e.Type == "link" {
			e.NodeCount, e.NodeTotal = ext.Counts(e)
			e.ContentSig = ext.ContentSig(ext.ItemsOf(e))
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
			safe.Go1("web.RefreshExt", e.Id, func(id uint) { _, _ = s.run.RefreshExt(id) })
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
	if len(rest) >= 2 && rest[1] == "nodes" { // 展开:节点明细 / 逐台测速 / 添加为上游
		s.handleExtNodes(w, r, e, rest[2:])
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
			it := ext.ItemsOf(e)
			names := make([]string, 0, len(it.Nodes))
			for _, n := range it.Nodes {
				names = append(names, n.Name)
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"links": len(it.Links()), "clash": len(it.Clash()), "names": names})
		default:
			http.NotFound(w, r)
		}
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			model.ExtNode
			// ApplyNoGroup 把「分给用户时单独成组」同步给已经分到这个订阅的用户(编辑框里改了默认值、勾着「同时改掉已分配的 N 个用户」)
			ApplyNoGroup bool `json:"applyNoGroup"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, err)
			return
		}
		p := body.ExtNode
		p.Id = id
		if err := s.validateExt(&p); err != nil {
			badRequest(w, err)
			return
		}
		p.Exclude = strings.TrimSpace(p.Exclude)
		updates := map[string]interface{}{"name": p.Name, "type": p.Type, "value": p.Value, "prefix": p.Prefix, "enabled": p.Enabled, "sort": p.Sort, "remark": p.Remark, "exclude": p.Exclude}
		// 节点数按改后的地址 / 关键词重算(列表上的节点数是输出到订阅的个数,扣除了隐藏与排除的)
		next := e
		next.Type, next.Value, next.Exclude = p.Type, p.Value, p.Exclude
		if p.Type != e.Type || p.Value != e.Value {
			updates["cache"], updates["last_fetch"], updates["last_error"] = "", 0, ""
			updates["last_try"], updates["content_sig"], updates["fetch_agent"] = 0, "", ""
			next.Cache = ""
			if p.Type == "link" {
				updates["content_sig"] = ext.ContentSig(ext.ItemsOf(next))
			}
		}
		updates["node_count"], updates["node_total"] = ext.Counts(next)
		noGroup := p.NoGroup && p.Type == "sub" // 分享链接只有一个节点,不成组,没有这个开关
		updates["no_group"] = noGroup
		var applied int64
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.ExtNode{}).Where("id = ?", id).Updates(updates).Error; err != nil {
				return err
			}
			switch {
			case p.Type != "sub":
				// 改成了分享链接:分配上留着的「不成组」一并清掉(分享链接没有这个开关,以后再改回订阅时从成组开始)
				return tx.Model(&model.UserExt{}).Where("ext_id = ? AND no_group = ?", id, true).Update("no_group", false).Error
			case body.ApplyNoGroup:
				res := tx.Model(&model.UserExt{}).Where("ext_id = ? AND no_group <> ?", id, noGroup).Update("no_group", noGroup)
				applied = res.RowsAffected
				return res.Error
			}
			return nil
		})
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "ext", "update", p.Name)
		if applied > 0 {
			s.audit(r, "ext", "group-apply", map[string]interface{}{"name": p.Name, "noGroup": noGroup, "users": applied})
		}
		if p.Type == "sub" && (p.Value != e.Value || e.Cache == "") {
			safe.Go1("web.RefreshExt", id, func(id uint) { _, _ = s.run.RefreshExt(id) })
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": "1", "applied": applied})
	case http.MethodDelete:
		s.db.Delete(&model.ExtNode{}, id)
		s.db.Where("ext_id = ?", id).Delete(&model.UserExt{})
		s.db.Where("ext_id = ?", id).Delete(&model.UpstreamSource{}) // 从它加来的上游还在,只是不再有订阅可跟
		s.audit(r, "ext", "delete", e.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// writeUserExts 在调用方的事务里全量替换用户的外部节点分配(重复的 id 只记一次)。
// noGroupIds 是关了「单独成组」的外部订阅 id。传 nil 表示调用方不管这个开关(套餐变更、外部 API 只给 extIds、老前端):
// 留下的订阅沿用原来的开关,新加的按这个外部订阅自己的默认值(ExtNode.NoGroup,默认成组)——
// 不然续一次套餐、API 改一次分配,管理员逐个关掉的开关就悄悄全打开了。
func writeUserExts(tx *gorm.DB, userID uint, extIds []uint, noGroupIds []uint) error {
	noGroup := map[uint]bool{}
	if noGroupIds == nil {
		var cur []model.UserExt
		if err := tx.Where("user_id = ?", userID).Find(&cur).Error; err != nil {
			return err
		}
		had := map[uint]bool{}
		for _, r := range cur {
			had[r.ExtId] = true
			if r.NoGroup {
				noGroup[r.ExtId] = true
			}
		}
		// 新加的分配按这个外部订阅自己的默认值(外部节点编辑框里的「分给用户时单独成组」)
		var fresh []uint
		for _, eid := range extIds {
			if !had[eid] {
				fresh = append(fresh, eid)
			}
		}
		if len(fresh) > 0 {
			var defs []model.ExtNode
			if err := tx.Select("id").Where("id IN ? AND type = ? AND no_group = ?", fresh, "sub", true).Find(&defs).Error; err != nil {
				return err
			}
			for _, d := range defs {
				noGroup[d.Id] = true
			}
		}
	}
	for _, eid := range noGroupIds {
		noGroup[eid] = true
	}
	if err := tx.Where("user_id = ?", userID).Delete(&model.UserExt{}).Error; err != nil {
		return err
	}
	seen := map[uint]bool{}
	for _, eid := range extIds {
		if seen[eid] {
			continue
		}
		seen[eid] = true
		if err := tx.Create(&model.UserExt{UserId: userID, ExtId: eid, NoGroup: noGroup[eid]}).Error; err != nil {
			return err
		}
	}
	return nil
}

// apiWriteUserExts 外部 API 落外部节点分配(只主面板令牌,代理令牌由调用方挡掉;新建、修改、带套餐修改三条路共用):
//   - extIds、extNoGroupIds 都给:整份替换,给出的关掉「单独成组」,其余成组;
//   - 只给 extIds:留下的沿用原来的开关,新加的按外部订阅自己的默认值(writeUserExts 传 nil);
//   - 只给 extNoGroupIds:分配不动,只把现有分配的开关改成「给出的关、其余开」。
//
// extNoGroupIds 里每个 id 都得是存在的外部订阅、而且(改完以后)分给了这个用户,不是就报错 —— 调用方看得到哪个不对,
// 不像面板(前端只会提交勾着的)那样静默忽略。
func apiWriteUserExts(tx *gorm.DB, uid uint, extIds, noGroupIds *[]uint) error {
	if extIds == nil && noGroupIds == nil {
		return nil
	}
	if noGroupIds != nil && len(*noGroupIds) > 0 {
		assigned := map[uint]bool{}
		if extIds != nil {
			for _, id := range *extIds {
				assigned[id] = true
			}
		} else {
			var cur []model.UserExt
			if err := tx.Where("user_id = ?", uid).Find(&cur).Error; err != nil {
				return err
			}
			for _, r := range cur {
				assigned[r.ExtId] = true
			}
		}
		var exts []model.ExtNode
		if err := tx.Select("id", "type").Where("id IN ?", *noGroupIds).Find(&exts).Error; err != nil {
			return err
		}
		typ := map[uint]string{}
		for _, e := range exts {
			typ[e.Id] = e.Type
		}
		for _, id := range *noGroupIds {
			switch {
			case typ[id] == "":
				return fmt.Errorf("extNoGroupIds 里的外部节点 %d 不存在", id)
			case !assigned[id]:
				return fmt.Errorf("extNoGroupIds 里的外部节点 %d 没有分给这个用户(要先放进 extIds)", id)
			case typ[id] != "sub":
				return fmt.Errorf("extNoGroupIds 里的外部节点 %d 是分享链接:只有外部订阅能设「单独成组」", id)
			}
		}
	}
	if extIds != nil {
		var ng []uint // nil = 调用方没给开关
		if noGroupIds != nil {
			ng = append([]uint{}, *noGroupIds...) // 给了空列表也是「全部成组」,不能变成 nil
		}
		return writeUserExts(tx, uid, *extIds, ng)
	}
	if err := tx.Model(&model.UserExt{}).Where("user_id = ?", uid).Update("no_group", false).Error; err != nil {
		return err
	}
	if len(*noGroupIds) == 0 {
		return nil
	}
	return tx.Model(&model.UserExt{}).Where("user_id = ? AND ext_id IN ?", uid, *noGroupIds).Update("no_group", true).Error
}
