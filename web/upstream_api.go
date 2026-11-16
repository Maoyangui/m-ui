package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/safe"
	"github.com/Maoyangui/m-ui/upstream"
)

// dryRunUpstream 保存前用 sing-box 干跑该出站:拦住会拖垮数据面的坏配置
// (如 tuic uuid 格式错误),错误原样回给前端。
func (s *Server) dryRunUpstream(up *model.Upstream) error {
	if render.IsGroup(*up) {
		return nil // 单独校验不了(成员不在这份配置里);成员已查过,整份配置的干跑在事务里做
	}
	ob, err := render.OutboundJSON(*up)
	if err != nil {
		return err
	}
	return core.ValidateOutbound(ob)
}

// dryRunLine 保存前做线路参数、TLS 与传输配置的结构校验。
func (s *Server) dryRunLine(line *model.Line) error {
	ib, err := render.InboundJSON(*line, s.run.NodeCert())
	if err != nil {
		return err
	}
	return core.ValidateInbound(ib)
}

// dispatchUpstreamSubroute 处理 /upstreams/test、/upstreams/parse、/upstreams/{id}/test。
// 返回 true 表示请求已被处理。
func (s *Server) dispatchUpstreamSubroute(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case strings.HasSuffix(path, "/upstreams/test"):
		s.handleUpstreamTestAll(w, r)
		return true
	case strings.HasSuffix(path, "/upstreams/parse"):
		s.handleUpstreamParse(w, r)
		return true
	case strings.HasSuffix(path, "/upstreams/health"):
		s.handleUpstreamHealth(w, r)
		return true
	case strings.HasSuffix(path, "/source"): // DELETE /upstreams/{id}/source:解除跟随(这条上游以后不再跟着外部节点改)
		trimmed := strings.TrimSuffix(path, "/source")
		idx := strings.LastIndex(trimmed, "/upstreams/")
		if idx < 0 {
			return false
		}
		id, err := strconv.ParseUint(trimmed[idx+len("/upstreams/"):], 10, 64)
		if err != nil {
			badRequest(w, fmt.Errorf("上游 id 无效"))
			return true
		}
		// PUT {follow: true|false}:打开 / 停止跟随。DELETE 是老写法,等于停止跟随(来源不删:以后还能再打开)
		follow := false
		switch r.Method {
		case http.MethodDelete:
		case http.MethodPut:
			var p struct {
				Follow *bool `json:"follow"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&p); err != nil || p.Follow == nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
				return true
			}
			follow = *p.Follow
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return true
		}
		var up model.Upstream
		if s.db.First(&up, uint(id)).Error != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "上游不存在"})
			return true
		}
		var src model.UpstreamSource
		if s.db.First(&src, "upstream_id = ?", up.Id).Error != nil {
			badRequest(w, fmt.Errorf("这条上游不是从外部节点加来的,没有可以跟随的订阅"))
			return true
		}
		upd := map[string]interface{}{"stop": model.FollowStopUser}
		if follow {
			// 重新打开:上次尝试的时间清掉(不必等半小时的间隔),结论也清掉(那是停下之前的事)
			upd = map[string]interface{}{"stop": "", "tried_at": 0, "note": "", "detail": ""}
		}
		if (src.Stop == "") != follow {
			s.db.Model(&model.UpstreamSource{}).Where("upstream_id = ?", up.Id).Updates(upd)
			s.audit(r, "upstream", map[bool]string{true: "follow:on", false: "follow:off"}[follow], up.Name)
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
		return true
	case strings.HasSuffix(path, "/test"):
		trimmed := strings.TrimSuffix(path, "/test")
		idx := strings.LastIndex(trimmed, "/upstreams/")
		if idx < 0 {
			return false
		}
		id, err := strconv.ParseUint(trimmed[idx+len("/upstreams/"):], 10, 64)
		if err != nil {
			badRequest(w, fmt.Errorf("上游 id 无效"))
			return true
		}
		s.handleUpstreamTestOne(w, r, uint(id))
		return true
	}
	return false
}

func (s *Server) handleUpstreamTestOne(w http.ResponseWriter, r *http.Request, id uint) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var up model.Upstream
	if err := s.db.First(&up, id).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "上游不存在"})
		return
	}
	writeJSON(w, http.StatusOK, upRow{Id: up.Id, Name: up.Name, Servers: s.testUpstreamEverywhere(up)})
}

// handleUpstreamTestAll 并发测试全部上游(最多 12 个同时),按 id 排序返回。
func (s *Server) handleUpstreamTestAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var ups []model.Upstream
	s.db.Order("id asc").Find(&ups)

	results := make([]upRow, len(ups))
	sem := make(chan struct{}, 6) // 每条上游还要往各机派发,并发别开太大
	var wg sync.WaitGroup
	for i, up := range ups {
		wg.Add(1)
		safe.Go2("web.handleUpstreamTestAll", i, up, func(i int, up model.Upstream) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = upRow{Id: up.Id, Name: up.Name, Servers: s.testUpstreamEverywhere(up)}
		})
	}
	wg.Wait()
	sort.Slice(results, func(a, b int) bool { return results[a].Id < results[b].Id })
	writeJSON(w, http.StatusOK, results)
}

// handleNotifyTest POST /notify/test:发送 Telegram 测试消息。
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	if err := s.run.Notifier().Test(); err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// handleUpstreamParse 把分享链接解析为上游表单数据。
func (s *Server) handleUpstreamParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req struct {
		Link string `json:"link"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	p, err := upstream.ParseLink(req.Link)
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
