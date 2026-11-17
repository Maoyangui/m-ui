package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/fw"
	"github.com/Maoyangui/m-ui/hub"

	"gorm.io/gorm"
)

// 面板的「防火墙」一栏(在主机上管;规则随配置快照发到各台,每台自己落成内核规则,见 runner/fw.go):
//
//	GET    fw                                   总览:两个开关、各服务器的状况、规则、黑名单、白名单
//	PUT    fw/settings                          {autoBan?, autoAllow?}
//	POST   fw/rules                             新建规则
//	PUT    fw/rules/{id} · DELETE fw/rules/{id} · POST fw/rules/{id}/toggle
//	POST   fw/bans                              手动拉黑 {ip, note, seconds}
//	DELETE fw/bans/{id} · POST fw/bans/{id}/promote(升成防火墙规则,连代理一起挡)· POST fw/bans/clear {onlyAuto}
//	POST   fw/allow · DELETE fw/allow/{id}      白名单(自动拉黑永远不碰)
//	POST   fw/servers/{id}/{state|open|undo}    这台服务器:现在核对一次(顺带列出它用到的端口)/ 放开系统防火墙 / 撤销放开
//
// 两条防呆:会把正在操作的这个人自己挡在外面的(拉黑的来源里有他现在的 IP;「只允许」管到了面板或 SSH 端口却没把他算进去),
// 要带 force 才存;集群里服务器的地址不让拉黑(主机和副机之间的同步走的就是这些地址)。

const (
	fwMaxSources = 256
	fwMaxPorts   = 64
	fwMaxSeconds = 10 * 365 * 86400
)

type fwRulePayload struct {
	Kind    string `json:"kind"`
	Proto   string `json:"proto"`
	Ports   string `json:"ports"`
	Sources string `json:"sources"`
	NodeIds []uint `json:"nodeIds"`
	Seconds *int64 `json:"seconds"` // 从现在起有效多久;0 = 一直有效;不带 = 编辑时不改到期时间
	Note    string `json:"note"`
	Enabled *bool  `json:"enabled"`
	Force   bool   `json:"force"`
}

// errSelfLock 这条规则会把提交它的人自己挡在外面:409 + code self_lockout,界面再问一次,确认了带 force 重发。
type errSelfLock struct{ err error }

func (e errSelfLock) Error() string { return e.err.Error() }

func fwFail(w http.ResponseWriter, err error) {
	var lock errSelfLock
	if errors.As(err, &lock) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": lock.Error(), "code": "self_lockout"})
		return
	}
	badRequest(w, err)
}

// clusterPrefixes 集群里每台服务器的地址(公网 v4 / v6、连接地址是 IP 的):名字 → 地址。
func (s *Server) clusterAddrs() map[netip.Addr]string {
	var nodes []model.Node
	s.db.Select("name, public_ip, public_ip6, addr").Find(&nodes)
	out := map[netip.Addr]string{}
	for _, n := range nodes {
		for _, v := range []string{n.PublicIP, n.PublicIP6, model.BareHost(n.Addr)} {
			if a, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
				out[a.Unmap().WithZone("")] = n.Name
			}
		}
	}
	return out
}

func covers(srcs []netip.Prefix, ip string) bool {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	a = a.Unmap().WithZone("")
	for _, p := range srcs {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// toRule 校验并规整一条规则。cur 是编辑前的那条(新建为 nil)。
func (s *Server) fwToRule(p fwRulePayload, cur *model.FwRule, me string, now int64) (model.FwRule, error) {
	out := model.FwRule{Origin: "manual", Enabled: true, CreatedAt: now}
	if cur != nil {
		out = *cur
	}
	out.Kind, out.Proto = strings.TrimSpace(p.Kind), strings.ToLower(strings.TrimSpace(p.Proto))
	ports, err := fw.ParsePorts(p.Ports)
	if err != nil {
		return out, err
	}
	srcs, err := fw.ParseSources(p.Sources)
	if err != nil {
		return out, err
	}
	if len(srcs) > fwMaxSources {
		return out, fmt.Errorf("一条规则最多 %d 个来源", fwMaxSources)
	}
	if len(ports) > fwMaxPorts {
		return out, fmt.Errorf("一条规则最多 %d 段端口", fwMaxPorts)
	}
	if err := (fw.Rule{Kind: out.Kind, Proto: out.Proto, Ports: ports, Sources: srcs}).Check(); err != nil {
		return out, err
	}
	for _, src := range srcs {
		if src.Bits() == 0 {
			return out, fmt.Errorf("来源不能是全部地址(0.0.0.0/0、::/0)")
		}
		if out.Kind == fw.KindDeny && src.Addr().IsLoopback() {
			return out, fmt.Errorf("不能拉黑回环地址")
		}
	}

	ps := make([]string, len(ports))
	for i, x := range ports {
		ps[i] = x.String()
	}
	ss := make([]string, len(srcs))
	for i, x := range srcs {
		ss[i] = x.String()
	}
	out.Ports, out.Sources = strings.Join(ps, ","), strings.Join(ss, ",")
	// 应用到哪些服务器:空 = 全部
	out.NodeIds = nil
	if len(p.NodeIds) > 0 {
		var n int64
		s.db.Model(&model.Node{}).Where("id IN ?", p.NodeIds).Count(&n)
		if int(n) != len(p.NodeIds) {
			return out, fmt.Errorf("选的服务器里有已经不在的,刷新后重选")
		}
		ids := append([]uint(nil), p.NodeIds...)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		b, _ := json.Marshal(ids)
		out.NodeIds = b
	}
	if p.Seconds != nil {
		switch {
		case *p.Seconds < 0 || *p.Seconds > fwMaxSeconds:
			return out, fmt.Errorf("有效期要在 0 到 10 年之间(0 = 一直有效)")
		case *p.Seconds == 0:
			out.Expiry = 0
		default:
			out.Expiry = now + *p.Seconds
		}
	}
	out.Note = strings.TrimSpace(p.Note)
	if utf8.RuneCountInString(out.Note) > 200 {
		return out, fmt.Errorf("备注太长(最多 200 个字)")
	}
	if p.Enabled != nil {
		out.Enabled = *p.Enabled
	}
	// 防呆一:集群里服务器的地址不让拉黑
	if out.Kind == fw.KindDeny {
		for a, name := range s.clusterAddrs() {
			if covers(srcs, a.String()) {
				return out, fmt.Errorf("来源里有集群服务器「%s」的地址 %s:拉黑它会断开主机和这台之间的同步", name, a)
			}
		}
	}
	// 防呆二:别把自己挡在外面。请求是从本机回环来的(本机上的反向代理、SSH 隧道)就不查:
	// 看不到操作的人真正的地址,回环也永远不受规则影响 —— 实机上每条管到面板端口的规则都被它误拦过
	if a, err := netip.ParseAddr(strings.TrimSpace(me)); err != nil || a.Unmap().IsLoopback() {
		me = ""
	}
	if !p.Force && out.Enabled && me != "" {
		switch out.Kind {
		case fw.KindDeny:
			if covers(srcs, me) {
				return out, errSelfLock{fmt.Errorf("来源里包含你现在用的 IP %s:保存后你自己也连不上这些服务器了", me)}
			}
		case fw.KindOnly:
			if !covers(srcs, me) && out.Proto != "udp" {
				if hit := s.fwGuardedPorts(ports); hit != "" {
					return out, errSelfLock{fmt.Errorf("这条规则管到了 SSH 或面板的端口(%s),允许的来源里却没有你现在用的 IP %s:保存后你自己会被挡在外面", hit, me)}
				}
			}
		}
	}
	return out, nil
}

// fwGuardedPorts 这些端口里有没有管理用的(SSH 的 22、面板端口):有就返回这些端口号("22, 2053")。
func (s *Server) fwGuardedPorts(ports []fw.PortRange) string {
	in := func(p int) bool {
		for _, r := range ports {
			if p >= r.From && p <= r.To {
				return true
			}
		}
		return false
	}
	var hit []string
	if in(22) {
		hit = append(hit, "22")
	}
	if p := s.settingInt("webPort", 2053); in(p) && p != 22 {
		hit = append(hit, strconv.Itoa(p))
	}
	return strings.Join(hit, ", ")
}

type fwServerView struct {
	Id      uint           `json:"id"`
	Name    string         `json:"name"`
	IsLocal bool           `json:"isLocal"`
	Enabled bool           `json:"enabled"`
	Online  bool           `json:"online"`
	State   *model.FwState `json:"state"`           // nil = 这台没报(离线,或版本旧没有这个功能)
	Ports   []string       `json:"ports,omitempty"` // 这台上 m-ui 要对外开的端口(本机总有;副机点「查看」时才问)
}

func (s *Server) fwServers() []fwServerView {
	var nodes []model.Node
	s.db.Select("id, name, is_local, enabled, sort").Order("sort asc, id asc").Find(&nodes)
	var statuses map[uint]hub.NodeStatus
	if s.run != nil && s.run.Hub() != nil {
		statuses = s.run.Hub().Statuses()
	}
	out := make([]fwServerView, 0, len(nodes))
	for _, n := range nodes {
		v := fwServerView{Id: n.Id, Name: n.Name, IsLocal: n.IsLocal, Enabled: n.Enabled}
		if n.IsLocal {
			v.Online = true
			if s.run != nil {
				st := s.run.FwState(false)
				v.State, v.Ports = &st, s.run.FwPorts()
			}
		} else if st, ok := statuses[n.Id]; ok {
			v.Online, v.State = st.OK, st.Fw
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) fwOverview() map[string]interface{} {
	var rules []model.FwRule
	s.db.Order("id desc").Find(&rules)
	var bans []model.FwBan
	s.db.Where("expiry = 0 OR expiry > ?", time.Now().Unix()).Order("last_at desc, id desc").Limit(1000).Find(&bans)
	var banTotal int64
	s.db.Model(&model.FwBan{}).Where("expiry = 0 OR expiry > ?", time.Now().Unix()).Count(&banTotal)
	var allow []model.FwAllow
	s.db.Order("id desc").Find(&allow)
	return map[string]interface{}{
		"autoBan":   strings.EqualFold(s.setting("fwAutoBan"), "true"),
		"autoAllow": !strings.EqualFold(s.setting("fwAutoAllow"), "false"),
		"servers":   s.fwServers(),
		"rules":     rules,
		"bans":      bans,
		"banTotal":  banTotal,
		"allow":     allow,
		"myIp":      "",
	}
}

func (s *Server) fwKick() {
	if s.run != nil {
		s.run.FwKick()
	}
}

// handleFw GET fw / PUT fw/settings / POST fw/{rules|bans|allow} …
func (s *Server) handleFw(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	idx := strings.LastIndex(path, "/fw")
	rest := ""
	if idx >= 0 {
		rest = strings.Trim(path[idx+len("/fw"):], "/")
	}
	seg := []string{}
	if rest != "" {
		seg = strings.Split(rest, "/")
	}
	me := s.sourceIP(r)
	now := time.Now().Unix()
	decode := func(v interface{}) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(v); err != nil {
			badRequest(w, fmt.Errorf("请求体不是合法 JSON: %w", err))
			return false
		}
		return true
	}
	idOf := func(i int) (uint, bool) {
		if len(seg) <= i {
			return 0, false
		}
		n, err := strconv.ParseUint(seg[i], 10, 64)
		return uint(n), err == nil && n > 0
	}
	notAllowed := func() { writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"}) }

	switch {
	case len(seg) == 0:
		if r.Method != http.MethodGet {
			notAllowed()
			return
		}
		out := s.fwOverview()
		out["myIp"] = me
		writeJSON(w, http.StatusOK, out)

	case seg[0] == "settings" && len(seg) == 1:
		if r.Method != http.MethodPut {
			notAllowed()
			return
		}
		var p struct {
			AutoBan   *bool `json:"autoBan"`
			AutoAllow *bool `json:"autoAllow"`
		}
		if !decode(&p) {
			return
		}
		if p.AutoBan != nil {
			if err := s.run.SetSetting("fwAutoBan", strconv.FormatBool(*p.AutoBan)); err != nil {
				badRequest(w, err)
				return
			}
			s.audit(r, "fw", map[bool]string{true: "autoban:on", false: "autoban:off"}[*p.AutoBan], "")
		}
		if p.AutoAllow != nil {
			if err := s.run.SetSetting("fwAutoAllow", strconv.FormatBool(*p.AutoAllow)); err != nil {
				badRequest(w, err)
				return
			}
			s.audit(r, "fw", map[bool]string{true: "autoallow:on", false: "autoallow:off"}[*p.AutoAllow], "")
			s.fwKick()
		}
		writeJSON(w, http.StatusOK, s.fwOverview())

	case seg[0] == "rules":
		s.handleFwRules(w, r, seg, me, now, decode, idOf, notAllowed)

	case seg[0] == "bans":
		s.handleFwBans(w, r, seg, me, now, decode, idOf, notAllowed)

	case seg[0] == "allow":
		switch {
		case len(seg) == 1 && r.Method == http.MethodPost:
			var p struct{ Ip, Note string }
			if !decode(&p) {
				return
			}
			srcs, err := fw.ParseSources(p.Ip)
			if err != nil || len(srcs) != 1 {
				badRequest(w, fmt.Errorf("白名单一次填一个 IP 或网段(例:1.2.3.4、203.0.113.0/24)"))
				return
			}
			if srcs[0].Bits() == 0 {
				badRequest(w, fmt.Errorf("来源不能是全部地址(0.0.0.0/0、::/0)"))
				return
			}
			ip := srcs[0].String()
			if srcs[0].IsSingleIP() {
				ip = srcs[0].Addr().String()
			}
			row := model.FwAllow{Ip: ip, Note: strings.TrimSpace(p.Note), CreatedAt: now}
			if utf8.RuneCountInString(row.Note) > 200 {
				badRequest(w, fmt.Errorf("备注太长(最多 200 个字)"))
				return
			}
			var old model.FwAllow
			if s.db.Where("ip = ?", ip).First(&old).Error == nil {
				row.Id, row.CreatedAt = old.Id, old.CreatedAt
			}
			if err := s.db.Save(&row).Error; err != nil {
				badRequest(w, err)
				return
			}
			s.audit(r, "fw", "allow:add", ip)
			s.fwKick()
			writeJSON(w, http.StatusOK, s.fwOverview())
		case len(seg) == 2 && r.Method == http.MethodDelete:
			id, ok := idOf(1)
			var row model.FwAllow
			if !ok || s.db.First(&row, id).Error != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "这条白名单已经不在了"})
				return
			}
			s.db.Delete(&row)
			s.audit(r, "fw", "allow:remove", row.Ip)
			s.fwKick()
			writeJSON(w, http.StatusOK, s.fwOverview())
		default:
			notAllowed()
		}

	case seg[0] == "servers" && len(seg) == 3:
		if r.Method != http.MethodPost {
			notAllowed()
			return
		}
		id, ok := idOf(1)
		var n model.Node
		if !ok || s.db.First(&n, id).Error != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "服务器不存在"})
			return
		}
		s.handleFwServer(w, r, n, seg[2])

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "路径无效"})
	}
}

func (s *Server) handleFwRules(w http.ResponseWriter, r *http.Request, seg []string, me string, now int64,
	decode func(interface{}) bool, idOf func(int) (uint, bool), notAllowed func()) {
	if len(seg) == 1 {
		if r.Method != http.MethodPost {
			notAllowed()
			return
		}
		var p fwRulePayload
		if !decode(&p) {
			return
		}
		rule, err := s.fwToRule(p, nil, me, now)
		if err != nil {
			fwFail(w, err)
			return
		}
		if err := s.db.Create(&rule).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "fw", "rule:create", map[string]interface{}{"kind": rule.Kind, "ports": rule.Ports, "sources": rule.Sources})
		s.fwKick()
		writeJSON(w, http.StatusOK, s.fwOverview())
		return
	}
	id, ok := idOf(1)
	var cur model.FwRule
	if !ok || s.db.First(&cur, id).Error != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "这条规则已经不在了(可能刚到期)"})
		return
	}
	switch {
	case len(seg) == 2 && r.Method == http.MethodPut:
		var p fwRulePayload
		if !decode(&p) {
			return
		}
		rule, err := s.fwToRule(p, &cur, me, now)
		if err != nil {
			fwFail(w, err)
			return
		}
		if err := s.db.Save(&rule).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "fw", "rule:update", map[string]interface{}{"kind": rule.Kind, "ports": rule.Ports, "sources": rule.Sources})
	case len(seg) == 2 && r.Method == http.MethodDelete:
		s.db.Delete(&cur)
		s.audit(r, "fw", "rule:delete", map[string]interface{}{"kind": cur.Kind, "ports": cur.Ports, "sources": cur.Sources})
	case len(seg) == 3 && seg[2] == "toggle" && r.Method == http.MethodPost:
		var p struct {
			Force bool `json:"force"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&p)
		if !cur.Enabled { // 重新启用:照样过一遍防呆
			on := true
			var ids []uint
			_ = json.Unmarshal(cur.NodeIds, &ids)
			if _, err := s.fwToRule(fwRulePayload{Kind: cur.Kind, Proto: cur.Proto, Ports: cur.Ports, Sources: cur.Sources, NodeIds: ids, Note: cur.Note, Enabled: &on, Force: p.Force}, &cur, me, now); err != nil {
				fwFail(w, err)
				return
			}
		}
		s.db.Model(&model.FwRule{}).Where("id = ?", cur.Id).Update("enabled", !cur.Enabled)
		s.audit(r, "fw", map[bool]string{true: "rule:disable", false: "rule:enable"}[cur.Enabled], map[string]interface{}{"kind": cur.Kind, "ports": cur.Ports, "sources": cur.Sources})
	default:
		notAllowed()
		return
	}
	s.fwKick()
	writeJSON(w, http.StatusOK, s.fwOverview())
}

func (s *Server) handleFwBans(w http.ResponseWriter, r *http.Request, seg []string, me string, now int64,
	decode func(interface{}) bool, idOf func(int) (uint, bool), notAllowed func()) {
	if s.run == nil || s.run.Guard() == nil {
		badRequest(w, fmt.Errorf("黑名单还没准备好,稍后再试"))
		return
	}
	g := s.run.Guard()
	switch {
	case len(seg) == 1 && r.Method == http.MethodPost: // 手动拉黑
		var p struct {
			Ip      string `json:"ip"`
			Note    string `json:"note"`
			Seconds int64  `json:"seconds"`
			Force   bool   `json:"force"`
		}
		if !decode(&p) {
			return
		}
		srcs, err := fw.ParseSources(p.Ip)
		if err != nil || len(srcs) != 1 {
			badRequest(w, fmt.Errorf("黑名单一次填一个 IP 或网段(例:1.2.3.4、203.0.113.0/24)"))
			return
		}
		if p.Seconds < 0 || p.Seconds > fwMaxSeconds {
			badRequest(w, fmt.Errorf("有效期要在 0 到 10 年之间(0 = 一直有效)"))
			return
		}
		if utf8.RuneCountInString(strings.TrimSpace(p.Note)) > 200 {
			badRequest(w, fmt.Errorf("备注太长(最多 200 个字)"))
			return
		}
		for a, name := range s.clusterAddrs() {
			if srcs[0].Contains(a) {
				badRequest(w, fmt.Errorf("来源里有集群服务器「%s」的地址 %s:拉黑它会断开主机和这台之间的同步", name, a))
				return
			}
		}
		if !p.Force && covers(srcs, me) {
			fwFail(w, errSelfLock{fmt.Errorf("这里面包含你现在用的 IP %s:保存后你自己就打不开面板了", me)})
			return
		}
		b, err := g.Add(p.Ip, p.Note, p.Seconds)
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "fw", "ban:add", b.Ip)
	case len(seg) == 2 && seg[1] == "clear" && r.Method == http.MethodPost:
		var p struct {
			OnlyAuto bool `json:"onlyAuto"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&p)
		n := g.Clear(p.OnlyAuto)
		s.audit(r, "fw", "ban:clear", map[string]interface{}{"count": n, "onlyAuto": p.OnlyAuto})
	case len(seg) == 2 && r.Method == http.MethodDelete:
		id, ok := idOf(1)
		var b model.FwBan
		if !ok || s.db.First(&b, id).Error != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "这条已经不在黑名单里了(可能刚到期)"})
			return
		}
		g.Remove(b.Id, "")
		s.audit(r, "fw", "ban:remove", b.Ip)
	case len(seg) == 3 && seg[2] == "promote" && r.Method == http.MethodPost: // 升成防火墙规则:连代理流量一起挡
		id, ok := idOf(1)
		var b model.FwBan
		if !ok || s.db.First(&b, id).Error != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "这条已经不在黑名单里了(可能刚到期)"})
			return
		}
		var p struct {
			NodeIds []uint `json:"nodeIds"`
			Force   bool   `json:"force"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&p)
		var secs *int64
		if b.Expiry > now {
			left := b.Expiry - now
			secs = &left
		} else {
			zero := int64(0)
			secs = &zero
		}
		rule, err := s.fwToRule(fwRulePayload{Kind: fw.KindDeny, Sources: b.Ip, NodeIds: p.NodeIds, Seconds: secs, Note: strings.TrimSpace("黑名单 " + b.Evidence), Force: p.Force}, nil, me, now)
		if err != nil {
			fwFail(w, err)
			return
		}
		rule.Origin = "ban"
		if err := s.db.Create(&rule).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "fw", "ban:promote", b.Ip)
		s.fwKick()
	default:
		notAllowed()
		return
	}
	writeJSON(w, http.StatusOK, s.fwOverview())
}

// pruneFwRulesTx 删服务器时:面板防火墙规则的「应用到」里点了它名的,把它去掉;去掉后一台都不剩的规则整条删掉 ——
// 留一个空名单就成了「全部服务器」,规则的范围会悄悄变大(和用户 / 代理的线路范围是同一个坑,见 revokeSoleNodeScopes)。
// 返回整条删掉的规则数。
func pruneFwRulesTx(tx *gorm.DB, nodeID uint) (removed int, err error) {
	var rules []model.FwRule
	if err = tx.Find(&rules).Error; err != nil {
		return 0, err
	}
	for _, r := range rules {
		var ids []uint
		if len(r.NodeIds) == 0 || json.Unmarshal(r.NodeIds, &ids) != nil || len(ids) == 0 {
			continue // 本来就是全部服务器
		}
		kept, hit := make([]uint, 0, len(ids)), false
		for _, x := range ids {
			if x == nodeID {
				hit = true
				continue
			}
			kept = append(kept, x)
		}
		if !hit {
			continue
		}
		if len(kept) == 0 {
			if err = tx.Delete(&model.FwRule{}, r.Id).Error; err != nil {
				return removed, err
			}
			removed++
			continue
		}
		b, _ := json.Marshal(kept)
		if err = tx.Model(&model.FwRule{}).Where("id = ?", r.Id).Update("node_ids", json.RawMessage(b)).Error; err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// handleFwServer 一台服务器上的动作:本机直接做,副机经 agent/fw 转过去。返回这台做完之后的状况和端口。
func (s *Server) handleFwServer(w http.ResponseWriter, r *http.Request, n model.Node, op string) {
	if op != "state" && op != "open" && op != "undo" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "路径无效"})
		return
	}
	if s.run == nil {
		badRequest(w, fmt.Errorf("数据面未运行"))
		return
	}
	var rep hub.FwReply
	if n.IsLocal {
		rep = s.fwDo(op)
	} else {
		if !n.Enabled {
			badRequest(w, fmt.Errorf("服务器已停用"))
			return
		}
		if s.run.Hub() == nil {
			badRequest(w, fmt.Errorf("数据面未运行"))
			return
		}
		got, err := s.run.Hub().Fw(n, op)
		if err != nil {
			if hub.IsNotFound(err) {
				badRequest(w, fmt.Errorf("这台服务器的版本太旧,升级后才有防火墙功能"))
				return
			}
			badRequest(w, fmt.Errorf("联系不上这台服务器: %w", err))
			return
		}
		rep = got
		s.run.Hub().NoteFw(n.Id, rep.State)
	}
	if op != "state" {
		s.audit(r, "fw", "server:"+op, map[string]interface{}{"name": n.Name, "error": rep.Error})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": n.Id, "state": rep.State, "ports": rep.Ports, "error": rep.Error})
}

// fwDo 在本机做一件防火墙的事(主机自己那台,或者副机收到主机转来的)。
func (s *Server) fwDo(op string) hub.FwReply {
	var st model.FwState
	var err error
	switch op {
	case "open":
		st, err = s.run.FwOpen()
	case "undo":
		st, err = s.run.FwUndo()
	default:
		st = s.run.FwState(true)
	}
	rep := hub.FwReply{State: st, Ports: s.run.FwPorts()}
	if err != nil {
		rep.Error = err.Error()
	}
	return rep
}

// handleAgentFw 副机:主机的防火墙页让本机核对状态 / 放开系统防火墙 / 撤销 / 放行端口。
func (s *Server) handleAgentFw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req struct {
		Op string `json:"op"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if req.Op != "state" && req.Op != "open" && req.Op != "undo" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if s.run == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "数据面未运行"})
		return
	}
	writeJSON(w, http.StatusOK, s.fwDo(req.Op))
}

// reportFw 随报告带给主机的防火墙状况:用缓存的那份,不为每 5 秒一次的报告去跑系统命令。刚启动、第一轮还没量完时不带。
func (s *Server) reportFw() *model.FwState {
	if s.run == nil {
		return nil
	}
	st := s.run.FwState(false)
	if st.At == 0 {
		return nil
	}
	return &st
}
