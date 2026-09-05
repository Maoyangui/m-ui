package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hop"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/tz"

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
	var lineCount, linesEnabled, upstreamCount, userCount, enabledUsers, resellerUsers int64
	s.db.Model(&model.Line{}).Count(&lineCount)
	s.db.Model(&model.Line{}).Where("enabled = ?", true).Count(&linesEnabled)
	s.db.Model(&model.Upstream{}).Count(&upstreamCount)
	// 用户数与用户页同口径:只算直属用户,代理的单独给一个数
	s.db.Model(&model.User{}).Where("COALESCE(reseller_id,0) = 0").Count(&userCount)
	s.db.Model(&model.User{}).Where("COALESCE(reseller_id,0) = 0 AND enabled = ?", true).Count(&enabledUsers)
	s.db.Model(&model.User{}).Where("reseller_id > 0").Count(&resellerUsers)

	var totalUp, totalDown int64
	s.db.Model(&model.User{}).Select("COALESCE(SUM(up+total_up),0)").Scan(&totalUp)
	s.db.Model(&model.User{}).Select("COALESCE(SUM(down+total_down),0)").Scan(&totalDown)

	if rid := scope(r); rid > 0 {
		writeJSON(w, http.StatusOK, s.resellerStatus(r, rid))
		return
	}
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
		"timezone":      s.panelLocation().String(),
		"goroutines":    runtime.NumGoroutine(),
		"version":       Version,
		"repo":          brand.Repo,
		"onlineUsers":   len(mergeIPs(s.run.Onlines().Users, s.run.Hub().RemoteOnlineUsers())), // 含副机上的在线用户
		"linesEnabled":  linesEnabled,
		"resellerUsers": resellerUsers,
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
	if st := s.lastUpgrade(); st != nil { // 上次一键更新回滚过:页面顶部要明说,管理员点"知道了"才消
		status["upgrade"] = st
	}
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
// panelLocation 面板时区(设置 timezone,默认 Asia/Shanghai):所有时间按它显示与对齐。
func (s *Server) panelLocation() *time.Location { return tz.Location(s.setting("timezone")) }

func (s *Server) role() string {
	if strings.EqualFold(s.setting("nodeMode"), "true") {
		return "node"
	}
	return "master"
}

// ---- 线路 ----

func (s *Server) handleLines(w http.ResponseWriter, r *http.Request) {
	rid := scope(r)
	if rid > 0 && r.Method != http.MethodGet {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权限"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		var lines []model.Line
		q := s.db.Order("sort asc, id asc")
		if rid > 0 {
			q = q.Where("id IN (SELECT line_id FROM reseller_lines WHERE reseller_id = ?)", rid)
		}
		q.Find(&lines)
		if rid > 0 { // 代理只需要名字与协议,服务端凭据一律不给
			type slim struct {
				Id       uint   `json:"id"`
				Name     string `json:"name"`
				Protocol string `json:"protocol"`
				Port     int    `json:"port"`
				Enabled  bool   `json:"enabled"`
			}
			out := make([]slim, 0, len(lines))
			for _, l := range lines {
				out = append(out, slim{l.Id, l.Name, l.Protocol, l.Port, l.Enabled})
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
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
		var p struct {
			model.Line
			AssignAll bool `json:"assignAll"` // 新线路直接分配给全部现有用户
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		line := p.Line
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
		nodeCert := s.run.NodeCert() // 事务里不能再走连接池,先取好
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&line).Error; err != nil {
				return err
			}
			if p.AssignAll {
				var userIds []uint
				tx.Model(&model.User{}).Pluck("id", &userIds)
				for _, uid := range userIds {
					if err := tx.Create(&model.UserLine{UserId: uid, LineId: line.Id}).Error; err != nil {
						return err
					}
				}
			}
			return validateFullConfig(tx, nodeCert)
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
		nodeCert := s.run.NodeCert() // 事务里不能再走连接池,先取好
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.Line{}).Where("id = ?", id).Select(
				"name", "protocol", "port", "upstream_id", "options", "addrs", "node_ids", "tls", "transport", "enabled",
			).Updates(line).Error; err != nil {
				return err
			}
			return validateFullConfig(tx, nodeCert)
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
		s.db.Where("line_id = ?", id).Delete(&model.ResellerLine{}) // 代理的线路授权一并清掉
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
	// 干跑校验不绑定端口,和面板/订阅/代理面板撞号要等数据面启动才炸,在这里先拦
	for _, p := range []struct {
		label string
		port  int
	}{
		{"面板", s.settingInt("webPort", 2053)},
		{"订阅", s.settingInt("subPort", 2056)},
		{"代理面板", s.settingInt("resellerPort", 2054)},
	} {
		if p.label == "代理面板" && strings.EqualFold(s.setting("resellerEnabled"), "false") {
			continue
		}
		if line.Port == p.port {
			return fmt.Errorf("端口 %d 已被%s占用,换一个", line.Port, p.label)
		}
	}
	// 线路之间撞端口(端口全局唯一,不分服务器):后面的 bind 探测只会说"被别的程序占用",
	// 这里先把占着端口的那条线路点名
	if name := s.lineOnPort(line); name != "" {
		return fmt.Errorf("端口 %d 已被线路 %q 占用,换一个", line.Port, name)
	}
	// 机器上可能还跑着别的项目:端口有变化时试着监听一次,占着就别让它进库
	// (端口没改的编辑不测——那时占着它的正是本机运行中的数据面)
	if s.portChanged(line) && !portBindable(line.Port) {
		return fmt.Errorf("端口 %d 已被本机其它程序占用,换一个", line.Port)
	}
	if len(line.Options) == 0 {
		line.Options = json.RawMessage("{}")
	}
	var probe map[string]interface{}
	if err := json.Unmarshal(line.Options, &probe); err != nil {
		return fmt.Errorf("协议参数不是合法 JSON: %w", err)
	}
	if line.Protocol == "hysteria2" {
		if ph, _ := probe["port_hopping"].(string); strings.TrimSpace(ph) != "" {
			n, err := hop.Normalize(ph)
			if err != nil {
				return err
			}
			probe["port_hopping"] = n
			var others []model.Line
			s.db.Where("protocol = ? AND id != ?", "hysteria2", line.Id).Find(&others)
			var rules []hop.Rule
			a, b, _ := hop.ParseRange(n)
			rules = append(rules, hop.Rule{From: a, To: b, Port: line.Port})
			for _, o := range others {
				var oo struct {
					PortHopping string `json:"port_hopping"`
				}
				if json.Unmarshal(o.Options, &oo) == nil && oo.PortHopping != "" {
					if x, y, err := hop.ParseRange(oo.PortHopping); err == nil {
						rules = append(rules, hop.Rule{From: x, To: y, Port: o.Port})
					}
				}
			}
			var ports []int
			s.db.Model(&model.Line{}).Where("id != ?", line.Id).Pluck("port", &ports)
			if err := hop.Overlaps(rules, ports); err != nil {
				return err
			}
			if b, err := json.Marshal(probe); err == nil {
				line.Options = b
			}
		} else if _, has := probe["port_hopping"]; has {
			delete(probe, "port_hopping")
			if b, err := json.Marshal(probe); err == nil {
				line.Options = b
			}
		}
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
	// 名称全局唯一(端口在上面已按线路点名拦过)
	dup := s.db.Model(&model.Line{}).Where("name = ?", line.Name)
	if line.Id != 0 {
		dup = dup.Where("id != ?", line.Id)
	}
	var n int64
	dup.Count(&n)
	if n > 0 {
		return errors.New("线路名称已被占用")
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
		nodeCert := s.run.NodeCert() // 事务里不能再走连接池,先取好
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&up).Error; err != nil {
				return err
			}
			return validateFullConfig(tx, nodeCert)
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
		nodeCert := s.run.NodeCert() // 事务里不能再走连接池,先取好
		err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.Upstream{}).Where("id = ?", id).
				Select("name", "type", "options").Updates(up).Error; err != nil {
				return err
			}
			return validateFullConfig(tx, nodeCert)
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
	// direct / block 是内置出站的标签:重名会让整份配置的出站标签冲突,
	// 单独校验这个上游时看不出来,等到重载数据面才炸。
	if n := strings.ToLower(up.Name); n == "direct" || n == "block" {
		return errors.New("上游名称不能用 direct 或 block(内置出站占用)")
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
		q := s.db.Order("id asc")
		if scope(r) == 0 {
			q = q.Where("COALESCE(reseller_id,0) = 0") // 代理的用户不混进主面板用户页
		}
		s.scoped(r, q).Find(&users)
		localName := s.localNodeName()
		localLines, remoteLines := s.run.OnlineIPLines(), s.run.Hub().RemoteIPLinesAll()
		localIPs, remoteIPs := s.run.OnlineIPsAll(), s.run.Hub().RemoteIPsAll() // 一次锁拿全量,别按用户逐个抢数据面的锁
		subBase := s.subBase()
		type row struct {
			model.User
			LineIds      []uint              `json:"lineIds"`
			ExtIds       []uint              `json:"extIds"`
			OnlineIP     []string            `json:"onlineIps"`
			OnlineOn     map[string][]string `json:"onlineLines"` // 源 IP → 该 IP 正在使用的线路(带服务器后缀)
			SubURL       string              `json:"subUrl"`
			ResellerName string              `json:"resellerName,omitempty"` // 归属代理
		}
		lineIdsBy, extIdsBy := s.userLineMap(), s.userExtMap() // 两条查询代替每个用户两条
		rsNames := map[uint]string{}
		if scope(r) == 0 {
			var rss []model.Reseller
			s.db.Find(&rss)
			for _, x := range rss {
				rsNames[x.Id] = x.Name
			}
		}
		out := make([]row, 0, len(users))
		for _, u := range users {
			ids, eids := lineIdsBy[u.Id], extIdsBy[u.Id]
			if eids == nil {
				eids = []uint{}
			}
			u.Credentials, u.ShareCreds = nil, nil // 列表不返回凭据
			out = append(out, row{
				User: u, LineIds: ids, ExtIds: eids,
				OnlineIP:     mergeIPs(localIPs[u.Name], remoteIPs[u.Name]),
				OnlineOn:     s.onlineLines(u.Name, localName, localLines, remoteLines),
				SubURL:       subBase + subKey(u),
				ResellerName: rsNames[u.ResellerId],
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
		s.applySubTokenPolicy(&p.User) // 设置里关掉"用用户名作订阅地址"时,发一个随机地址
		if rid := scope(r); rid > 0 {
			if err := s.prepareResellerUser(rid, &p.User, p.LineIds); err != nil {
				badRequest(w, err)
				return
			}
			p.ExtIds = nil // 外部节点只由主面板分配
		}
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
	if !s.guardScope(w, r) {
		return
	}
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
		if rid := scope(r); rid > 0 {
			if err := s.checkResellerUser(rid, id, &p.User, p.LineIds); err != nil {
				badRequest(w, err)
				return
			}
			p.ExtIds = nil
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
		if err := s.db.First(&u, id).Error; err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
			return
		}
		if err := s.deleteUser(u, s.actor(r)); err != nil {
			badRequest(w, err)
			return
		}
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

// deleteUser 删除用户及其线路/外部节点关联,踢下线并热更新数据面。
func (s *Server) deleteUser(u model.User, actor string) error {
	carryUsage(s.db, u) // 代理的用户:删之前把用量结转到代理头上,免得删号洗额度
	if err := s.db.Delete(&model.User{}, u.Id).Error; err != nil {
		return err
	}
	s.db.Where("user_id = ?", u.Id).Delete(&model.UserLine{})
	s.db.Where("user_id = ?", u.Id).Delete(&model.UserExt{})
	s.run.KickUser(u.Name)
	s.auditAs(actor, "user", "delete", u.Name)
	s.reloadUsers("删除用户 " + u.Name)
	return nil
}

// localNodeName 本机在服务器列表里的名称,用于给线路名加服务器后缀(如 "香港1-高带宽")。
func (s *Server) localNodeName() string {
	var n model.Node
	if err := s.db.Where("is_local = ?", true).First(&n).Error; err != nil {
		return ""
	}
	return n.Name
}

// onlineLines 汇总某用户每个在线 IP 正在使用的线路:本机来自连接跟踪器,副机来自它们的上报,
// 线路名一律带上服务器后缀,面板上就能看出这台设备连的是哪台服务器的哪条线路。
func (s *Server) onlineLines(user, localName string, local, remote map[string]map[string][]string) map[string][]string {
	out := map[string][]string{}
	for ip, lines := range local[user] {
		for _, l := range lines {
			if localName != "" {
				l += "-" + localName
			}
			out[ip] = append(out[ip], l)
		}
	}
	for ip, lines := range remote[user] {
		out[ip] = append(out[ip], lines...)
	}
	for ip := range out {
		sort.Strings(out[ip])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// userLineMap / userExtMap 整表关联一次查出来,列表渲染时按用户取,避免 N+1。
func (s *Server) userLineMap() map[uint][]uint {
	var rows []model.UserLine
	s.db.Order("user_id asc, line_id asc").Find(&rows)
	out := map[uint][]uint{}
	for _, r := range rows {
		out[r.UserId] = append(out[r.UserId], r.LineId)
	}
	return out
}

func (s *Server) userExtMap() map[uint][]uint {
	var rows []model.UserExt
	s.db.Order("user_id asc, ext_id asc").Find(&rows)
	out := map[uint][]uint{}
	for _, r := range rows {
		out[r.UserId] = append(out[r.UserId], r.ExtId)
	}
	return out
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
	rid := scope(r)
	if rid > 0 && r.Method != http.MethodGet {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权限"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		var rows []model.Setting
		s.db.Find(&rows)
		out := map[string]string{}
		for _, row := range rows {
			out[row.Key] = row.Value
		}
		if rid > 0 { // 代理只需要这几项来渲染界面
			out = map[string]string{"timezone": out["timezone"], "subUpdates": out["subUpdates"]}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.validatePorts(in); err != nil { // 端口写错会导致重启后打不开,先拦下
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

// validatePorts 校验这次提交后的面板 / 订阅 / 代理面板端口:必须是 1-65535 且互不相同。
// 端口填错要等重启才发现,那时面板已经起不来,只能进 SSH 用 m-ui set 救,所以在这里拦。
func (s *Server) validatePorts(in map[string]string) error {
	ports := map[string]struct {
		label string
		def   int
	}{
		"webPort":      {"面板", 2053},
		"subPort":      {"订阅", 2056},
		"resellerPort": {"代理面板", 2054},
	}
	got := map[string]int{}
	for key, meta := range ports {
		raw, ok := in[key]
		if !ok {
			raw = s.setting(key)
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			got[key] = meta.def
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%s端口 %q 不合法,应为 1-65535", meta.label, raw)
		}
		got[key] = n
	}
	seen := map[int]string{}
	for key, n := range got {
		if key == "resellerPort" && strings.EqualFold(s.settingOr(in, "resellerEnabled"), "false") {
			continue // 代理面板关着就不占端口
		}
		if other, dup := seen[n]; dup {
			return fmt.Errorf("%s与%s的端口都是 %d,不能重复", ports[key].label, ports[other].label, n)
		}
		seen[n] = key
	}
	// 端口有改动时,确认本机没有别的程序占着(没改的不测:占着它的就是自己)
	for key, n := range got {
		if _, active := seen[n]; !active {
			continue
		}
		if _, ok := in[key]; !ok || n == s.settingInt(key, ports[key].def) {
			continue // 没提交或没变(包括把默认值手动填进去):占着它的正是面板自己
		}
		if !portBindable(n) {
			return fmt.Errorf("%s端口 %d 已被本机其它程序占用,换一个", ports[key].label, n)
		}
	}
	// 反方向也要拦:把面板/订阅端口改成某条线路正在用的端口,同样会起不来
	lineByPort := map[int]string{}
	var lines []model.Line
	s.db.Select("name, port").Find(&lines)
	for _, l := range lines {
		lineByPort[l.Port] = l.Name
	}
	for key, n := range got {
		if _, active := seen[n]; !active {
			continue
		}
		if name, dup := lineByPort[n]; dup {
			return fmt.Errorf("%s端口 %d 已被线路 %q 占用,换一个", ports[key].label, n, name)
		}
	}
	return nil
}

// settingOr 取这次提交里的值,没提交就取库里的。
func (s *Server) settingOr(in map[string]string, key string) string {
	if v, ok := in[key]; ok {
		return v
	}
	return s.setting(key)
}

// lineOnPort 返回占着同一端口的另一条线路名;没有返回空。
func (s *Server) lineOnPort(line *model.Line) string {
	var o model.Line
	if s.db.Select("name").Where("port = ? AND id <> ?", line.Port, line.Id).First(&o).Error != nil {
		return ""
	}
	return o.Name
}

// portChanged 这次保存是否动了线路端口(新建视为动过)。
// 端口没变的编辑不做占用检测,否则运行中的数据面自己占着自己,会误报冲突。
func (s *Server) portChanged(line *model.Line) bool {
	if line.Id == 0 {
		return true
	}
	var cur model.Line
	if s.db.Select("port").First(&cur, line.Id).Error != nil {
		return true
	}
	return cur.Port != line.Port
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
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
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
	}()
}

func (s *Server) reloadUsers(reason string) {
	go func() {
		if err := s.run.ReloadUsers(); err != nil {
			logger.Warning(reason, " 后热更新用户失败: ", err)
			return
		}
		logger.Info(reason, ",用户已热更新")
	}()
}
