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
	"github.com/Maoyangui/m-ui/upstream"
)

// dryRunUpstream 保存前用 sing-box 干跑该出站:拦住会拖垮数据面的坏配置
// (如 tuic uuid 格式错误),错误原样回给前端。
func (s *Server) dryRunUpstream(up *model.Upstream) error {
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

// defaultTestURL 是上游健康检查的目标(与 clash 默认一致),可用设置 upstreamTestUrl 覆盖。
const defaultTestURL = "http://www.gstatic.com/generate_204"

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
		go func(i int, up model.Upstream) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = upRow{Id: up.Id, Name: up.Name, Servers: s.testUpstreamEverywhere(up)}
		}(i, up)
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
