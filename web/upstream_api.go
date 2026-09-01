package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

// dryRunLine 保存前做线路参数的结构校验。
func (s *Server) dryRunLine(line *model.Line) error {
	ib, err := render.InboundJSON(*line)
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

// testUpstream 对单个上游做健康检查。
// 数据面运行时:经该上游真实请求测试 URL,得到端到端延迟(最可信,WARP 是否通也能测出)。
// 数据面未运行时:TCP 类上游做端口探测兜底;QUIC 类(tuic/hysteria2)无法离线测试。
func (s *Server) testUpstream(up model.Upstream) upstreamTestResult {
	res := upstreamTestResult{Id: up.Id, Name: up.Name}
	testURL := s.setting("upstreamTestUrl")
	if testURL == "" {
		testURL = defaultTestURL
	}

	if s.run.CoreRunning() {
		r := s.run.TestUpstream(up.Name, testURL)
		res.Method = "urltest"
		res.OK, res.DelayMs, res.Error = r.OK, int(r.Delay), r.Error
		if r.Error == "outbound not found" {
			res.Error = "数据面中尚无该上游(刚创建/修改请等重载完成后再测)"
		}
		return res
	}

	switch up.Type {
	case "shadowsocks", "socks", "http":
		addr, err := upstream.ServerAddr(up.Options)
		if err != nil {
			res.Method, res.Error = "none", err.Error()
			return res
		}
		res.Method = "tcp"
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			res.Error = "TCP 连接失败: " + err.Error()
			return res
		}
		conn.Close()
		res.OK = true
		res.DelayMs = int(time.Since(start).Milliseconds())
	default:
		res.Method = "none"
		res.Error = "数据面未运行:tuic/hysteria2 走 QUIC,需数据面运行后才能真实测试"
	}
	return res
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
