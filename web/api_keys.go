package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/stats"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ---- 外部 API:多把密钥、按密钥分权限、来源 IP 白名单 ----
//
// 原来每个面板(主面板、每个代理)只有一把令牌,能做全部操作。商城只要建号续费、Telegram 机器人只要查看,
// 给谁都是整把钥匙。现在:
//   - 原来那一把仍然有效,界面上叫「默认密钥」,全部权限(升级后对接程序不用改,回退到旧版也照样能用);
//   - 另外可以生成多把附加密钥(model.ApiKey),各自起名、各自勾权限、可单独停用 / 重新生成 / 删除,
//     列表里看得到每把最近一次是什么时候、从哪个 IP 用的;
//   - 来源 IP 白名单:主面板与每个代理各一份,默认关;打开后只有名单里的 IP / 网段能调用(对这个面板的全部密钥生效)。
//     来源按 sourceIP 认:挂在反向代理后面时先在 设置 → 面板 填「受信任的反向代理」。
// 外部 API 总开关(apiEnabled / 代理的 ApiEnabled)关着时,所有密钥都不能用。

// 权限名。接口与权限的对应见 apiPermFor。
const (
	permUsersRead   = "users.read"   // 查看用户:列表、详情、订阅地址、套餐记录、用量曲线
	permUsersCreate = "users.create" // 创建用户
	permUsersUpdate = "users.update" // 修改用户:改字段、启停、重置流量、踢线、重置订阅链接
	permUsersDelete = "users.delete" // 删除用户
	permBilling     = "billing"      // 续费 / 延期 / 换套餐
	permPlansRead   = "plans.read"   // 查看套餐
	permPanelRead   = "panel.read"   // 面板信息:概况、服务器、线路、在线用户
)

// apiPerms 全部权限,界面按这个顺序列。
var apiPerms = []string{permUsersRead, permUsersCreate, permUsersUpdate, permUsersDelete, permBilling, permPlansRead, permPanelRead}

// normPerms 规整一组权限名:去重、按 apiPerms 的顺序,认不出的报错。
func normPerms(in []string) ([]string, error) {
	want := map[string]bool{}
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		known := false
		for _, k := range apiPerms {
			if k == p {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("不认识的权限 %q", p)
		}
		want[p] = true
	}
	out := make([]string, 0, len(want))
	for _, k := range apiPerms {
		if want[k] {
			out = append(out, k)
		}
	}
	return out, nil
}

func permSet(csv string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}

// apiPermFor 一次调用需要的权限;"" = 只要密钥有效(ping)。parts 是 /v1/ 后面按 / 切开的路径。
func apiPermFor(method string, parts []string) string {
	read := method == http.MethodGet || method == http.MethodHead
	switch {
	case len(parts) == 1 && parts[0] == "ping":
		return ""
	case len(parts) == 1 && parts[0] == "plans":
		return permPlansRead
	case len(parts) == 1 && (parts[0] == "overview" || parts[0] == "servers" || parts[0] == "lines" || parts[0] == "onlines"):
		return permPanelRead
	case len(parts) == 1 && parts[0] == "users":
		if read {
			return permUsersRead
		}
		return permUsersCreate
	case len(parts) == 2 && parts[0] == "users":
		switch {
		case read:
			return permUsersRead
		case method == http.MethodDelete:
			return permUsersDelete
		}
		return permUsersUpdate
	case len(parts) == 3 && parts[0] == "users":
		switch parts[2] {
		case "sub", "plan-ops", "traffic":
			return permUsersRead
		case "plan", "renew", "change":
			return permBilling
		}
		return permUsersUpdate
	}
	return ""
}

// ---- 来源 IP 白名单 ----

// parseIPList 解析白名单:逗号、空格、换行分隔的 IP 或网段;返回规范化后的写法(逗号分隔)。
func parseIPList(v string) ([]*net.IPNet, string, error) {
	var nets []*net.IPNet
	var norm []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '，'
	}) {
		var n *net.IPNet
		if strings.Contains(f, "/") {
			_, c, err := net.ParseCIDR(f)
			if err != nil {
				return nil, "", fmt.Errorf("白名单里 %q 不是有效的 IP 或网段", f)
			}
			n = c
		} else {
			ip := net.ParseIP(strings.Trim(f, "[]"))
			if ip == nil {
				return nil, "", fmt.Errorf("白名单里 %q 不是有效的 IP 或网段", f)
			}
			bits := 128
			if ip4 := ip.To4(); ip4 != nil {
				ip, bits = ip4, 32
			}
			n = &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
		}
		s := n.String()
		if ones, bits := n.Mask.Size(); ones == bits {
			s = n.IP.String()
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		nets = append(nets, n)
		norm = append(norm, s)
	}
	if len(norm) > 200 {
		return nil, "", errors.New("白名单最多 200 项")
	}
	return nets, strings.Join(norm, ", "), nil
}

// ipAllowed 来源 ip 在不在名单里(名单写坏了按不在处理:宁可挡住也不放开)。
func ipAllowed(src, list string) bool {
	ip := net.ParseIP(src)
	if ip == nil {
		return false
	}
	nets, _, err := parseIPList(list)
	if err != nil {
		return false
	}
	return ipInNets(ip, nets)
}

// apiIPCheck 主面板(rid 0)或代理 rid 的白名单:没开放行;开了只放名单里的来源。
func (s *Server) apiIPCheck(r *http.Request, rid uint) bool {
	on, list := s.setting("apiIpAllow") == "true", s.setting("apiIpList")
	if rid > 0 {
		var rs model.Reseller
		if s.db.Select("id, api_ip_allow, api_ip_list").First(&rs, rid).Error != nil {
			return false
		}
		on, list = rs.ApiIpAllow, rs.ApiIpList
	}
	return !on || ipAllowed(s.sourceIP(r), list)
}

func writeIPDenied(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "来源 IP 不在外部 API 的白名单里"})
}

// ---- 鉴权 ----

// apiKeyByToken 按令牌找附加密钥(rid 0 = 主面板的,>0 = 代理的;代理入口不认主面板的,反之亦然)。
func (s *Server) apiKeyByToken(tok string, reseller bool) (model.ApiKey, bool) {
	var k model.ApiKey
	if tok == "" {
		return k, false
	}
	q := s.db.Where("token = ?", tok)
	if reseller {
		q = q.Where("reseller_id > 0")
	} else {
		q = q.Where("reseller_id = 0")
	}
	if q.First(&k).Error != nil || subtle.ConstantTimeCompare([]byte(tok), []byte(k.Token)) != 1 {
		return k, false
	}
	return k, true
}

// keyUse 记附加密钥最近一次使用(同一把一分钟最多写一次库,别让高频调用变成高频写)。
var keyUse = struct {
	sync.Mutex
	at map[uint]int64
}{at: map[uint]int64{}}

func (s *Server) noteKeyUse(id uint, ip string) {
	now := time.Now().Unix()
	keyUse.Lock()
	last := keyUse.at[id]
	if now-last < 60 {
		keyUse.Unlock()
		return
	}
	keyUse.at[id] = now
	keyUse.Unlock()
	s.db.Model(&model.ApiKey{}).Where("id = ?", id).Updates(map[string]interface{}{"last_used_at": now, "last_used_ip": ip})
}

// adminAPIAuth 主面板入口:默认密钥(设置 apiToken)或主面板的附加密钥。known 同 publicAuth:对得上某把密钥(只是关着)的不算猜令牌。
func (s *Server) adminAPIAuth(r *http.Request) (sc apiScope, ok, known bool) {
	if ok, known := s.publicAuth(r); known {
		return apiScope{actor: "api"}, ok, true
	}
	k, found := s.apiKeyByToken(apiToken(r), false)
	if !found {
		return sc, false, false
	}
	if !k.Enabled || s.setting("apiEnabled") != "true" {
		return sc, false, true
	}
	return apiScope{actor: "api:" + k.Name, perms: permSet(k.Perms), keyId: k.Id}, true, true
}

// resellerAPIAuth 代理入口:代理的默认密钥(ApiToken)或这个代理的附加密钥;代理停用、到期、关了外部 API 时都不行。
func (s *Server) resellerAPIAuth(r *http.Request) (rs model.Reseller, sc apiScope, ok, known bool) {
	tok := apiToken(r)
	if rs, ok, known := s.resellerAPIToken(tok); known {
		return rs, apiScope{rid: rs.Id, actor: "api:" + rs.Name}, ok, true
	}
	k, found := s.apiKeyByToken(tok, true)
	if !found {
		return rs, sc, false, false
	}
	if s.db.First(&rs, k.ResellerId).Error != nil {
		return rs, sc, false, true
	}
	if !k.Enabled || !rs.ApiEnabled || !rs.Enabled || (rs.Expiry > 0 && rs.Expiry < time.Now().Unix()) {
		return rs, sc, false, true
	}
	return rs, apiScope{rid: rs.Id, actor: "api:" + rs.Name + "/" + k.Name, perms: permSet(k.Perms), keyId: k.Id}, true, true
}

// allowed 这次调用的密钥有没有这个权限(默认密钥 perms 为 nil = 全部)。
func (sc apiScope) allowed(perm string) bool {
	return perm == "" || sc.perms == nil || sc.perms[perm]
}

// ---- 管理附加密钥(主面板「管理员」页与代理「我的账号」页共用) ----

type apiKeyView struct {
	model.ApiKey
	PermList []string `json:"permList"`
}

func viewKey(k model.ApiKey) apiKeyView {
	out := apiKeyView{ApiKey: k, PermList: []string{}}
	for _, p := range apiPerms {
		if permSet(k.Perms)[p] {
			out.PermList = append(out.PermList, p)
		}
	}
	return out
}

// handleAPIKeys 分发 …/api/keys[/id[/rotate]] 与 …/api/ip。rid 0 = 主面板,>0 = 代理自己。
// sub 是 "keys"、"keys/3"、"keys/3/rotate" 或 "ip";audit 记审计。
func (s *Server) handleAPIKeys(w http.ResponseWriter, r *http.Request, rid uint, sub string, audit func(action string, obj interface{})) {
	parts := strings.Split(strings.Trim(sub, "/"), "/")
	if parts[0] == "ip" {
		s.handleAPIIP(w, r, rid, audit)
		return
	}
	if parts[0] != "keys" || len(parts) > 3 {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			var keys []model.ApiKey
			s.db.Where("reseller_id = ?", rid).Order("id asc").Find(&keys)
			out := make([]apiKeyView, 0, len(keys))
			for _, k := range keys {
				out = append(out, viewKey(k))
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"keys": out, "perms": apiPerms})
		case http.MethodPost:
			k, err := s.readKeyBody(r, model.ApiKey{ResellerId: rid, Enabled: true})
			if err != nil {
				badRequest(w, err)
				return
			}
			var n int64
			s.db.Model(&model.ApiKey{}).Where("reseller_id = ?", rid).Count(&n)
			if n >= 50 {
				badRequest(w, errors.New("附加密钥最多 50 把"))
				return
			}
			k.Token, k.CreatedAt = newAPIKeyToken(), time.Now().Unix()
			if err := s.db.Create(&k).Error; err != nil {
				badRequest(w, err)
				return
			}
			// gorm 对 bool 零值走列默认值:建的时候要关着就补一笔(这张表没设默认值,这里只是保险)
			s.db.Model(&model.ApiKey{}).Where("id = ?", k.Id).Update("enabled", k.Enabled)
			audit("api-key:create", map[string]interface{}{"id": k.Id, "name": k.Name, "perms": k.Perms})
			writeJSON(w, http.StatusOK, viewKey(k))
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		}
		return
	}
	id, _ := strconv.ParseUint(parts[1], 10, 64)
	var k model.ApiKey
	if id == 0 || s.db.Where("id = ? AND reseller_id = ?", id, rid).First(&k).Error != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "密钥不存在"})
		return
	}
	if len(parts) == 3 {
		if parts[2] != "rotate" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		k.Token = newAPIKeyToken()
		if err := s.db.Model(&model.ApiKey{}).Where("id = ?", k.Id).Update("token", k.Token).Error; err != nil {
			badRequest(w, err)
			return
		}
		audit("api-key:rotate", map[string]interface{}{"id": k.Id, "name": k.Name})
		writeJSON(w, http.StatusOK, viewKey(k))
		return
	}
	switch r.Method {
	case http.MethodPut, http.MethodPatch:
		nk, err := s.readKeyBody(r, k)
		if err != nil {
			badRequest(w, err)
			return
		}
		s.db.Model(&model.ApiKey{}).Where("id = ?", k.Id).Updates(map[string]interface{}{"name": nk.Name, "perms": nk.Perms, "enabled": nk.Enabled})
		audit("api-key:update", map[string]interface{}{"id": k.Id, "name": nk.Name, "perms": nk.Perms, "enabled": nk.Enabled})
		writeJSON(w, http.StatusOK, viewKey(nk))
	case http.MethodDelete:
		s.db.Delete(&model.ApiKey{}, k.Id)
		audit("api-key:delete", map[string]interface{}{"id": k.Id, "name": k.Name})
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// readKeyBody 读 {name, perms, enabled};名字必填、同一面板内不重名,至少一项权限。
func (s *Server) readKeyBody(r *http.Request, base model.ApiKey) (model.ApiKey, error) {
	var p struct {
		Name    *string  `json:"name"`
		Perms   []string `json:"perms"`
		Enabled *bool    `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		return base, errors.New("请求格式不对")
	}
	k := base
	if p.Name != nil {
		k.Name = strings.TrimSpace(*p.Name)
	}
	if k.Name == "" || len([]rune(k.Name)) > 40 {
		return base, errors.New("密钥名称不能为空,最多 40 个字")
	}
	if p.Perms != nil {
		perms, err := normPerms(p.Perms)
		if err != nil {
			return base, err
		}
		k.Perms = strings.Join(perms, ",")
	}
	if k.Perms == "" {
		return base, errors.New("至少勾一项权限")
	}
	if p.Enabled != nil {
		k.Enabled = *p.Enabled
	}
	var dup int64
	s.db.Model(&model.ApiKey{}).Where("reseller_id = ? AND name = ? AND id <> ?", k.ResellerId, k.Name, k.Id).Count(&dup)
	if dup > 0 {
		return base, fmt.Errorf("已经有一把叫 %q 的密钥", k.Name)
	}
	return k, nil
}

// newAPIKeyToken 附加密钥的令牌:32 字节随机数的十六进制(和默认密钥同样长)。
func newAPIKeyToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// handleAPIIP GET / PUT 白名单 {enabled, list}。打开时名单不能空(空名单等于谁都挡)。
func (s *Server) handleAPIIP(w http.ResponseWriter, r *http.Request, rid uint, audit func(action string, obj interface{})) {
	get := func() (bool, string) {
		if rid == 0 {
			return s.setting("apiIpAllow") == "true", s.setting("apiIpList")
		}
		var rs model.Reseller
		s.db.Select("id, api_ip_allow, api_ip_list").First(&rs, rid)
		return rs.ApiIpAllow, rs.ApiIpList
	}
	switch r.Method {
	case http.MethodGet:
		on, list := get()
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": on, "list": list, "you": s.sourceIP(r)})
	case http.MethodPut, http.MethodPost:
		var p struct {
			Enabled bool   `json:"enabled"`
			List    string `json:"list"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, errors.New("请求格式不对"))
			return
		}
		_, norm, err := parseIPList(p.List)
		if err != nil {
			badRequest(w, err)
			return
		}
		if p.Enabled && norm == "" {
			badRequest(w, errors.New("打开白名单前至少填一个来源 IP 或网段"))
			return
		}
		if rid == 0 {
			// 两项一起写(一笔事务):不会出现「开关开了、名单还是旧的」的中间状态
			if err := s.putSettings(map[string]string{"apiIpList": norm, "apiIpAllow": map[bool]string{true: "true", false: "false"}[p.Enabled]}); err != nil {
				badRequest(w, err)
				return
			}
		} else if err := s.db.Model(&model.Reseller{}).Where("id = ?", rid).Updates(map[string]interface{}{"api_ip_allow": p.Enabled, "api_ip_list": norm}).Error; err != nil {
			badRequest(w, err)
			return
		}
		audit("api-ip", map[string]interface{}{"enabled": p.Enabled, "list": norm})
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": p.Enabled, "list": norm, "you": s.sourceIP(r)})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// ---- 面板信息接口(权限 panel.read):给 Telegram 机器人、商城后台看面板状态 ----

// apiOverview GET /overview:用户、在线、流量(今天 / 本月 / 累计按面板时区)、线路;主面板另给服务器数。
func (s *Server) apiOverview(w http.ResponseWriter, sc apiScope) {
	users := s.db.Model(&model.User{})
	if sc.rid > 0 {
		users = users.Where("reseller_id = ?", sc.rid)
	}
	var total, enabled, depleted, expiring int64
	now := time.Now()
	users.Session(&gorm.Session{}).Count(&total)
	users.Session(&gorm.Session{}).Where("enabled = ?", true).Count(&enabled)
	users.Session(&gorm.Session{}).Where("(volume > 0 AND up + down >= volume)").Count(&depleted)
	users.Session(&gorm.Session{}).Where("enabled = ? AND expiry > ? AND expiry <= ?", true, now.Unix(), now.Add(7*24*time.Hour).Unix()).Count(&expiring)
	var allUp, allDown int64
	users.Session(&gorm.Session{}).Select("COALESCE(SUM(up+total_up),0)").Scan(&allUp)
	users.Session(&gorm.Session{}).Select("COALESCE(SUM(down+total_down),0)").Scan(&allDown)
	online := s.liveOnlineUsers()
	names := map[string]bool(nil)
	if sc.rid > 0 {
		names = s.resellerUserNames(sc.rid)
		online = filterNames(online, names)
	}
	loc := s.panelLocation()
	t := now.In(loc)
	dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).Unix()
	monthStart := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc).Unix()
	sum := func(from int64) map[string]int64 {
		q := s.db.Model(&model.Stats{}).Where("resource = ? AND date_time >= ?", "user", from)
		if names != nil {
			list := make([]string, 0, len(names))
			for n := range names {
				list = append(list, n)
			}
			if len(list) == 0 {
				return map[string]int64{"up": 0, "down": 0}
			}
			q = q.Where("tag IN ?", list)
		}
		var rows []struct {
			Direction bool
			Traffic   int64
		}
		q.Select("direction, SUM(traffic) AS traffic").Group("direction").Scan(&rows)
		out := map[string]int64{"up": 0, "down": 0}
		for _, r := range rows {
			if r.Direction {
				out["up"] += r.Traffic
			} else {
				out["down"] += r.Traffic
			}
		}
		return out
	}
	out := map[string]interface{}{
		"version": Version,
		"time":    now.Unix(),
		"users": map[string]interface{}{
			"total": total, "enabled": enabled, "online": len(online), "depleted": depleted, "expiringIn7Days": expiring,
		},
		"traffic": map[string]interface{}{
			"today": sum(dayStart), "month": sum(monthStart), "total": map[string]int64{"up": allUp, "down": allDown},
		},
	}
	if sc.rid == 0 {
		var lines, linesOn, nodes int64
		s.db.Model(&model.Line{}).Count(&lines)
		s.db.Model(&model.Line{}).Where("enabled = ?", true).Count(&linesOn)
		s.db.Model(&model.Node{}).Count(&nodes)
		online := 0
		for _, st := range s.hubStatuses() {
			if st.OK {
				online++
			}
		}
		out["lines"] = map[string]int64{"total": lines, "enabled": linesOn}
		out["servers"] = map[string]int64{"total": nodes, "online": int64(online) + 1} // 本机算在线
		out["coreRunning"] = s.run != nil && s.run.CoreRunning()
	} else {
		var granted int64
		s.db.Model(&model.ResellerLine{}).Where("reseller_id = ?", sc.rid).Count(&granted)
		out["lines"] = map[string]int64{"total": granted}
	}
	writeJSON(w, http.StatusOK, out)
}

// apiServers GET /servers(只有主面板的密钥能看):每台服务器在不在线、版本、在线人数、上次联系时间。
func (s *Server) apiServers(w http.ResponseWriter, sc apiScope) {
	if sc.rid > 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "代理的密钥看不了服务器"})
		return
	}
	var nodes []model.Node
	s.db.Order("sort asc, id asc").Find(&nodes)
	sts := s.hubStatuses()
	out := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		v := map[string]interface{}{"id": n.Id, "name": n.Name, "enabled": n.Enabled, "ratio": n.Ratio, "isLocal": n.IsLocal}
		if n.IsLocal {
			v["online"], v["version"], v["coreRunning"] = true, Version, s.run != nil && s.run.CoreRunning()
			v["onlineUsers"] = len(s.localOnlineUsers())
		} else if st, ok := sts[n.Id]; ok {
			v["online"], v["version"], v["coreRunning"], v["onlineUsers"], v["lastSeen"] = st.OK, st.Version, st.CoreRunning, st.OnlineUsers, st.LastSeen
			if st.Error != "" {
				v["error"] = st.Error
			}
		} else {
			v["online"] = false
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// apiLines GET /lines:线路、部署在哪几台、此刻在线人数;代理只看授权给自己的线路(不给部署细节)。
func (s *Server) apiLines(w http.ResponseWriter, sc apiScope) {
	var lines []model.Line
	q := s.db.Order("sort asc, id asc")
	if sc.rid > 0 {
		q = q.Where("id IN (SELECT line_id FROM reseller_lines WHERE reseller_id = ?)", sc.rid)
	}
	q.Find(&lines)
	online := s.lineOnlineUsers()
	var mine map[string]bool
	if sc.rid > 0 {
		mine = s.resellerUserNames(sc.rid)
	}
	out := make([]map[string]interface{}, 0, len(lines))
	for _, l := range lines {
		users := online[l.Name]
		if mine != nil {
			users = filterNames(users, mine)
		}
		v := map[string]interface{}{"id": l.Id, "name": l.Name, "protocol": l.Protocol, "enabled": l.Enabled, "onlineUsers": len(users)}
		if sc.rid == 0 {
			v["port"] = l.Port
			v["servers"] = s.lineServerNames(l)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// apiOnlines GET /onlines:此刻在线的用户、设备数(IPv4 一个地址一台、IPv6 按 /64)与所在服务器。
func (s *Server) apiOnlines(w http.ResponseWriter, sc apiScope) {
	users := s.liveOnlineUsers()
	if sc.rid > 0 {
		users = filterNames(users, s.resellerUserNames(sc.rid))
	}
	if s.run == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	ips := s.onlineIPsAll()
	localName := s.localNodeName()
	localLines, remoteLines := s.run.OnlineIPLines(), s.run.Hub().RemoteIPLinesAll()
	sort.Strings(users)
	out := make([]map[string]interface{}, 0, len(users))
	for _, u := range users {
		where := map[string]bool{}
		for _, ls := range s.onlineLines(u, localName, localLines, remoteLines) {
			for _, l := range ls {
				where[l] = true
			}
		}
		lines := make([]string, 0, len(where))
		for l := range where {
			lines = append(lines, l)
		}
		sort.Strings(lines)
		out = append(out, map[string]interface{}{"user": u, "devices": model.DeviceCount(ips[u]), "lines": lines})
	}
	writeJSON(w, http.StatusOK, out)
}

// apiUserTraffic GET /users/{x}/traffic?days=30:按面板时区每天一格的上下行(最多 90 天)。
func (s *Server) apiUserTraffic(w http.ResponseWriter, r *http.Request, u model.User) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 30
	}
	if days > 90 {
		days = 90
	}
	res := stats.Series(s.db, "user", u.Name, days*24, 86400, s.settingInt("statsBucketSeconds", 60), s.panelLocation())
	type day struct {
		Date string `json:"date"`
		Up   int64  `json:"up"`
		Down int64  `json:"down"`
	}
	out := make([]day, 0, len(res.Points))
	loc := s.panelLocation()
	for _, p := range res.Points {
		out = append(out, day{Date: time.Unix(p.T, 0).In(loc).Format("2006-01-02"), Up: p.Up, Down: p.Down})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"user": u.Name, "days": out, "totalUp": res.TotalUp, "totalDown": res.TotalDown})
}

// lineOnlineUsers 线路名 → 此刻在这条线路上在线的用户(本机加各副机上报的,去重)。
func (s *Server) lineOnlineUsers() map[string][]string {
	seen := map[string]map[string]bool{}
	add := func(m map[string]map[string][]string) {
		for user, ips := range m {
			for _, lines := range ips {
				for _, l := range lines {
					if seen[l] == nil {
						seen[l] = map[string]bool{}
					}
					seen[l][user] = true
				}
			}
		}
	}
	if s.run != nil {
		add(s.run.OnlineIPLines())
		for _, m := range s.run.Hub().RemoteIPLinesRaw() {
			add(m)
		}
	}
	out := make(map[string][]string, len(seen))
	for l, us := range seen {
		for u := range us {
			out[l] = append(out[l], u)
		}
		sort.Strings(out[l])
	}
	return out
}

// lineServerNames 线路部署在哪几台服务器(名字;没限定 = 全部)。
func (s *Server) lineServerNames(l model.Line) []string {
	var nodes []model.Node
	s.db.Order("sort asc, id asc").Find(&nodes)
	out := []string{}
	for _, n := range nodes {
		if render.LineOnNode(l, n.Id) {
			out = append(out, n.Name)
		}
	}
	return out
}

// permTitle 权限的中文名(403 的错误信息里用;界面上的名字在 i18n 里)。
func permTitle(p string) string {
	return map[string]string{
		permUsersRead: "查看用户", permUsersCreate: "创建用户", permUsersUpdate: "修改用户", permUsersDelete: "删除用户",
		permBilling: "续费与换套餐", permPlansRead: "查看套餐", permPanelRead: "面板信息",
	}[p]
}

// ---- 运行时数据(测试里没有 runner 时当作没人在线、没有副机状态) ----

func (s *Server) localOnlineUsers() []string {
	if s.run == nil {
		return nil
	}
	return s.run.Onlines().Users
}

func (s *Server) liveOnlineUsers() []string {
	if s.run == nil {
		return []string{}
	}
	return mergeIPs(s.run.Onlines().Users, s.run.Hub().RemoteOnlineUsers())
}

func (s *Server) hubStatuses() map[uint]hub.NodeStatus {
	if s.run == nil {
		return map[uint]hub.NodeStatus{}
	}
	return s.run.Hub().Statuses()
}

// putSettings 一笔事务写几项设置(有 runner 时走它,测试里直接写库)。
func (s *Server) putSettings(kv map[string]string) error {
	if s.run != nil {
		return s.run.SetSettings(kv)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		for k, v := range kv {
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&model.Setting{Key: k, Value: v}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
