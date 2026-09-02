package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/logger"

	"github.com/skip2/go-qrcode"
	"gorm.io/gorm"
)

// ---- 审计 ----

// audit 记录一次面板操作(谁、对什么、做了什么)。
func (s *Server) audit(r *http.Request, key, action string, obj interface{}) {
	b, _ := json.Marshal(obj)
	actor := s.actor(r)
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

	q := s.db.Model(&model.Stats{}).Where("resource = ? AND date_time > ? AND date_time <= ?", dbResource, start, end)
	if tag != "" {
		q = q.Where("tag = ?", tag)
	}
	var rows []model.Stats
	q.Order("date_time asc").Find(&rows)

	// 目标最多 240 个点;桶宽不小于落库桶
	numBuckets := 240
	bucketSeconds := s.settingInt("statsBucketSeconds", 60)
	if bucketSeconds < 1 {
		bucketSeconds = 60
	}
	if maxB := int((end - start) / int64(bucketSeconds)); maxB < numBuckets {
		numBuckets = maxB
	}
	if numBuckets < 1 {
		numBuckets = 1
	}
	span := (end - start) / int64(numBuckets)
	if span == 0 {
		span = 1
	}
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

// ---- 日志 ----

// handleLogs 返回面板与内嵌 sing-box 的最近日志(内存环形缓冲)。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	count := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("count")); err == nil && v > 0 && v <= 2000 {
		count = v
	}
	level := r.URL.Query().Get("level")
	if level == "" {
		level = "info"
	}
	writeJSON(w, http.StatusOK, logger.GetLogs(count, level))
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
	host := s.setting("subDomain")
	if host == "" {
		host = s.setting("webDomain")
	}
	port := s.setting("subPort")
	path := s.setting("subPath")
	if path == "" {
		path = "/sub/"
	}
	base := fmt.Sprintf("%s://%s:%s%s%s", scheme, host, port, path, u.Name)
	return map[string]string{"link": base, "clash": base + "?format=clash"}
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
