package web

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/safe"
)

// ---- 流量明细:这段时间的流量分给了谁 ----
//
// 概况页点「流量」那一格看的。同一个合计,三种分法:
//   - 按用户:全部服务器合计,按倍率计(和用量、计费同一口径);
//   - 按服务器:各副机是主机并账时记下的(按倍率计),本机是本机各线路的合计乘本机倍率;
//   - 按线路:线路的流量只记在它所在那台服务器自己的库里(真实字节)——本机的直接查,副机的临时去问(agent/line-totals),
//     各乘那台的倍率后按线路名合起来。问不到的副机(离线、版本旧没有这个接口)列在 linesMissing 里,
//     它们那部分流量在线路这一栏里就归不到具体线路上。
// 中转入口自己的入站(名字是「线路 ⇄端口」)不算:那段流量在出口服务器的线路上已经算过一遍。

type trafficItem struct {
	Name  string `json:"name"`
	Up    int64  `json:"up"`
	Down  int64  `json:"down"`
	Owner string `json:"owner,omitempty"` // 用户:所属代理
	Gone  bool   `json:"gone,omitempty"`  // 用户 / 服务器已经删了或改了名(流量记录还在)
	Local bool   `json:"local,omitempty"` // 服务器:本机
}

func (t trafficItem) sum() int64 { return t.Up + t.Down }

// relayInboundMark 中转入口的入站名里有它(见 render 包):「线路名 ⇄端口」。
const relayInboundMark = "⇄"

// tagTotals 库里某一类对象这段时间各自的上下行合计。
func (s *Server) tagTotals(resource string, since int64) []trafficItem {
	var rows []trafficItem
	s.db.Raw(`SELECT tag AS name, COALESCE(SUM(CASE WHEN direction = 1 THEN traffic ELSE 0 END),0) AS up,
		COALESCE(SUM(CASE WHEN direction = 0 THEN traffic ELSE 0 END),0) AS down
		FROM stats WHERE resource = ? AND date_time > ? GROUP BY tag`, resource, since).Scan(&rows)
	return rows
}

// lineTotals 本机各线路这段时间的真实流量(中转入口的入站不算)。
func (s *Server) lineTotals(since int64) []hub.LineTotal {
	out := []hub.LineTotal{}
	for _, r := range s.tagTotals("inbound", since) {
		if strings.Contains(r.Name, relayInboundMark) || r.sum() == 0 {
			continue
		}
		out = append(out, hub.LineTotal{Name: r.Name, Up: r.Up, Down: r.Down})
	}
	return out
}

// handleAgentLineTotals 副机:GET agent/line-totals?hours=,本机各线路这段时间的真实流量(主机的流量明细要)。
func (s *Server) handleAgentLineTotals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lineTotals(time.Now().Unix()-int64(breakdownHours(r))*3600))
}

func breakdownHours(r *http.Request) int {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 24*90 {
		hours = 24
	}
	return hours
}

func scaleBy(v int64, ratio float64) int64 {
	if ratio <= 0 || ratio == 1 {
		return v
	}
	return int64(float64(v) * ratio)
}

// sortTraffic 按合计从大到小;一样大的按名字,顺序稳定。
func sortTraffic(list []trafficItem) {
	sort.Slice(list, func(i, j int) bool {
		if a, b := list[i].sum(), list[j].sum(); a != b {
			return a > b
		}
		return list[i].Name < list[j].Name
	})
}

// handleStatsBreakdown GET /stats/breakdown?hours=24:合计,以及按用户、按服务器、按线路各怎么分。
func (s *Server) handleStatsBreakdown(w http.ResponseWriter, r *http.Request) {
	hours := breakdownHours(r)
	since := time.Now().Unix() - int64(hours)*3600

	// 用户:全部列出来由前端取前几名(用户再多也就是一行一个名字两个数);合计按全部用户算
	users := s.tagTotals("user", since)
	var total trafficItem
	for _, u := range users {
		total.Up += u.Up
		total.Down += u.Down
	}
	sortTraffic(users)
	userCount := len(users)
	if len(users) > 50 {
		users = users[:50]
	}
	if len(users) > 0 { // 名字点过去要去对地方:代理名下的去代理页,删掉的不给链接
		names := make([]string, len(users))
		for i, x := range users {
			names[i] = x.Name
		}
		type who struct{ Name, Owner string }
		var ws []who
		s.db.Raw(`SELECT u.name AS name, COALESCE(r.name, '') AS owner FROM users u LEFT JOIN resellers r ON r.id = u.reseller_id WHERE u.name IN ?`, names).Scan(&ws)
		by := make(map[string]string, len(ws))
		for _, x := range ws {
			by[x.Name] = x.Owner
		}
		for i := range users {
			owner, ok := by[users[i].Name]
			users[i].Owner, users[i].Gone = owner, !ok
		}
	}
	out := map[string]interface{}{"hours": hours, "total": total, "users": users, "userCount": userCount}

	// 服务器与线路:只有主机(或单机)给;副机面板只看得到自己这一台,没有分法可言
	if s.role() == "node" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	var nodes []model.Node
	s.db.Order("sort asc, id asc").Find(&nodes)
	localRatio := 1.0
	byName := map[string]model.Node{}
	for _, n := range nodes {
		byName[n.Name] = n
		if n.IsLocal && n.Ratio > 0 {
			localRatio = n.Ratio
		}
	}
	local := s.lineTotals(since)

	// 按线路:本机的 + 各副机的(并发去问,每台最多等 6 秒)
	lines := map[string]*trafficItem{}
	addLine := func(name string, up, down int64, ratio float64) {
		it := lines[name]
		if it == nil {
			it = &trafficItem{Name: name}
			lines[name] = it
		}
		it.Up += scaleBy(up, ratio)
		it.Down += scaleBy(down, ratio)
	}
	var localSum trafficItem
	for _, l := range local {
		addLine(l.Name, l.Up, l.Down, localRatio)
		localSum.Up += scaleBy(l.Up, localRatio)
		localSum.Down += scaleBy(l.Down, localRatio)
	}
	missing := []string{}
	{
		var h *hub.Hub
		if s.run != nil {
			h = s.run.Hub()
		}
		sts := map[uint]hub.NodeStatus{}
		if h != nil {
			sts = h.Statuses()
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, n := range nodes {
			if n.IsLocal || !n.Enabled {
				continue
			}
			if st, ok := sts[n.Id]; !ok || !st.OK { // 联系不上的(含还没联系过的):问不到,记进 linesMissing
				missing = append(missing, n.Name)
				continue
			}
			wg.Add(1)
			safe.Go1("web.handleStatsBreakdown", n, func(n model.Node) {
				defer wg.Done()
				got, err := h.LineTotalsOn(n, hours)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					missing = append(missing, n.Name)
					return
				}
				for _, l := range got {
					if strings.Contains(l.Name, relayInboundMark) {
						continue
					}
					addLine(l.Name, l.Up, l.Down, n.Ratio)
				}
			})
		}
		wg.Wait()
	}
	sort.Strings(missing)
	lineList := make([]trafficItem, 0, len(lines))
	for _, it := range lines {
		if it.sum() > 0 {
			lineList = append(lineList, *it)
		}
	}
	sortTraffic(lineList)

	// 按服务器:副机用并账时记下的那份(名字是当时的名字,后来删了 / 改了名的标出来),本机用本机线路的合计
	nodeList := []trafficItem{}
	for _, n := range s.tagTotals("node", since) {
		if n.sum() == 0 {
			continue
		}
		_, ok := byName[n.Name]
		n.Gone = !ok
		nodeList = append(nodeList, n)
	}
	if localSum.sum() > 0 {
		localSum.Name, localSum.Local = s.localNodeName(), true
		nodeList = append(nodeList, localSum)
	}
	sortTraffic(nodeList)

	out["nodes"], out["lines"], out["linesMissing"] = nodeList, lineList, missing
	writeJSON(w, http.StatusOK, out)
}
