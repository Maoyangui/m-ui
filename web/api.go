package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fangjunsheng555/m-ui/creds"
	"github.com/fangjunsheng555/m-ui/database"
	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/logger"
	"github.com/fangjunsheng555/m-ui/core"
	"github.com/fangjunsheng555/m-ui/render"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"gorm.io/gorm"
)

const credAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomString 生成指定长度的随机口令(密码学安全)。
func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	for i := range b {
		b[i] = credAlphabet[int(b[i])%len(credAlphabet)]
	}
	return string(b)
}

// randomBase64 生成 n 字节随机数据的 base64(shadowsocks 密码要求)。
func randomBase64(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// pathID 取出 /api/xxx/<id> 里的 id。
func pathID(r *http.Request, prefix string) (uint, error) {
	idx := strings.Index(r.URL.Path, prefix)
	if idx < 0 {
		return 0, errors.New("路径无效")
	}
	rest := r.URL.Path[idx+len(prefix):]
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return 0, errors.New("缺少 id")
	}
	id, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("id 无效: %s", rest)
	}
	return uint(id), nil
}

// ---- 状态 ----

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var lineCount, upstreamCount, userCount, enabledUsers int64
	s.db.Model(&model.Line{}).Count(&lineCount)
	s.db.Model(&model.Upstream{}).Count(&upstreamCount)
	s.db.Model(&model.User{}).Count(&userCount)
	s.db.Model(&model.User{}).Where("enabled = ?", true).Count(&enabledUsers)

	var totalUp, totalDown int64
	s.db.Model(&model.User{}).Select("COALESCE(SUM(up+total_up),0)").Scan(&totalUp)
	s.db.Model(&model.User{}).Select("COALESCE(SUM(down+total_down),0)").Scan(&totalDown)

	status := map[string]interface{}{
		"role":          s.role(),
		"coreRunning":   s.run.CoreRunning(),
		"uptime":        s.run.Uptime(),
		"lines":         lineCount,
		"upstreams":     upstreamCount,
		"users":         userCount,
		"enabledUsers":  enabledUsers,
		"trafficUp":     totalUp,
		"trafficDown":   totalDown,
		"domain":        s.setting("webDomain"),
		"goroutines":    runtime.NumGoroutine(),
		"version":       Version,
		"onlineUsers":   len(s.run.Onlines().Users),
	}
	// 快速开始清单用
	var nodeCount, planCount int64
	s.db.Model(&model.Node{}).Count(&nodeCount)
	s.db.Model(&model.Plan{}).Count(&planCount)
	ci := s.run.CertInfo()
	status["nodes"] = nodeCount
	status["plans"] = planCount
	status["certExists"] = ci.Exists
	status["certSelfSigned"] = ci.SelfSigned
	status["certDaysLeft"] = ci.DaysLeft
	status["tgEnabled"] = s.run.Notifier().Enabled()
	status["defaultPassword"] = s.setting("adminDefault") == "true"
	status["panelTLS"] = s.setting("webCertFile") != ""
	status["subTLS"] = s.setting("subCertFile") != ""
	status["subPort"] = s.settingInt("subPort", 2056)
	status["subPath"] = s.setting("subPath")
	status["webPort"] = s.settingInt("webPort", 2053)
	status["webPath"] = s.basePath()
	if pct, err := cpu.Percent(0, false); err == nil && len(pct) > 0 {
		status["cpu"] = pct[0]
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		status["memUsed"], status["memTotal"] = vm.Used, vm.Total
	}
	if bt, err := host.BootTime(); err == nil {
		status["bootTime"] = bt
	}
	writeJSON(w, http.StatusOK, status)
}

// role 返回本机角色:master(主/香港) 或 node(副/台湾)。
func (s *Server) role() string {
	if strings.EqualFold(s.setting("nodeMode"), "true") {
		return "node"
	}
	return "master"
}

// ---- 线路 ----

func (s *Server) handleLines(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var lines []model.Line
		s.db.Order("sort asc, id asc").Find(&lines)
		// 附带上游名与用户数,便于前端直接展示
		type row struct {
			model.Line
			UpstreamName string `json:"upstreamName"`
			UserCount    int64  `json:"userCount"`
		}
		out := make([]row, 0, len(lines))
		for _, l := range lines {
			rr := row{Line: l, UpstreamName: "direct"}
			if l.UpstreamId != 0 {
				var name string
				s.db.Model(&model.Upstream{}).Select("name").Where("id = ?", l.UpstreamId).Scan(&name)
				if name != "" {
					rr.UpstreamName = name
				}
			}
			s.db.Model(&model.UserLine{}).Where("line_id = ?", l.Id).Count(&rr.UserCount)
			out = append(out, rr)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var line model.Line
		if err := json.NewDecoder(r.Body).Decode(&line); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.validateLine(&line); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.dryRunLine(&line); err != nil {
			badRequest(w, err)
			return
		}
		line.Id = 0
		if line.Sort == 0 {
			var maxSort int
			s.db.Model(&model.Line{}).Select("COALESCE(MAX(sort),0)").Scan(&maxSort)
			line.Sort = maxSort + 1
		}
		// 在事务里写入并对整套配置做 sing-box 干跑:通不过就回滚,绝不让一条坏线路留在库里拖垮后续所有重载
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&line).Error; err != nil {
				return err
			}
			return validateFullConfig(tx, s.run.NodeCert())
		})
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "line", "create", line.Name)
		s.reloadAll("新增线路 " + line.Name)
		writeJSON(w, http.StatusOK, line)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

func (s *Server) handleLineItem(w http.ResponseWriter, r *http.Request) {
	// /lines/sort 由专门的处理函数负责
	if strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/sort") {
		s.handleLineSort(w, r)
		return
	}
	if s.dispatchLineSubroute(w, r) {
		return
	}
	id, err := pathID(r, "/lines/")
	if err != nil {
		badRequest(w, err)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var line model.Line
		if err := json.NewDecoder(r.Body).Decode(&line); err != nil {
			badRequest(w, err)
			return
		}
		line.Id = id
		if err := s.validateLine(&line); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.dryRunLine(&line); err != nil {
			badRequest(w, err)
			return
		}
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.Line{}).Where("id = ?", id).Select(
				"name", "protocol", "port", "upstream_id", "options", "addrs", "node_ids", "tls", "transport", "enabled",
			).Updates(line).Error; err != nil {
				return err
			}
			return validateFullConfig(tx, s.run.NodeCert())
		})
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "line", "update", line.Name)
		s.reloadAll("修改线路 " + line.Name)
		writeJSON(w, http.StatusOK, line)
	case http.MethodDelete:
		var line model.Line
		s.db.First(&line, id)
		if err := s.db.Delete(&model.Line{}, id).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.db.Where("line_id = ?", id).Delete(&model.UserLine{})
		s.audit(r, "line", "delete", line.Name)
		s.reloadAll("删除线路 " + line.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// validateFullConfig 用事务内的当前数据渲染整套 sing-box 配置并干跑初始化。
// 单条入站/出站的 parse 校验抓不住"初始化才报错"的问题(如 ss2022 缺 PSK),
// 而这类错误会让之后每一次重载都失败,所以保存前必须整体干跑一遍。
func validateFullConfig(tx *gorm.DB, cert render.NodeCert) error {
	raw, err := render.BuildConfig(tx, cert)
	if err != nil {
		return fmt.Errorf("渲染配置失败(已取消保存): %w", err)
	}
	if err := core.ValidateConfig(raw); err != nil {
		return fmt.Errorf("配置未通过 sing-box 校验(已取消保存): %w", err)
	}
	return nil
}

// validateLine 校验线路必填项与唯一性(端口/名称)。
func (s *Server) validateLine(line *model.Line) error {
	line.Name = strings.TrimSpace(line.Name)
	if line.Name == "" {
		return errors.New("线路名称不能为空")
	}
	if _, ok := render.Protocols[line.Protocol]; !ok {
		return fmt.Errorf("不支持的协议: %s", line.Protocol)
	}
	if len(line.Tls) > 0 {
		var probe map[string]interface{}
		if err := json.Unmarshal(line.Tls, &probe); err != nil {
			return fmt.Errorf("TLS 配置不是合法 JSON: %w", err)
		}
	}
	if len(line.Transport) > 0 {
		var probe map[string]interface{}
		if err := json.Unmarshal(line.Transport, &probe); err != nil {
			return fmt.Errorf("传输配置不是合法 JSON: %w", err)
		}
	}
	if len(line.NodeIds) > 0 {
		var ids []uint
		if err := json.Unmarshal(line.NodeIds, &ids); err != nil {
			return fmt.Errorf("服务器列表格式错误: %w", err)
		}
		if len(ids) == 0 {
			line.NodeIds = nil // 空 = 全部服务器
		}
	}
	if line.Port < 1 || line.Port > 65535 {
		return errors.New("端口需在 1-65535 之间")
	}
	if len(line.Options) == 0 {
		line.Options = json.RawMessage("{}")
	}
	var probe map[string]interface{}
	if err := json.Unmarshal(line.Options, &probe); err != nil {
		return fmt.Errorf("协议参数不是合法 JSON: %w", err)
	}
	if line.Protocol == "shadowsocks" {
		m, _ := probe["method"].(string)
		if m == "" {
			return errors.New("shadowsocks 线路必须设置 method")
		}
		// 2022 系列需要服务端 PSK(aes-128 为 16 字节,其余 32 字节),留空则自动生成
		if strings.HasPrefix(m, "2022-") {
			if p, _ := probe["password"].(string); p == "" {
				n := 32
				if strings.Contains(m, "aes-128") {
					n = 16
				}
				probe["password"] = randomBase64(n)
				if b, err := json.Marshal(probe); err == nil {
					line.Options = b
				}
			}
		}
	}
	// 名称与端口全局唯一
	dup := s.db.Model(&model.Line{}).Where("(name = ? OR port = ?)", line.Name, line.Port)
	if line.Id != 0 {
		dup = dup.Where("id != ?", line.Id)
	}
	var n int64
	dup.Count(&n)
	if n > 0 {
		return errors.New("线路名称或端口已被占用")
	}
	if line.UpstreamId != 0 {
		var exists int64
		s.db.Model(&model.Upstream{}).Where("id = ?", line.UpstreamId).Count(&exists)
		if exists == 0 {
			return errors.New("指定的上游不存在")
		}
	}
	return nil
}

// handleLineSort 批量保存拖拽后的顺序。
func (s *Server) handleLineSort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var ids []uint
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		badRequest(w, err)
		return
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		for i, id := range ids {
			if err := tx.Model(&model.Line{}).Where("id = ?", id).Update("sort", i+1).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "line", "sort", ids)
	// 顺序只影响订阅展示,不需要重启数据面
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// ---- 上游 ----

func (s *Server) handleUpstreams(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var ups []model.Upstream
		s.db.Order("id asc").Find(&ups)
		writeJSON(w, http.StatusOK, ups)
	case http.MethodPost:
		var up model.Upstream
		if err := json.NewDecoder(r.Body).Decode(&up); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.validateUpstream(&up); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.dryRunUpstream(&up); err != nil {
			badRequest(w, err)
			return
		}
		up.Id = 0
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&up).Error; err != nil {
				return err
			}
			return validateFullConfig(tx, s.run.NodeCert())
		})
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "upstream", "create", up.Name)
		s.reloadUpstreams("新增上游 " + up.Name)
		writeJSON(w, http.StatusOK, up)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

func (s *Server) handleUpstreamItem(w http.ResponseWriter, r *http.Request) {
	if s.dispatchUpstreamSubroute(w, r) {
		return
	}
	id, err := pathID(r, "/upstreams/")
	if err != nil {
		badRequest(w, err)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var up model.Upstream
		if err := json.NewDecoder(r.Body).Decode(&up); err != nil {
			badRequest(w, err)
			return
		}
		up.Id = id
		if err := s.validateUpstream(&up); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.dryRunUpstream(&up); err != nil {
			badRequest(w, err)
			return
		}
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.Upstream{}).Where("id = ?", id).
				Select("name", "type", "options").Updates(up).Error; err != nil {
				return err
			}
			return validateFullConfig(tx, s.run.NodeCert())
		})
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "upstream", "update", up.Name)
		s.reloadUpstreams("修改上游 " + up.Name)
		writeJSON(w, http.StatusOK, up)
	case http.MethodDelete:
		var inUse int64
		s.db.Model(&model.Line{}).Where("upstream_id = ?", id).Count(&inUse)
		if inUse > 0 {
			badRequest(w, fmt.Errorf("该上游仍被 %d 条线路使用,无法删除", inUse))
			return
		}
		if err := s.db.Delete(&model.Upstream{}, id).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "upstream", "delete", id)
		s.reloadUpstreams("删除上游")
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

func (s *Server) validateUpstream(up *model.Upstream) error {
	up.Name = strings.TrimSpace(up.Name)
	if up.Name == "" {
		return errors.New("上游名称不能为空")
	}
	if up.Type == "" {
		return errors.New("上游类型不能为空")
	}
	if len(up.Options) == 0 {
		up.Options = json.RawMessage("{}")
	}
	var probe map[string]interface{}
	if err := json.Unmarshal(up.Options, &probe); err != nil {
		return fmt.Errorf("上游参数不是合法 JSON: %w", err)
	}
	dup := s.db.Model(&model.Upstream{}).Where("name = ?", up.Name)
	if up.Id != 0 {
		dup = dup.Where("id != ?", up.Id)
	}
	var n int64
	dup.Count(&n)
	if n > 0 {
		return errors.New("上游名称已存在")
	}
	return nil
}

// ---- 用户 ----

type userPayload struct {
	model.User
	LineIds []uint `json:"lineIds"`
	ExtIds  []uint `json:"extIds"`
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var users []model.User
		s.db.Order("id asc").Find(&users)
		type row struct {
			model.User
			LineIds  []uint   `json:"lineIds"`
			ExtIds   []uint   `json:"extIds"`
			OnlineIP []string `json:"onlineIps"`
			SubURL   string   `json:"subUrl"`
		}
		out := make([]row, 0, len(users))
		for _, u := range users {
			var ids []uint
			s.db.Model(&model.UserLine{}).Where("user_id = ?", u.Id).Pluck("line_id", &ids)
			eids := []uint{}
			s.db.Model(&model.UserExt{}).Where("user_id = ?", u.Id).Pluck("ext_id", &eids)
			u.Credentials = nil // 列表不返回凭据
			out = append(out, row{
				User: u, LineIds: ids, ExtIds: eids,
				OnlineIP: mergeIPs(s.run.OnlineIPs(u.Name), s.run.Hub().RemoteIPs(u.Name)),
				SubURL:   s.subLinks(u)["link"],
			})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var p userPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.validateUser(&p.User); err != nil {
			badRequest(w, err)
			return
		}
		p.User.Id = 0
		p.User.CreatedAt = time.Now().Unix()
		if len(p.User.Credentials) == 0 {
			p.User.Credentials = generateCredentials(p.User.Name)
		}
		if err := s.db.Create(&p.User).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.setUserLines(p.User.Id, p.LineIds)
		s.setUserExts(p.User.Id, p.ExtIds)
		s.audit(r, "user", "create", p.User.Name)
		s.reloadUsers("新增用户 " + p.User.Name)
		writeJSON(w, http.StatusOK, p.User)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

func (s *Server) handleUserItem(w http.ResponseWriter, r *http.Request) {
	if s.dispatchUserSubroute(w, r) {
		return
	}
	id, err := pathID(r, "/users/")
	if err != nil {
		badRequest(w, err)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var p userPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		p.User.Id = id
		if err := s.validateUser(&p.User); err != nil {
			badRequest(w, err)
			return
		}
		// 凭据与累计流量不经由表单覆盖
		if err := s.db.Model(&model.User{}).Where("id = ?", id).Select(
			"name", "enabled", "volume", "expiry", "auto_reset", "reset_days",
			"next_reset", "device_limit", "speed_up", "speed_down", "remark", "desc",
		).Updates(p.User).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.setUserLines(id, p.LineIds)
		s.setUserExts(id, p.ExtIds)
		s.audit(r, "user", "update", p.User.Name)
		s.reloadUsers("修改用户 " + p.User.Name)
		writeJSON(w, http.StatusOK, p.User)
	case http.MethodDelete:
		var u model.User
		s.db.First(&u, id)
		if err := s.db.Delete(&model.User{}, id).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.db.Where("user_id = ?", id).Delete(&model.UserLine{})
		s.audit(r, "user", "delete", u.Name)
		s.reloadUsers("删除用户 " + u.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

func (s *Server) validateUser(u *model.User) error {
	u.Name = strings.TrimSpace(u.Name)
	if u.Name == "" {
		return errors.New("用户名不能为空")
	}
	if strings.ContainsAny(u.Name, "/?#& ") {
		return errors.New("用户名不能包含空格或 / ? # & (它是订阅地址的一部分)")
	}
	if u.DeviceLimit < 0 || u.SpeedUp < 0 || u.SpeedDown < 0 {
		return errors.New("设备数与限速不能为负")
	}
	dup := s.db.Model(&model.User{}).Where("name = ?", u.Name)
	if u.Id != 0 {
		dup = dup.Where("id != ?", u.Id)
	}
	var n int64
	dup.Count(&n)
	if n > 0 {
		return errors.New("用户名已存在")
	}
	return nil
}

func (s *Server) setUserLines(userID uint, lineIds []uint) {
	s.db.Where("user_id = ?", userID).Delete(&model.UserLine{})
	for _, lid := range lineIds {
		s.db.Create(&model.UserLine{UserId: userID, LineId: lid})
	}
}

// generateCredentials 为新用户生成全部协议的凭据。
func generateCredentials(name string) json.RawMessage {
	b, _ := json.Marshal(creds.Generate(name))
	return b
}

// ---- 设置 ----

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var rows []model.Setting
		s.db.Find(&rows)
		out := map[string]string{}
		for _, row := range rows {
			out[row.Key] = row.Value
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			badRequest(w, err)
			return
		}
		roleChanged := false
		oldDomain := s.setting("webDomain")
		for k, v := range in {
			v = strings.TrimSpace(v)
			if k == "nodeMode" && v != s.setting("nodeMode") {
				roleChanged = true
			}
			if k == "webDomain" && v != oldDomain && v != "" {
				// 本机服务器记录的域名跟着面板域名走(用户没单独改过时)
				s.db.Model(&model.Node{}).Where("is_local = ? AND (domain = ? OR domain = '')", true, oldDomain).Update("domain", v)
			}
			var existing model.Setting
			if err := s.db.Where("key = ?", k).First(&existing).Error; err == nil {
				s.db.Model(&model.Setting{}).Where("key = ?", k).Update("value", v)
			} else {
				s.db.Create(&model.Setting{Key: k, Value: v})
			}
		}
		if roleChanged {
			logger.Info("面板角色已切换为: ", s.role())
		}
		keys := make([]string, 0, len(in))
		for k := range in {
			keys = append(keys, k)
		}
		s.audit(r, "settings", "update", keys)
		// 端口/证书类改动需重启进程才生效,这里提示前端
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok": "1", "role": s.role(),
			"note": "端口、证书与监听地址的改动需重启 m-ui 生效",
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req struct{ Username, OldPassword, NewPassword string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	if len(req.NewPassword) < 6 {
		badRequest(w, errors.New("新密码至少 6 位"))
		return
	}
	var admin model.Admin
	if err := s.db.First(&admin).Error; err != nil {
		badRequest(w, err)
		return
	}
	if !checkPassword(req.OldPassword, admin.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "原密码错误"})
		return
	}
	hashed, err := hashPassword(req.NewPassword)
	if err != nil {
		badRequest(w, err)
		return
	}
	updates := map[string]interface{}{"password": hashed}
	if u := strings.TrimSpace(req.Username); u != "" {
		updates["username"] = u
	}
	s.db.Model(&model.Admin{}).Where("id = ?", admin.Id).Updates(updates)
	s.run.SetSetting("adminDefault", "false") // 默认密码提示解除
	s.audit(r, "admin", "password", admin.Username)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// ---- 订阅访问日志 ----

func (s *Server) handleSubLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	q := s.db.Model(&model.SubLog{}).Order("ts desc").Limit(limit)
	if user := r.URL.Query().Get("user"); user != "" {
		q = q.Where("user = ?", user)
	}
	var logs []model.SubLog
	q.Find(&logs)
	writeJSON(w, http.StatusOK, logs)
}

// ---- 重载 ----

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if err := s.run.ReloadAll(); err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// reloadAll 在线路/上游变更后重建数据面;失败只记日志,数据已落库。
func (s *Server) reloadAll(reason string) {
	go func() {
		if err := s.run.ReloadAll(); err != nil {
			logger.Warning(reason, " 后重载数据面失败: ", err)
			return
		}
		logger.Info(reason, ",数据面已重载")
		_ = database.Checkpoint(s.db)
	}()
}

// reloadUsers 用户变更走热更新,不断开其他用户连接。
// reloadUpstreams 上游变化:只热换出站,不重启数据面(失败时 runner 自动回退全量重载)。
func (s *Server) reloadUpstreams(reason string) {
	go func() {
		if err := s.run.ReloadUpstreams(); err != nil {
			logger.Warning(reason, " 后更新上游失败: ", err)
			return
		}
		logger.Info(reason, ",上游已更新")
		_ = database.Checkpoint(s.db)
	}()
}

func (s *Server) reloadUsers(reason string) {
	go func() {
		if err := s.run.ReloadUsers(); err != nil {
			logger.Warning(reason, " 后热更新用户失败: ", err)
			return
		}
		logger.Info(reason, ",用户已热更新")
		_ = database.Checkpoint(s.db)
	}()
}
