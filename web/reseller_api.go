package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// ---- 代理 CRUD(主面板)----
//
// 代理自己没有订阅,只是一个额度容器 + 一个独立面板:
// 他建的用户走随机令牌订阅,用量与设备数全部汇总到他名下。

type resellerRow struct {
	model.Reseller
	LineIds    []uint `json:"lineIds"`
	NeedsClaim bool   `json:"needsClaim"` // 还没设过密码
	Users      int    `json:"users"`
	Used       int64  `json:"used"`    // 名下用户已用流量之和
	Devices    int    `json:"devices"` // 名下用户设备上限之和
	Online     int    `json:"online"`  // 当前在线设备数(去重 IP)
}

func (s *Server) handleResellers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var list []model.Reseller
		s.db.Order("id asc").Find(&list)
		online := s.onlineIPsAll() // 一次锁拿全量,别按用户逐个抢数据面的锁
		out := make([]resellerRow, 0, len(list))
		for _, rs := range list {
			out = append(out, s.resellerRowWith(rs, online))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var p resellerRow
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.validateReseller(&p.Reseller); err != nil {
			badRequest(w, err)
			return
		}
		p.Id, p.CreatedAt = 0, time.Now().Unix()
		p.Password, p.TotpSecret, p.TotpEnabled = "", "", false // 新代理无密码,首次登录时自行设置
		p.ClaimBefore = p.CreatedAt + 24*3600                   // 24 小时内首登设密码,过期要重新重置
		p.PageEnabled, p.ShareOn = true, true
		if err := s.db.Create(&p.Reseller).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.setResellerLines(p.Id, p.LineIds)
		s.audit(r, "reseller", "create", p.Name)
		writeJSON(w, http.StatusOK, s.resellerRow(p.Reseller))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

func (s *Server) handleResellerItem(w http.ResponseWriter, r *http.Request) {
	if s.dispatchResellerSubroute(w, r) {
		return
	}
	id, err := pathID(r, "/resellers/")
	if err != nil {
		badRequest(w, err)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var p resellerRow
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		p.Id = id
		if err := s.validateReseller(&p.Reseller); err != nil {
			badRequest(w, err)
			return
		}
		// 密码、2FA、落地页文案由代理自己在代理面板里改,主面板表单不覆盖
		if err := s.db.Model(&model.Reseller{}).Where("id = ?", id).Select(
			"name", "enabled", "volume", "device_limit", "speed_up", "speed_down", "expiry", "remark").
			Updates(p.Reseller).Error; err != nil {
			badRequest(w, err)
			return
		}
		s.setResellerLines(id, p.LineIds)
		s.audit(r, "reseller", "update", p.Name)
		s.reloadUsers("修改代理 " + p.Name)
		writeJSON(w, http.StatusOK, s.resellerRow(p.Reseller))
	case http.MethodDelete:
		var rs model.Reseller
		if err := s.db.First(&rs, id).Error; err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "代理不存在"})
			return
		}
		var users []model.User
		s.db.Where("reseller_id = ?", id).Find(&users)
		for _, u := range users {
			if err := s.deleteUser(u, s.actor(r)); err != nil {
				badRequest(w, err)
				return
			}
		}
		s.db.Where("reseller_id = ?", id).Delete(&model.ResellerLine{})
		s.db.Delete(&model.Reseller{}, id)
		s.audit(r, "reseller", "delete", rs.Name)
		writeJSON(w, http.StatusOK, map[string]int{"users": len(users)})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// dispatchResellerSubroute 处理 /resellers/{id}/users|reset|passwd。
func (s *Server) dispatchResellerSubroute(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	idx := strings.LastIndex(path, "/resellers/")
	if idx < 0 {
		return false
	}
	rest := strings.Split(path[idx+len("/resellers/"):], "/")
	if len(rest) != 2 {
		return false
	}
	id, err := strconv.ParseUint(rest[0], 10, 64)
	if err != nil {
		return false
	}
	var rs model.Reseller
	if s.db.First(&rs, uint(id)).Error != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "代理不存在"})
		return true
	}
	switch rest[1] {
	case "users": // 详情:名下用户 + 订阅地址 + 在线 IP 与所连线路
		writeJSON(w, http.StatusOK, s.resellerUsers(rs))
		return true
	case "reset":
		if r.Method != http.MethodPost {
			break
		}
		err := s.db.Model(&model.User{}).Where("reseller_id = ?", rs.Id).Updates(map[string]interface{}{
			"total_up": gorm.Expr("total_up + up"), "total_down": gorm.Expr("total_down + down"),
			"up": 0, "down": 0, "enabled": true,
		}).Error
		if err != nil {
			badRequest(w, err)
			return true
		}
		var allTime int64
		s.db.Model(&model.User{}).Where("reseller_id = ?", rs.Id).
			Select("COALESCE(SUM(up + down + total_up + total_down),0)").Scan(&allTime)
		s.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).
			Update("used_base", allTime+rs.UsedCarried) // 额度归零
		s.audit(r, "reseller", "reset", rs.Name)
		s.reloadUsers("重置代理流量 " + rs.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
		return true
	case "passwd": // 清空密码与 2FA:代理下次登录时重新设置
		if r.Method != http.MethodPost {
			break
		}
		s.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Updates(map[string]interface{}{
			"password": "", "totp_secret": "", "totp_enabled": false,
			"claim_before": time.Now().Unix() + 24*3600, // 重开 24 小时认领窗口
		})
		s.audit(r, "reseller", "passwd", rs.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
		return true
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	return true
}

// resellerRow 汇总代理的用户数、已用流量、设备预算与在线设备。
func (s *Server) resellerRow(rs model.Reseller) resellerRow {
	return s.resellerRowWith(rs, s.onlineIPsAll())
}

// onlineIPsAll 用户 → 在线源 IP(本机 + 副机),整表算一次。
func (s *Server) onlineIPsAll() map[string][]string {
	local, remote := s.run.OnlineIPsAll(), s.run.Hub().RemoteIPsAll()
	out := make(map[string][]string, len(local)+len(remote))
	for u, ips := range local {
		out[u] = ips
	}
	for u, ips := range remote {
		out[u] = mergeIPs(out[u], ips)
	}
	return out
}

func (s *Server) resellerRowWith(rs model.Reseller, online map[string][]string) resellerRow {
	row := resellerRow{Reseller: rs, LineIds: s.resellerLineIds(rs.Id), NeedsClaim: rs.Password == ""}
	var agg struct {
		N       int64
		Devices int64
	}
	s.db.Raw(`SELECT COUNT(*) n, COALESCE(SUM(device_limit), 0) devices FROM users WHERE reseller_id = ?`, rs.Id).Scan(&agg)
	row.Users, row.Used, row.Devices = int(agg.N), resellerUsed(s.db, rs), int(agg.Devices)
	row.Online = len(s.resellerOnlineIPs(rs.Id, online))
	return row
}

// resellerOnlineIPs 代理名下所有用户当前在线的源 IP(去重)。
func (s *Server) resellerOnlineIPs(id uint, online map[string][]string) []string {
	var names []string
	s.db.Model(&model.User{}).Where("reseller_id = ?", id).Pluck("name", &names)
	seen := map[string]bool{}
	for _, n := range names {
		for _, ip := range online[n] {
			seen[ip] = true
		}
	}
	out := make([]string, 0, len(seen))
	for ip := range seen {
		out = append(out, ip)
	}
	return out
}

type resellerUserRow struct {
	model.User
	LineIds  []uint              `json:"lineIds"`
	OnlineIP []string            `json:"onlineIps"`
	OnlineOn map[string][]string `json:"onlineLines"`
	Sub      map[string]string   `json:"sub"`
}

func (s *Server) resellerUsers(rs model.Reseller) []resellerUserRow {
	var users []model.User
	s.db.Where("reseller_id = ?", rs.Id).Order("id asc").Find(&users)
	localName, localLines := s.localNodeName(), s.run.OnlineIPLines()
	remoteLines, online, base := s.run.Hub().RemoteIPLinesAll(), s.onlineIPsAll(), s.subBase()
	lineIdsBy := s.userLineMap()
	out := make([]resellerUserRow, 0, len(users))
	for _, u := range users {
		ids := lineIdsBy[u.Id]
		key := base + subKey(u)
		links := map[string]string{"link": key, "clash": key + "?format=clash", "json": key + "?format=json"}
		u.Credentials, u.ShareCreds = nil, nil
		out = append(out, resellerUserRow{
			User: u, LineIds: ids, Sub: links,
			OnlineIP: online[u.Name],
			OnlineOn: s.onlineLines(u.Name, localName, localLines, remoteLines),
		})
	}
	return out
}

func (s *Server) resellerLineIds(id uint) []uint {
	ids := []uint{}
	s.db.Model(&model.ResellerLine{}).Where("reseller_id = ?", id).Pluck("line_id", &ids)
	return ids
}

func (s *Server) setResellerLines(id uint, lineIds []uint) {
	s.db.Where("reseller_id = ?", id).Delete(&model.ResellerLine{})
	for _, lid := range lineIds {
		s.db.Create(&model.ResellerLine{ResellerId: id, LineId: lid})
	}
}

func (s *Server) validateReseller(rs *model.Reseller) error {
	rs.Name = strings.TrimSpace(rs.Name)
	if rs.Name == "" {
		return errors.New("代理名称不能为空")
	}
	if strings.ContainsAny(rs.Name, "/?#& ") {
		return errors.New("代理名称不能包含空格或 / ? # &(它是登录名)")
	}
	if rs.Volume < 0 || rs.DeviceLimit < 0 || rs.SpeedUp < 0 || rs.SpeedDown < 0 || rs.Expiry < 0 {
		return errors.New("额度不能为负")
	}
	dup := s.db.Model(&model.Reseller{}).Where("name = ?", rs.Name)
	if rs.Id != 0 {
		dup = dup.Where("id != ?", rs.Id)
	}
	var n int64
	dup.Count(&n)
	if n > 0 {
		return errors.New("代理名称已存在")
	}
	return nil
}
