package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/fangjunsheng555/m-ui/core"
	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/render"
	"github.com/fangjunsheng555/m-ui/upstream"
)

// dryRunUpstream 保存前用 sing-box 干跑该出站:拦住会拖垮数据面的坏配置
//(如 tuic uuid 格式错误),错误原样回给前端。
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

// defaultTestURL 是上游健康检查的目标(与 clash/s-ui 默认一致),可用设置 upstreamTestUrl 覆盖。
const defaultTestURL = "http://www.gstatic.com/generate_204"

type upstreamTestResult struct {
	Id      uint   `json:"id"`
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	DelayMs int    `json:"delayMs"`
	Method  string `json:"method"` // urltest(经数据面真实请求)| tcp(端口探测)| none
	Error   string `json:"error,omitempty"`
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
	writeJSON(w, http.StatusOK, s.testUpstream(up))
}

// handleUpstreamTestAll 并发测试全部上游(最多 12 个同时),按 id 排序返回。
func (s *Server) handleUpstreamTestAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var ups []model.Upstream
	s.db.Order("id asc").Find(&ups)

	results := make([]upstreamTestResult, len(ups))
	sem := make(chan struct{}, 12)
	var wg sync.WaitGroup
	for i, up := range ups {
		wg.Add(1)
		go func(i int, up model.Upstream) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.testUpstream(up)
		}(i, up)
	}
	wg.Wait()
	sort.Slice(results, func(a, b int) bool { return results[a].Id < results[b].Id })
	writeJSON(w, http.StatusOK, results)
}

// testUpstream 对单个上游做健康检查(与定时巡检共用 runner.CheckUpstream)。
func (s *Server) testUpstream(up model.Upstream) upstreamTestResult {
	ok, ms, method, errStr := s.run.CheckUpstream(up)
	return upstreamTestResult{Id: up.Id, Name: up.Name, OK: ok, DelayMs: ms, Method: method, Error: errStr}
}

// handleUpstreamHealth GET /upstreams/health:定时巡检的最近结果;POST 立即巡检一次。
func (s *Server) handleUpstreamHealth(w http.ResponseWriter, r *http.Request) {
	m := s.run.Monitor()
	if r.Method == http.MethodPost {
		changed := m.RunUpstreamCheck()
		s.audit(r, "upstream", "health-check", changed)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": m.Results(), "lastRun": m.LastRun(),
		"intervalMinutes": s.settingInt("upstreamCheckMinutes", 10),
	})
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
