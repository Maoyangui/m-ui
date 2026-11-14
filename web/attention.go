package web

import (
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 概况「需要处理」里的几项计数:直属用户快到期 / 流量将尽、代理额度用尽 / 快到期、定时备份的状况。
// 只在主机、非代理作用域的 /api/status 里给;60 秒缓存,惰性计算(status 每 5 秒一次,也只是一分钟查几次库,
// 和打开了多少个标签页无关)。口径和 Telegram 的到期 / 流量预警是同一组设置。

const attentionTTL = 60 * time.Second

type attentionBackup struct {
	Auto      bool   `json:"auto"`      // 开着定时备份(backupHour ≥ 0)
	LastAt    int64  `json:"lastAt"`    // 最新一份备份文件的时间
	LastErr   string `json:"lastErr"`   // 最近一次定时备份失败的原因(成功后清空)
	LastErrAt int64  `json:"lastErrAt"` // 同上,时间
}

type attentionView struct {
	At                int64           `json:"at"`
	ExpiringDays      int             `json:"expiringDays"`
	QuotaPct          int             `json:"quotaPct"`
	UsersExpiring     int64           `json:"usersExpiring"`     // 直属、启用,N 天内到期(不含已过期)
	UsersQuota        int64           `json:"usersQuota"`        // 直属、启用、限量,已用到 P% 及以上
	ResellersDepleted int64           `json:"resellersDepleted"` // 启用、额度已用尽
	ResellersExpiring int64           `json:"resellersExpiring"` // 启用、7 天内到期(还没到期)
	ResellersExpired  int64           `json:"resellersExpired"`  // 启用、过去 7 天里到期的(更早到期的不再常驻提示,代理页能看到)
	UsersAttn         int64           `json:"usersAttn"`         // 上面两类用户去重后的人数
	ResellersAttn     int64           `json:"resellersAttn"`     // 上面三类代理去重后的个数
	Backup            attentionBackup `json:"backup"`
}

// attention 取缓存;过期了重算一次(同一时刻只有一个请求在算,其它的等它)。
func (s *Server) attention() attentionView {
	s.attnMu.Lock()
	defer s.attnMu.Unlock()
	now := time.Now()
	if s.attnAt.IsZero() || now.Sub(s.attnAt) >= attentionTTL || now.Before(s.attnAt) {
		s.attn = s.computeAttention(now)
		s.attnAt = now
	}
	return s.attn
}

// attentionThresholds 「快到期」的天数和「流量将尽」的百分比:跟 Telegram 预警同一组设置,没填或填 0 时按 3 天 / 80%。
func (s *Server) attentionThresholds() (days, pct int) {
	days = s.settingInt("tgExpiringDays", 3)
	if days <= 0 {
		days = 3
	}
	pct = s.settingInt("tgQuotaPercent", 80)
	if pct <= 0 || pct > 100 {
		pct = 80
	}
	return days, pct
}

func (s *Server) computeAttention(now time.Time) attentionView {
	days, pct := s.attentionThresholds()
	ts := now.Unix()
	v := attentionView{At: ts, ExpiringDays: days, QuotaPct: pct}
	direct := "COALESCE(reseller_id,0) = 0 AND enabled = ?"
	s.db.Model(&model.User{}).Where(direct, true).
		Where("expiry > ? AND expiry <= ?", ts, ts+int64(days)*86400).Count(&v.UsersExpiring)
	s.db.Model(&model.User{}).Where(direct, true).
		Where("volume > 0 AND (up + down) * 100 >= volume * ?", pct).Count(&v.UsersQuota)
	s.db.Model(&model.Reseller{}).Where("enabled = ? AND depleted = ?", true, true).Count(&v.ResellersDepleted)
	s.db.Model(&model.Reseller{}).Where("enabled = ? AND expiry > ? AND expiry <= ?", true, ts, ts+7*86400).Count(&v.ResellersExpiring)
	s.db.Model(&model.Reseller{}).Where("enabled = ? AND expiry > ? AND expiry <= ?", true, ts-7*86400, ts).Count(&v.ResellersExpired)
	if h, err := strconv.Atoi(strings.TrimSpace(s.setting("backupHour"))); err == nil && h >= 0 {
		v.Backup.Auto = true
	}
	if files := s.run.ListBackups(); len(files) > 0 {
		v.Backup.LastAt = files[0].Time
	}
	if v.Backup.Auto { // 关了定时备份就不再报它失败过
		v.Backup.LastErr, v.Backup.LastErrAt = s.run.BackupError()
	}
	// 同时快到期又流量将尽的人、同时额度用尽又快到期的代理只算一次:「需要处理」的标题用这两个数,分项仍各算各的
	s.db.Model(&model.User{}).Where(direct, true).
		Where("(expiry > ? AND expiry <= ?) OR (volume > 0 AND (up + down) * 100 >= volume * ?)", ts, ts+int64(days)*86400, pct).Count(&v.UsersAttn)
	s.db.Model(&model.Reseller{}).Where("enabled = ?", true).
		Where("depleted = ? OR (expiry > ? AND expiry <= ?)", true, ts-7*86400, ts+7*86400).Count(&v.ResellersAttn)
	return v
}
