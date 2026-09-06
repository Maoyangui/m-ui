package web

import (
	"net/http"
	"sort"
	"sync"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/render"
)

// 上游健康:按"上游 × 使用它的服务器"汇总。
//
// 一条上游通不通,只有在真正跑这条线路的机器上量才算数:主机在香港、落地从高带宽出去,
// 主机测得通不代表高带宽通;反过来,主机不部署线路时,它测不通只是它自己没有那条路,与谁都无关。
// 所以每台服务器只测自己线路用到的上游,结果随报告汇总到主机,面板按服务器展示。

// upServer 一台服务器上这条上游的状态。
type upServer struct {
	NodeId    uint   `json:"nodeId"`
	Name      string `json:"name"`
	IsLocal   bool   `json:"isLocal"`
	State     string `json:"state"` // ok | fail | pending(还没测到)| stale(该副机失联,数据过期)
	DelayMs   int    `json:"delayMs"`
	Method    string `json:"method,omitempty"`
	Error     string `json:"error,omitempty"`
	CheckedAt int64  `json:"checkedAt,omitempty"`
}

// upRow 一条上游一行:哪些服务器在用它、各自什么状态。
type upRow struct {
	Id      uint       `json:"id"`
	Name    string     `json:"name"`
	Unused  bool       `json:"unused"` // 没有任何启用的线路用它:不测也不告警
	Servers []upServer `json:"servers"`
}

// upstreamUsers 每条上游被哪些服务器用到:启用的线路 ∩ 部署到该服务器 ∩ 指定了这条上游。
func (s *Server) upstreamUsers() (map[uint]map[uint]bool, []model.Node) {
	var nodes []model.Node
	s.db.Where("enabled = ?", true).Order("sort asc, id asc").Find(&nodes)
	var lines []model.Line
	s.db.Select("id, upstream_id, node_ids").Where("enabled = ? AND upstream_id > 0", true).Find(&lines)
	out := map[uint]map[uint]bool{}
	for _, l := range lines {
		for _, n := range nodes {
			if !render.LineOnNode(l, n.Id) {
				continue
			}
			if out[l.UpstreamId] == nil {
				out[l.UpstreamId] = map[uint]bool{}
			}
			out[l.UpstreamId][n.Id] = true
		}
	}
	return out, nodes
}

// upstreamHealthRows 汇总面板要显示的那张表。
func (s *Server) upstreamHealthRows() []upRow {
	var ups []model.Upstream
	s.db.Order("sort asc, id asc").Find(&ups)
	users, nodes := s.upstreamUsers()

	// 本机的结果来自本机巡检器,副机的来自它们的上报
	local := map[uint]hub.UpstreamHealth{}
	var selfID uint
	if s.run != nil {
		selfID = s.run.LocalNodeId()
		for _, h := range s.run.UpstreamHealthLocal() {
			local[h.Id] = hub.UpstreamHealth{Id: h.Id, Name: h.Name, OK: h.OK, DelayMs: h.DelayMs,
				Method: h.Method, Error: h.Error, CheckedAt: h.CheckedAt, Fails: h.Fails}
		}
	}
	remote := map[uint][]hub.UpstreamHealth{}
	// down:主机确认联系不上的副机。"还没联系过"(刚接入、Hub 还没跑完第一轮)不算掉线,
	// 那种情况显示待测就好,别一上来给人一片"数据过期"。
	down := map[uint]bool{}
	if s.run != nil && s.run.Hub() != nil {
		remote = s.run.Hub().UpstreamHealthAll()
		for id, st := range s.run.Hub().Statuses() {
			down[id] = !st.OK
		}
	}

	rows := make([]upRow, 0, len(ups))
	for _, up := range ups {
		row := upRow{Id: up.Id, Name: up.Name}
		useBy := users[up.Id]
		if len(useBy) == 0 {
			row.Unused = true
			rows = append(rows, row)
			continue
		}
		for _, n := range nodes {
			if !useBy[n.Id] {
				continue
			}
			sv := upServer{NodeId: n.Id, Name: n.Name, IsLocal: n.IsLocal, State: "pending"}
			var h hub.UpstreamHealth
			var got bool
			if n.IsLocal || n.Id == selfID {
				h, got = local[up.Id]
			} else {
				for _, x := range remote[n.Id] {
					if x.Id == up.Id {
						h, got = x, true
						break
					}
				}
			}
			switch {
			case !got:
				// 还没测到:副机刚接入、或它那一轮还没跑完;确认掉线的才标过期,不当故障
				if !n.IsLocal && down[n.Id] {
					sv.State = "stale"
				}
			case !n.IsLocal && down[n.Id]:
				sv.State, sv.DelayMs, sv.Method, sv.Error, sv.CheckedAt = "stale", h.DelayMs, h.Method, h.Error, h.CheckedAt
			case h.OK:
				sv.State, sv.DelayMs, sv.Method, sv.CheckedAt = "ok", h.DelayMs, h.Method, h.CheckedAt
			default:
				sv.State, sv.Method, sv.Error, sv.CheckedAt = "fail", h.Method, h.Error, h.CheckedAt
			}
			row.Servers = append(row.Servers, sv)
		}
		rows = append(rows, row)
	}
	return rows
}

// testUpstreamEverywhere 面板"测试"按钮:派发到每台真正用这条上游的服务器上实测。
// 没有任何服务器用它时,退回在本机测一次(至少给个参考值)。
func (s *Server) testUpstreamEverywhere(up model.Upstream) []upServer {
	users, nodes := s.upstreamUsers()
	useBy := users[up.Id]
	var targets []model.Node
	for _, n := range nodes {
		if useBy[n.Id] {
			targets = append(targets, n)
		}
	}
	if len(targets) == 0 {
		ok, ms, meth, errStr := s.run.CheckUpstream(up)
		sv := upServer{Name: s.localNodeName(), IsLocal: true, State: "fail", Method: meth, Error: errStr}
		if ok {
			sv.State, sv.DelayMs = "ok", ms
		}
		return []upServer{sv}
	}
	out := make([]upServer, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, n := range targets {
		wg.Add(1)
		go func(i int, n model.Node) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sv := upServer{NodeId: n.Id, Name: n.Name, IsLocal: n.IsLocal}
			if n.IsLocal {
				ok, ms, meth, errStr := s.run.CheckUpstream(up)
				sv.State, sv.DelayMs, sv.Method, sv.Error = "fail", 0, meth, errStr
				if ok {
					sv.State, sv.DelayMs = "ok", ms
				}
			} else {
				h, err := s.run.Hub().TestUpstreamOn(n, up.Id)
				switch {
				case err != nil:
					sv.State, sv.Error = "stale", err.Error()
				case h.OK:
					sv.State, sv.DelayMs, sv.Method = "ok", h.DelayMs, h.Method
				default:
					sv.State, sv.Method, sv.Error = "fail", h.Method, h.Error
				}
			}
			out[i] = sv
		}(i, n)
	}
	wg.Wait()
	sort.Slice(out, func(a, b int) bool { return out[a].NodeId < out[b].NodeId })
	return out
}

// handleUpstreamHealth GET /upstreams/health:按服务器汇总的最近巡检结果;POST 让本机立刻巡检一次。
func (s *Server) handleUpstreamHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && s.run != nil {
		changed := s.run.Monitor().RunUpstreamCheck()
		s.audit(r, "upstream", "health-check", changed)
	}
	var lastRun int64
	if s.run != nil {
		lastRun = s.run.Monitor().LastRun()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": s.upstreamHealthRows(), "lastRun": lastRun,
		"intervalMinutes": s.settingInt("upstreamCheckMinutes", 10),
	})
}
