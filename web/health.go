package web

import (
	"net"
	"net/http"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/runner"
)

// healthStatus 是 GET api/health 的返回:面板能应答只说明进程活着,线路是否真的在监听要看数据面。
// 一键更新的守护和安装脚本以前只看首页 2xx,sing-box 升版后某个存量参数被废弃、数据面空跑,升级会被判成功。
type healthStatus struct {
	OK       bool                `json:"ok"`
	Core     bool                `json:"core"`     // 数据面在跑
	Inbounds int                 `json:"inbounds"` // 数据面里的入站数
	Lines    int                 `json:"lines"`    // 库里启用且部署在本机的线路数(期望的入站数)
	Reload   runner.ReloadStatus `json:"reload"`   // 最近一次重载的结果
	Version  string              `json:"version"`
}

// expectedLines 启用且部署到本机的线路数。
func (s *Server) expectedLines() int {
	var lines []model.Line
	s.db.Select("id, node_ids").Where("enabled = ?", true).Find(&lines)
	self := s.localNodeID()
	n := 0
	for _, l := range lines {
		if render.LineOnNode(l, self) {
			n++
		}
	}
	return n
}

func (s *Server) healthStatus() healthStatus {
	st := healthStatus{Lines: s.expectedLines(), Version: Version}
	if s.run != nil {
		st.Core, st.Inbounds, st.Reload = s.run.CoreRunning(), s.run.InboundCount(), s.run.ReloadStatus()
	}
	// 没有线路的机器(纯管理主机、刚装好)数据面不跑也算健康;有线路就要求监听器都起来了
	st.OK = st.Lines == 0 || (st.Core && st.Inbounds >= st.Lines)
	return st
}

// handleHealth 不走会话:只允许 127.0.0.1 / ::1 访问,别的来源一律 403,不向外泄露任何计数。
// 不健康返回 503,守护和脚本只看状态码。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ip := net.ParseIP(peerIP(r))
	if ip == nil || !ip.IsLoopback() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "只允许本机访问"})
		return
	}
	st := s.healthStatus()
	code := http.StatusOK
	if !st.OK {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, st)
}
