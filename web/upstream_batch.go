package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
	"gorm.io/gorm"
)

// 上游页的批量操作(用户 11-22「上游新增可勾选批量选择」):勾选几条上游,一起删、一起停止 / 恢复跟随订阅。
// 批量测试不走这里:前端逐条调 /upstreams/{id}/test(每条都要往各台服务器派发,逐条出结果,页面上一格一格亮起来)。

type upstreamBatchReq struct {
	Ids    []uint `json:"ids"`
	Action string `json:"action"` // delete / follow / unfollow
}

// upstreamBatchSkip 没删的一条和原因:Lines = 在用它的线路;Group = 它所在、这次又不一起删的上游组。
type upstreamBatchSkip struct {
	Name  string   `json:"name"`
	Lines []string `json:"lines,omitempty"`
	Group string   `json:"group,omitempty"`
}

// handleUpstreamBatch POST /upstreams/batch {ids, action}
func (s *Server) handleUpstreamBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req upstreamBatchReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		badRequest(w, errors.New("请求格式错误"))
		return
	}
	ids := uniqueIDs(req.Ids)
	if len(ids) == 0 {
		badRequest(w, errors.New("没有选中上游"))
		return
	}
	var ups []model.Upstream
	s.db.Where("id IN ?", ids).Order("id asc").Find(&ups)
	if len(ups) != len(ids) {
		badRequest(w, errors.New("有上游不存在,请刷新后重试"))
		return
	}
	switch req.Action {
	case "delete":
		s.batchDeleteUpstreams(w, r, ids)
	case "follow", "unfollow":
		n := 0
		for _, u := range ups {
			if s.setUpstreamFollow(r, u, req.Action == "follow") {
				n++
			}
		}
		writeJSON(w, http.StatusOK, map[string]int{"changed": n})
	default:
		badRequest(w, errors.New("不认识的批量操作"))
	}
}

// batchDeleteUpstreams 能删的一起删,删不了的跳过并说明原因,和单条删除同一套拦法:有线路在用(自己选的出站或分流规则指向它)的不删;
// 在上游组里的,那个组这次也一起删才删(组先删、成员后删)。判断和删除在同一个事务里(连接串是 _txlock=immediate,
// 一开头就拿写锁,判断完到删之间别人改不了);删完只重载一次。
func (s *Server) batchDeleteUpstreams(w http.ResponseWriter, r *http.Request, ids []uint) {
	var deleted []model.Upstream
	var skipped []upstreamBatchSkip
	err := s.db.Transaction(func(tx *gorm.DB) error {
		deleted, skipped = nil, nil
		var ups []model.Upstream
		if err := tx.Where("id IN ?", ids).Order("id asc").Find(&ups).Error; err != nil {
			return err
		}
		ok := map[uint]bool{}
		for _, u := range ups {
			if using := lineNamesUsingUpstream(tx, u.Id); len(using) > 0 {
				skipped = append(skipped, upstreamBatchSkip{Name: u.Name, Lines: using})
				continue
			}
			ok[u.Id] = true
		}
		groups := map[string]bool{} // 这次一起删的组
		for _, u := range ups {
			if ok[u.Id] && render.IsGroup(u) {
				groups[u.Name] = true
			}
		}
		for _, u := range ups {
			if !ok[u.Id] || render.IsGroup(u) {
				continue
			}
			for _, g := range groupsWith(tx, u.Name) {
				if !groups[g] {
					ok[u.Id] = false
					skipped = append(skipped, upstreamBatchSkip{Name: u.Name, Group: g})
					break
				}
			}
		}
		for _, u := range ups {
			if ok[u.Id] {
				deleted = append(deleted, u)
			}
		}
		sort.SliceStable(deleted, func(a, b int) bool { return render.IsGroup(deleted[a]) && !render.IsGroup(deleted[b]) }) // 组先删
		for _, u := range deleted {
			if err := tx.Delete(&model.Upstream{}, u.Id).Error; err != nil {
				return err
			}
			if err := tx.Where("upstream_id = ?", u.Id).Delete(&model.UpstreamSource{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		badRequest(w, err)
		return
	}
	for _, u := range deleted {
		s.audit(r, "upstream", "delete", u.Name)
	}
	if len(deleted) > 0 {
		s.reloadUsers("批量删除上游") // 线路上游的专用用户要从目标线路撤下
		s.reloadUpstreams("批量删除上游")
	}
	if skipped == nil {
		skipped = []upstreamBatchSkip{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": len(deleted), "skipped": skipped})
}

// setUpstreamFollow 打开 / 停止一条上游跟随外部节点(单条的开关和批量共用)。不是从外部节点加来的、或者本来就是这个状态,不动,返回 false。
// 停止:来源留着,以后还能再打开;重新打开:上次尝试的时间清掉(不必等半小时的间隔),结论也清掉(那是停下之前的事)。
func (s *Server) setUpstreamFollow(r *http.Request, up model.Upstream, follow bool) bool {
	var src model.UpstreamSource
	if s.db.First(&src, "upstream_id = ?", up.Id).Error != nil {
		return false
	}
	if (src.Stop == "") == follow {
		return false
	}
	upd := map[string]interface{}{"stop": model.FollowStopUser}
	if follow {
		upd = map[string]interface{}{"stop": "", "tried_at": 0, "note": "", "detail": ""}
	}
	s.db.Model(&model.UpstreamSource{}).Where("upstream_id = ?", up.Id).Updates(upd)
	s.audit(r, "upstream", map[bool]string{true: "follow:on", false: "follow:off"}[follow], up.Name)
	return true
}
