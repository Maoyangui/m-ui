package web

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/stats"
)

// ---- 线路详情:这条线路的总流量、各服务器上的流量、此刻在线的用户与 IP ----
//
// 线路的流量时序(stats 表 resource = inbound、tag = 线路名)各台服务器只记在自己库里,副机不上报。详情打开时
// 主机按同一组时间桶(hours / bucket / 查看者时区)向部署了这条线路的副机各要一份(agent/line-stats,并发、每台最多等 6 秒),
// 和本机的合在一起:总流量是各台相加,同时按服务器分开列。联系不上、版本太旧没有这个接口的副机在表里标出来,不算进总数。
// 在线用户和 IP 用的是副机每 5 秒随报告送来的「用户 → 源 IP → 线路」(本机的来自连接跟踪器)。

// lineServer 一台服务器上这条线路的流量。
type lineServer struct {
	NodeId   uint         `json:"nodeId"`
	Name     string       `json:"name"`
	IsLocal  bool         `json:"isLocal"`
	OK       bool         `json:"ok"`
	Disabled bool         `json:"disabled,omitempty"` // 服务器已停用:列出来,但不算「没取到」
	Error    string       `json:"error,omitempty"`
	Up       int64        `json:"up"`
	Down     int64        `json:"down"`
	Series   stats.Result `json:"series"`
	Online   int          `json:"online"` // 这台上这条线路此刻在线的用户数
}

// lineOnline 一个在线用户在某台服务器上连这条线路用的源 IP。
type lineOnline struct {
	User   string   `json:"user"`
	NodeId uint     `json:"nodeId"`
	Server string   `json:"server"`
	IPs    []string `json:"ips"`
}

// handleLineDetail GET /lines/{id}/detail?hours=24&bucket=3600&tz=480
// part=online 只要在线用户与 IP(抽屉开着时 5 秒一轮刷新用,不向副机取流量)。
// 副机自己的面板上只有本机这一份(副机联系不了别的副机)。
func (s *Server) handleLineDetail(w http.ResponseWriter, r *http.Request, id uint) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var line model.Line
	if err := s.db.First(&line, id).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "线路不存在"})
		return
	}
	q := r.URL.Query()
	hours, _ := strconv.Atoi(q.Get("hours"))
	if hours <= 0 || hours > 24*90 {
		hours = 24
	}
	bucket, _ := strconv.Atoi(q.Get("bucket"))
	if bucket <= 0 {
		bucket = int(stats.BucketFor(hours))
	}
	loc := s.panelLocation()
	if tz, err := strconv.Atoi(q.Get("tz")); err == nil && tz >= -14*60 && tz <= 14*60 {
		loc = time.FixedZone("viewer", tz*60)
	}
	floor := s.settingInt("statsBucketSeconds", 60)

	var nodes []model.Node
	s.db.Order("sort asc, id asc").Find(&nodes)
	if s.run != nil && s.run.IsNode() {
		local := nodes[:0]
		for _, n := range nodes {
			if n.IsLocal {
				local = append(local, n)
			}
		}
		nodes = local
	}
	onlines := s.lineOnlines(line.Name, nodes)
	perNode := map[uint]int{}
	users := map[string]bool{}
	ips := 0
	for _, o := range onlines {
		perNode[o.NodeId]++
		users[o.User] = true
		ips += len(o.IPs)
	}
	if q.Get("part") == "online" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"onlines": onlines, "onlineUsers": len(users), "onlineIPs": ips, "perNode": perNode})
		return
	}

	servers := make([]lineServer, 0, len(nodes))
	st := s.hubStatuses() // 上一轮巡检已经联系不上的副机直接标出来,不让抽屉干等 6 秒
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range nodes {
		if !render.LineOnNode(line, n.Id) {
			continue
		}
		ls := lineServer{NodeId: n.Id, Name: n.Name, IsLocal: n.IsLocal, Online: perNode[n.Id]}
		if n.IsLocal {
			ls.Series = stats.Series(s.db, "inbound", line.Name, hours, int64(bucket), floor, loc)
			ls.OK, ls.Up, ls.Down = true, ls.Series.TotalUp, ls.Series.TotalDown
			servers = append(servers, ls)
			continue
		}
		if !n.Enabled {
			ls.Disabled, ls.Error = true, "服务器已停用"
			servers = append(servers, ls)
			continue
		}
		if v, seen := st[n.Id]; s.run == nil || (seen && !v.OK) {
			ls.Error = "联系不上这台服务器"
			servers = append(servers, ls)
			continue
		}
		i := len(servers)
		servers = append(servers, ls)
		wg.Add(1)
		go func(n model.Node, i int) {
			defer wg.Done()
			res, err := s.run.Hub().LineStats(n, url.Values{"tag": {line.Name}, "hours": {strconv.Itoa(hours)}, "bucket": {strconv.Itoa(bucket)}, "tz": {q.Get("tz")}})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if hub.IsNotFound(err) {
					servers[i].Error = "这台服务器的版本太旧,升级后才有按线路的流量"
				} else {
					servers[i].Error = "联系不上这台服务器"
				}
				return
			}
			servers[i].OK, servers[i].Series, servers[i].Up, servers[i].Down = true, res, res.TotalUp, res.TotalDown
		}(n, i)
	}
	wg.Wait()

	// 总流量:以本机(或第一台拿到的)那份的时间格为准,别的按时间点落进同一格(各台的钟和取数时刻差几秒,格子起点可能差一格)
	var total stats.Result
	for _, sv := range servers {
		if sv.OK && len(sv.Series.Points) > 0 {
			total = stats.Result{Points: make([]stats.Point, len(sv.Series.Points)), Span: sv.Series.Span, Start: sv.Series.Start, End: sv.Series.End}
			for i := range total.Points {
				total.Points[i].T = sv.Series.Start + int64(i)*sv.Series.Span
			}
			break
		}
	}
	for _, sv := range servers {
		if !sv.OK || total.Span == 0 {
			continue
		}
		for _, p := range sv.Series.Points {
			i := int((p.T - total.Start + total.Span/2) / total.Span)
			if i < 0 {
				i = 0
			}
			if i >= len(total.Points) {
				i = len(total.Points) - 1
			}
			if i < 0 {
				continue
			}
			total.Points[i].Up += p.Up
			total.Points[i].Down += p.Down
		}
		total.TotalUp += sv.Up
		total.TotalDown += sv.Down
	}
	sort.SliceStable(servers, func(a, b int) bool { return servers[a].Up+servers[a].Down > servers[b].Up+servers[b].Down })

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"line":        map[string]interface{}{"id": line.Id, "name": line.Name, "protocol": line.Protocol, "port": line.Port, "enabled": line.Enabled},
		"total":       total,
		"servers":     servers,
		"onlines":     onlines,
		"onlineUsers": len(users),
		"onlineIPs":   ips,
	})
}

// lineOnlines 此刻连着这条线路的用户:本机来自连接跟踪器,副机来自它们的上报;按服务器分开,IP 排好序。
func (s *Server) lineOnlines(lineName string, nodes []model.Node) []lineOnline {
	if s.run == nil {
		return []lineOnline{}
	}
	return collectLineOnlines(lineName, nodes, s.run.OnlineIPLines(), s.run.Hub().RemoteIPLinesRaw())
}

// collectLineOnlines 从「用户 → 源 IP → 线路」里挑出连着 lineName 的:只算 nodes 里的服务器(已删的服务器留下的旧报告不算)。
func collectLineOnlines(lineName string, nodes []model.Node, local map[string]map[string][]string, remote map[uint]map[string]map[string][]string) []lineOnline {
	names := map[uint]string{}
	var localId uint
	for _, n := range nodes {
		names[n.Id] = n.Name
		if n.IsLocal {
			localId = n.Id
		}
	}
	out := []lineOnline{}
	collect := func(nodeId uint, m map[string]map[string][]string) {
		for user, ipLines := range m {
			var ips []string
			for ip, lines := range ipLines {
				for _, l := range lines {
					if l == lineName {
						ips = append(ips, ip)
						break
					}
				}
			}
			if len(ips) > 0 {
				sort.Strings(ips)
				out = append(out, lineOnline{User: user, NodeId: nodeId, Server: names[nodeId], IPs: ips})
			}
		}
	}
	if localId != 0 {
		collect(localId, local)
	}
	for id, m := range remote {
		if _, ok := names[id]; ok && id != localId {
			collect(id, m)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].User != out[b].User {
			return out[a].User < out[b].User
		}
		return out[a].Server < out[b].Server
	})
	return out
}

// handleAgentLineStats 副机:GET agent/line-stats?tag=线路名&hours=&bucket=&tz=,返回本机这条线路的流量时序(主机的线路详情要)。
func (s *Server) handleAgentLineStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tag := q.Get("tag")
	if tag == "" {
		badRequest(w, errors.New("缺少线路名"))
		return
	}
	hours, _ := strconv.Atoi(q.Get("hours"))
	bucket, _ := strconv.Atoi(q.Get("bucket"))
	loc := s.panelLocation()
	if tz, err := strconv.Atoi(q.Get("tz")); err == nil && tz >= -14*60 && tz <= 14*60 {
		loc = time.FixedZone("viewer", tz*60)
	}
	writeJSON(w, http.StatusOK, stats.Series(s.db, "inbound", tag, hours, int64(bucket), s.settingInt("statsBucketSeconds", 60), loc))
}
