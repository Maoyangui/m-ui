package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"

	"github.com/skip2/go-qrcode"
	"gorm.io/gorm"
)

// ---- 审计 ----

// audit 记录一次面板操作(谁、对什么、做了什么)。
func (s *Server) audit(r *http.Request, key, action string, obj interface{}) {
	s.auditAs(s.actor(r), key, action, obj)
}

// auditAs 以指定操作人记录(外部 API 调用记为 api)。
func (s *Server) auditAs(actor, key, action string, obj interface{}) {
	b, _ := json.Marshal(obj)
	if actor == "" {
		actor = "?"
	}
	s.db.Create(&model.Change{DateTime: time.Now().Unix(), Actor: actor, Key: key, Action: action, Obj: b})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	q := s.db.Model(&model.Change{}).Order("id desc").Limit(limit)
	if key := r.URL.Query().Get("key"); key != "" {
		q = q.Where("key = ?", key)
	}
	var rows []model.Change
	q.Find(&rows)
	writeJSON(w, http.StatusOK, rows)
}

// ---- 流量时序 ----

// handleStats 返回某对象(或某类对象合计)最近 N 小时的上/下行时序,按桶下采样。
//
//	GET /api/stats?resource=user|line|upstream[&tag=名称]&hours=24
//
// tag 为空时把该类全部对象合计,用于概览总流量。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	resource := r.URL.Query().Get("resource")
	dbResource := map[string]string{"user": "user", "line": "inbound", "upstream": "outbound"}[resource]
	if dbResource == "" {
		badRequest(w, fmt.Errorf("resource 需为 user/line/upstream"))
		return
	}
	tag := r.URL.Query().Get("tag")
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 24*90 {
		hours = 24
	}
	end := time.Now().Unix()
	start := end - int64(hours)*3600

	var rows []model.Stats

	// 桶宽:前端可指定 bucket(秒,如 300/3600/21600/86400)得到整齐的时段柱;
	// 未指定时最多 240 个点,桶宽不小于落库桶
	bucketSeconds := s.settingInt("statsBucketSeconds", 60)
	if bucketSeconds < 1 {
		bucketSeconds = 60
	}
	var span int64
	var numBuckets int
	if b, _ := strconv.Atoi(r.URL.Query().Get("bucket")); b >= bucketSeconds && b <= 7*86400 {
		span = int64(b)
		// 起点对齐到桶边界:按查看者时区(tz=分钟偏移,浏览器传入;缺省用服务器时区),
		// 日桶对齐到零点,小时级桶对齐到当天内的整数倍(6h 桶 → 0/6/12/18 点),更细的桶按 UTC 取整
		loc := s.panelLocation()
		if tz, err := strconv.Atoi(r.URL.Query().Get("tz")); err == nil && tz >= -14*60 && tz <= 14*60 {
			loc = time.FixedZone("viewer", tz*60)
		}
		st := time.Unix(start, 0).In(loc)
		var aligned time.Time
		switch {
		case span >= 86400:
			aligned = time.Date(st.Year(), st.Month(), st.Day(), 0, 0, 0, 0, loc)
		case span >= 3600:
			secOfDay := int64(st.Hour())*3600 + int64(st.Minute())*60 + int64(st.Second())
			midnight := time.Date(st.Year(), st.Month(), st.Day(), 0, 0, 0, 0, loc)
			aligned = midnight.Add(time.Duration(secOfDay-secOfDay%span) * time.Second)
		default:
			aligned = time.Unix(start-start%span, 0)
		}
		start = aligned.Unix()
		numBuckets = int((end-start)/span) + 1
	} else {
		numBuckets = 240
		if maxB := int((end - start) / int64(bucketSeconds)); maxB < numBuckets {
			numBuckets = maxB
		}
		if numBuckets < 1 {
			numBuckets = 1
		}
		span = (end - start) / int64(numBuckets)
		if span == 0 {
			span = 1
		}
	}
	q := s.db.Model(&model.Stats{}).Where("resource = ? AND date_time > ? AND date_time <= ?", dbResource, start, end)
	if tag != "" {
		q = q.Where("tag = ?", tag)
	}
	q.Order("date_time asc").Find(&rows)
	type point struct {
		T    int64 `json:"t"`
		Up   int64 `json:"up"`
		Down int64 `json:"down"`
	}
	points := make([]point, numBuckets)
	for i := range points {
		points[i].T = start + int64(i)*span
	}
	var totalUp, totalDown int64
	for _, row := range rows {
		i := int((row.DateTime - start) / span)
		if i < 0 {
			i = 0
		}
		if i >= numBuckets {
			i = numBuckets - 1
		}
		if row.Direction {
			points[i].Up += row.Traffic
			totalUp += row.Traffic
		} else {
			points[i].Down += row.Traffic
			totalDown += row.Traffic
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"points": points, "span": span, "start": start, "end": end,
		"totalUp": totalUp, "totalDown": totalDown,
	})
}

// ---- 在线 ----

func (s *Server) handleOnlines(w http.ResponseWriter, r *http.Request) {
	o := s.run.Onlines()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"users": mergeIPs(o.Users, s.run.Hub().RemoteOnlineUsers()), "lines": o.Lines, "upstreams": o.Upstreams,
		"connCounts": s.run.ConnCounts(),
	})
}

// mergeIPs 合并两组字符串并去重(顺序:a 在前)。
func mergeIPs(a, b []string) []string {
	if len(b) == 0 {
		if a == nil {
			return []string{}
		}
		return a
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// handleStatsTop GET /stats/top?hours=24&limit=10:按用户聚合的流量排行。
func (s *Server) handleStatsTop(w http.ResponseWriter, r *http.Request) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 24*90 {
		hours = 24
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	since := time.Now().Unix() - int64(hours)*3600
	type row struct {
		Tag  string `json:"name"`
		Up   int64  `json:"up"`
		Down int64  `json:"down"`
	}
	var rows []row
	s.db.Raw(`SELECT tag, COALESCE(SUM(CASE WHEN direction = 1 THEN traffic ELSE 0 END),0) AS up,
		COALESCE(SUM(CASE WHEN direction = 0 THEN traffic ELSE 0 END),0) AS down
		FROM stats WHERE resource = 'user' AND date_time > ? GROUP BY tag ORDER BY (up + down) DESC LIMIT ?`, since, limit).Scan(&rows)
	if rows == nil {
		rows = []row{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// ---- 最近入站连接(从 sing-box 日志提取,客户端"连不上"时用来判断包有没有到服务器)----

var reInboundConn = regexp.MustCompile(`inbound/(\w+)\[([^\]]+)\]\s*inbound connection from ([0-9a-fA-F.:\[\]]+?)(?::\d+)?\s*$`)

// 认证成功后紧跟的一条日志带用户名:inbound/hysteria2[香港1] [alice] inbound connection to example.com:443
var reInboundUser = regexp.MustCompile(`inbound/\w+\[([^\]]+)\]\s*\[([^\]]+)\]\s*inbound connection to`)
var reLogTime = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)

// logTime 把日志行首的时间按本机时区解析成 unix 秒:各机时区可能不同,统一成绝对时间,面板才能按面板时区显示。
func logTime(line string) int64 {
	m := reLogTime.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	t, err := time.ParseInLocation("2006/01/02 15:04:05", m[1], time.Local)
	if err != nil {
		return 0
	}
	return t.Unix()
}

type recentConn = hub.RecentConn

// recentConns 从本机数据面日志聚合最近入站连接(最近的在前,最多 limit 条)。
func (s *Server) recentConns(limit int) []recentConn {
	lines := logger.GetLogs(3000, "info")
	agg := map[string]*recentConn{}
	var order []string
	lastKeyOfLine := map[string]string{} // 线路 → 最近一条"来自 IP"的键,用来把随后那条带 [用户] 的日志归到它名下
	// GetLogs 返回倒序(新的在前),按时间正序遍历才能把 from → [用户] to 这一对接上
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if m := reInboundConn.FindStringSubmatch(l); m != nil {
			ip := strings.Trim(m[3], "[]")
			key := ip + "|" + m[2]
			c := agg[key]
			if c == nil {
				c = &recentConn{IP: ip, Line: m[2], Protocol: m[1]}
				agg[key] = c
				order = append(order, key)
			}
			c.Count++
			if ts := logTime(l); ts > 0 {
				c.Ts = ts
			}
			lastKeyOfLine[m[2]] = key
			continue
		}
		if m := reInboundUser.FindStringSubmatch(l); m != nil {
			if c := agg[lastKeyOfLine[m[1]]]; c != nil {
				c.User = m[2]
			}
		}
	}
	out := make([]recentConn, 0, len(agg))
	for i := len(order) - 1; i >= 0 && len(out) < limit; i-- { // 最近的在前
		out = append(out, *agg[order[i]])
	}
	return out
}

// handleRecentConns 本机 + 各副机最近入站连接;多服务器时带服务器名,按最近时间排序。
func (s *Server) handleRecentConns(w http.ResponseWriter, r *http.Request) {
	out := s.recentConns(100)
	remote := s.run.Hub().RemoteConns()
	if len(remote) > 0 {
		var local model.Node
		s.db.Where("is_local = ?", true).First(&local)
		for i := range out {
			out[i].Server = local.Name
		}
		out = append(out, remote...)
		sort.SliceStable(out, func(i, j int) bool { return out[i].Ts > out[j].Ts })
		if len(out) > 100 {
			out = out[:100]
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- 日志 ----

// handleLogs 面板与内嵌 sing-box 的最近日志(内存环形缓冲):
// GET 读取;POST {enabled} 打开/关闭记录(数据面 log 随之启停);DELETE ?kind=core|sub|audit 清空对应日志。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var in struct {
			Enabled string `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			badRequest(w, err)
			return
		}
		on := in.Enabled != "false"
		s.run.SetSetting("logEnabled", strconv.FormatBool(on))
		logger.SetEnabled(on)
		s.audit(r, "logs", map[bool]string{true: "enable", false: "disable"}[on], nil)
		s.reloadAll("日志开关") // 数据面 log.disabled 随之变化
		writeJSON(w, http.StatusOK, map[string]bool{"enabled": on})
		return
	case http.MethodDelete:
		kind := r.URL.Query().Get("kind")
		switch kind {
		case "core":
			logger.Clear()
		case "sub":
			s.db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.SubLog{})
		case "audit":
			s.db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.Change{})
		default:
			badRequest(w, fmt.Errorf("kind 须为 core / sub / audit"))
			return
		}
		s.audit(r, "logs", "clear:"+kind, nil)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
		return
	}
	count := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("count")); err == nil && v > 0 && v <= 2000 {
		count = v
	}
	level := r.URL.Query().Get("level")
	if level == "" {
		level = "info"
	}
	lines := logger.GetLogs(count, level)
	if lines == nil {
		lines = []string{} // 清空后返回 [] 而不是 null
	}
	writeJSON(w, http.StatusOK, lines)
}

// ---- 用户子操作:reset / kick / qr / sub ----

// dispatchUserSubroute 处理 /users/{id}/reset|kick|qr|sub。返回 true 表示已处理。
func (s *Server) dispatchUserSubroute(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	idx := strings.LastIndex(path, "/users/")
	if idx < 0 {
		return false
	}
	rest := strings.Split(path[idx+len("/users/"):], "/")
	if len(rest) == 1 {
		switch rest[0] {
		case "bulk":
			s.handleUsersBulk(w, r)
			return true
		case "batch":
			s.handleUsersBatch(w, r)
			return true
		case "export":
			s.handleUsersExport(w, r)
			return true
		}
		return false
	}
	if len(rest) != 2 {
		return false
	}
	id, err := strconv.ParseUint(rest[0], 10, 64)
	if err != nil {
		return false
	}
	var u model.User
	if err := s.db.First(&u, uint(id)).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
		return true
	}
	switch rest[1] {
	case "plan":
		if r.Method != http.MethodPost {
			break
		}
		s.handleUserPlan(w, r, u)
		return true
	case "reset":
		if r.Method != http.MethodPost {
			break
		}
		err := s.db.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{
			"total_up": gorm.Expr("total_up + up"), "total_down": gorm.Expr("total_down + down"),
			"up": 0, "down": 0, "enabled": true,
		}).Error
		if err != nil {
			badRequest(w, err)
			return true
		}
		s.audit(r, "user", "reset", u.Name)
		s.reloadUsers("重置用户流量 " + u.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
		return true
	case "kick":
		if r.Method != http.MethodPost {
			break
		}
		n := s.run.KickUser(u.Name)
		s.audit(r, "user", "kick", u.Name)
		writeJSON(w, http.StatusOK, map[string]int{"closed": n})
		return true
	case "sub":
		writeJSON(w, http.StatusOK, s.subLinks(u))
		return true
	case "qr":
		links := s.subLinks(u)
		target := links["clash"]
		if r.URL.Query().Get("format") == "link" {
			target = links["link"]
		}
		png, err := qrcode.Encode(target, qrcode.Medium, 256)
		if err != nil {
			badRequest(w, err)
			return true
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(png)
		return true
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	return true
}

// subLinks 组装用户的订阅地址(链接格式与 clash 格式)。
func (s *Server) subLinks(u model.User) map[string]string {
	scheme := "http"
	if s.setting("subCertFile") != "" && s.setting("subKeyFile") != "" {
		scheme = "https"
	}
	host := s.run.PublicHost() // 订阅域名 → 面板域名 → 公网 IP(无域名自签场景就是 IP:端口)
	if host == "" {
		host = "<服务器IP或域名>"
	}
	base := fmt.Sprintf("%s://%s:%d%s%s", scheme, host, s.settingInt("subPort", 2056), s.subPath(), u.Name)
	return map[string]string{"link": base, "clash": base + "?format=clash", "json": base + "?format=json"}
}

// subPath 订阅路径,始终以 / 开头结尾(默认 /sub/)。
func (s *Server) subPath() string {
	p := strings.TrimSpace(s.setting("subPath"))
	if p == "" {
		p = "/sub/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// ---- 线路子操作:toggle ----

// dispatchLineSubroute 处理 /lines/{id}/toggle。返回 true 表示已处理。
func (s *Server) dispatchLineSubroute(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if !strings.HasSuffix(path, "/toggle") {
		return false
	}
	idx := strings.LastIndex(path, "/lines/")
	if idx < 0 {
		return false
	}
	id, err := strconv.ParseUint(strings.TrimSuffix(path[idx+len("/lines/"):], "/toggle"), 10, 64)
	if err != nil {
		return false
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return true
	}
	var line model.Line
	if err := s.db.First(&line, uint(id)).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "线路不存在"})
		return true
	}
	if err := s.db.Model(&model.Line{}).Where("id = ?", line.Id).Update("enabled", !line.Enabled).Error; err != nil {
		badRequest(w, err)
		return true
	}
	action := "enable"
	if line.Enabled {
		action = "disable"
	}
	s.audit(r, "line", action, line.Name)
	s.reloadAll(action + " 线路 " + line.Name)
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": !line.Enabled})
	return true
}
