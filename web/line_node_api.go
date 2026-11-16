package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"

	"gorm.io/gorm"
)

// lineNodeReq PUT /lines/{id}/node 的请求:改这条线路的一个节点自己的设置(节点页用)。
type lineNodeReq struct {
	Key string        `json:"key"` // 节点键:"s<服务器 id>"(直连)/ "f<入口 id>"(中转入口)
	Opt model.NodeOpt `json:"opt"` // 这个节点的全部设置(整份替换;全是 0 = 恢复默认)
	// Src 只对中转入口:出口看到的来源 IP(model.LineForward.Src)。不带 = 不动;带空串 = 清掉手填的
	Src *string `json:"src"`
}

// handleLineNode 改一个节点的设置。不走整条线路的保存:不重新校验端口和协议参数,渲染出来的配置也不变,
// 保存后数据面不重启,只重新下发节点表(runner.applyLimits);副机随下一次快照推送更新。
func (s *Server) handleLineNode(w http.ResponseWriter, r *http.Request, id uint) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req lineNodeReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		badRequest(w, fmt.Errorf("请求体不是合法 JSON: %w", err))
		return
	}
	var line model.Line
	if err := s.db.First(&line, id).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "线路不存在"})
		return
	}
	if !s.lineNodeKeys(line)[req.Key] {
		badRequest(w, fmt.Errorf("这条线路没有这个节点(线路的部署范围或中转入口可能刚改过,刷新后再试)"))
		return
	}
	if err := req.Opt.Check(); err != nil {
		badRequest(w, err)
		return
	}
	opts := model.ParseNodeOpts(line.NodeOpts)
	if opts == nil {
		opts = map[string]model.NodeOpt{}
	}
	opts[req.Key] = req.Opt
	b, err := json.Marshal(opts)
	if err != nil {
		badRequest(w, err)
		return
	}
	line.NodeOpts = b
	if err := s.normNodeOpts(&line); err != nil { // 顺手把已经不存在的节点的键清掉、空的去掉
		badRequest(w, err)
		return
	}
	cols := []string{"node_opts"}
	if req.Src != nil {
		if !strings.HasPrefix(req.Key, "f") {
			badRequest(w, fmt.Errorf("只有中转入口才有来源 IP"))
			return
		}
		src, err := model.NormForwardSrc(*req.Src)
		if err != nil {
			badRequest(w, fmt.Errorf("来源 IP 不对: %w", err))
			return
		}
		fs := model.ParseForwards(line.Forwards)
		found := false
		for i := range fs {
			if model.ForwardKey(fs[i].Id) == req.Key {
				fs[i].Src, found = src, true
			}
		}
		if found {
			fb, err := json.Marshal(fs)
			if err != nil {
				badRequest(w, err)
				return
			}
			line.Forwards = fb
			cols = append(cols, "forwards")
		}
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&model.Line{}).Where("id = ?", line.Id).Select(cols).Updates(line).Error
	}); err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "line", "node", map[string]interface{}{"line": line.Name, "node": req.Key, "opt": req.Opt})
	s.reloadAll("修改线路 " + line.Name + " 的节点设置") // 配置文本没变:不重启,只重新下发限速和节点表
	writeJSON(w, http.StatusOK, line)
}

// lineNodeRoute 认出 /lines/{id}/node,返回线路 id。
func lineNodeRoute(path string) (uint, bool) {
	path = strings.TrimSuffix(path, "/")
	if !strings.HasSuffix(path, "/node") {
		return 0, false
	}
	idx := strings.LastIndex(path, "/lines/")
	if idx < 0 {
		return 0, false
	}
	id, err := strconv.ParseUint(strings.TrimSuffix(path[idx+len("/lines/"):], "/node"), 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return uint(id), true
}

// lineNodesBatchReq PUT /lines/nodes 的请求:一批节点改同样的几项(节点页勾选后的批量操作)。Set 里没带的项不改。
type lineNodesBatchReq struct {
	Items []struct {
		Line uint   `json:"line"`
		Key  string `json:"key"`
	} `json:"items"`
	Set struct {
		Hidden    *bool    `json:"hidden"`
		Ratio     *float64 `json:"ratio"`
		Speed     *int     `json:"speed"`
		UserSpeed *int     `json:"userSpeed"`
		Cap       *int     `json:"cap"`
	} `json:"set"`
}

// maxNodeBatch 一次最多改多少个节点。
const maxNodeBatch = 5000

// handleLineNodesBatch 批量改节点设置:全部校验通过才在一笔事务里写,写完只下发一次。
// 有任何一个节点已经不在(线路删了、部署范围或中转入口刚改过)就整批不做,让界面刷新后重选 —— 不做一半。
func (s *Server) handleLineNodesBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req lineNodesBatchReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		badRequest(w, fmt.Errorf("请求体不是合法 JSON: %w", err))
		return
	}
	set := req.Set
	if set.Hidden == nil && set.Ratio == nil && set.Speed == nil && set.UserSpeed == nil && set.Cap == nil {
		badRequest(w, fmt.Errorf("没有要改的项"))
		return
	}
	if len(req.Items) == 0 {
		badRequest(w, fmt.Errorf("没有选节点"))
		return
	}
	if len(req.Items) > maxNodeBatch {
		badRequest(w, fmt.Errorf("一次最多改 %d 个节点", maxNodeBatch))
		return
	}
	keysOf := map[uint][]string{}
	var order []uint
	for _, it := range req.Items {
		if _, ok := keysOf[it.Line]; !ok {
			order = append(order, it.Line)
		}
		keysOf[it.Line] = append(keysOf[it.Line], it.Key)
	}
	var lines []model.Line
	if err := s.db.Where("id IN ?", order).Find(&lines).Error; err != nil {
		badRequest(w, err)
		return
	}
	byId := make(map[uint]*model.Line, len(lines))
	for i := range lines {
		byId[lines[i].Id] = &lines[i]
	}
	gone := 0
	for _, id := range order {
		l := byId[id]
		if l == nil {
			gone += len(keysOf[id])
			continue
		}
		valid := s.lineNodeKeys(*l)
		opts := model.ParseNodeOpts(l.NodeOpts)
		if opts == nil {
			opts = map[string]model.NodeOpt{}
		}
		for _, key := range keysOf[id] {
			if !valid[key] {
				gone++
				continue
			}
			o := opts[key]
			if set.Hidden != nil {
				o.Hidden = *set.Hidden
			}
			if set.Ratio != nil {
				o.Ratio = *set.Ratio
			}
			if set.Speed != nil {
				o.Speed = *set.Speed
			}
			if set.UserSpeed != nil {
				o.UserSpeed = *set.UserSpeed
			}
			if set.Cap != nil {
				o.Cap = *set.Cap
			}
			if err := o.Check(); err != nil {
				badRequest(w, err)
				return
			}
			opts[key] = o
		}
		b, err := json.Marshal(opts)
		if err != nil {
			badRequest(w, err)
			return
		}
		l.NodeOpts = b
		if err := s.normNodeOpts(l); err != nil {
			badRequest(w, err)
			return
		}
	}
	if gone > 0 {
		badRequest(w, fmt.Errorf("选中的节点里有 %d 个已经不在了(线路、部署范围或中转入口刚改过),刷新后重新选", gone))
		return
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		for _, id := range order {
			if err := tx.Model(&model.Line{}).Where("id = ?", id).Select("node_opts").Updates(byId[id]).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "line", "nodes", map[string]interface{}{"count": len(req.Items), "lines": len(order), "set": set})
	s.reloadAll("批量修改节点设置") // 配置文本没变:不重启,只重新下发限速和节点表
	out := make([]model.Line, 0, len(order))
	for _, id := range order {
		out = append(out, *byId[id])
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"lines": out, "changed": len(req.Items)})
}

// nodeOnlines 各节点此刻在线的用户数,键 "<线路 id>:<节点键>"(节点键见 model.DirectKey / ForwardKey)。
// local 是本机的「用户 → 源 IP → 线路名」,remote 是各副机报上来的同一形状(按服务器 id)—— 和线路详情用的是同一份数据。
// 一条连接算哪个节点:它在哪台服务器上、连的哪条线路;来源 IP 对得上这条线路转到这台的某个中转入口就是那个入口,否则是这台的直连
// (和数据面认节点的办法一样,见 runner.ForwardSources)。同一个用户在一个节点上开几条连接、用几台设备只算一个。
func nodeOnlines(lines []model.Line, nodes []model.Node, local map[string]map[string][]string, remote map[uint]map[string]map[string][]string) map[string]int {
	byName := make(map[string]model.Line, len(lines))
	for _, l := range lines {
		byName[l.Name] = l
	}
	byId := make(map[uint]model.Node, len(nodes))
	var localId uint
	for _, n := range nodes {
		byId[n.Id] = n
		if n.IsLocal {
			localId = n.Id
		}
	}
	// 线路 id → 出口服务器 id → 来源 IP → 入口的键;用到才算
	type exitKey struct{ line, node uint }
	srcs := map[exitKey]map[netip.Addr]string{}
	sourcesOf := func(l model.Line, node uint) map[netip.Addr]string {
		k := exitKey{l.Id, node}
		if m, ok := srcs[k]; ok {
			return m
		}
		m := map[netip.Addr]string{}
		for _, f := range model.ParseForwards(l.Forwards) {
			if f.Off || f.To != node || f.Id == 0 {
				continue
			}
			for _, a := range runner.ForwardSources(f, byId) {
				if _, dup := m[a]; !dup {
					m[a] = model.ForwardKey(f.Id)
				}
			}
		}
		srcs[k] = m
		return m
	}
	seen := map[string]map[string]bool{}
	collect := func(node uint, users map[string]map[string][]string) {
		if _, ok := byId[node]; !ok {
			return // 已经删掉的服务器留下的旧报告
		}
		for user, ips := range users {
			for ip, names := range ips {
				for _, name := range names {
					l, ok := byName[name]
					if !ok {
						continue
					}
					key := model.DirectKey(node)
					if m := sourcesOf(l, node); len(m) > 0 {
						if a, err := netip.ParseAddr(ip); err == nil {
							if fk, hit := m[a.Unmap().WithZone("")]; hit {
								key = fk
							}
						}
					}
					id := strconv.FormatUint(uint64(l.Id), 10) + ":" + key
					if seen[id] == nil {
						seen[id] = map[string]bool{}
					}
					seen[id][user] = true
				}
			}
		}
	}
	if localId != 0 {
		collect(localId, local)
	}
	for id, m := range remote {
		if id != localId {
			collect(id, m)
		}
	}
	out := make(map[string]int, len(seen))
	for id, us := range seen {
		out[id] = len(us)
	}
	return out
}

// handleLineNodesOnline GET /lines/nodes/online:节点页「在线 / 容量」一列用。
func (s *Server) handleLineNodesOnline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	out := map[string]int{}
	if s.run != nil {
		var lines []model.Line
		s.db.Select("id, name, forwards").Find(&lines)
		var nodes []model.Node
		s.db.Select("id, is_local, public_ip, public_ip6, addr").Find(&nodes)
		out = nodeOnlines(lines, nodes, s.run.OnlineIPLines(), s.run.Hub().RemoteIPLinesRaw())
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"online": out})
}
