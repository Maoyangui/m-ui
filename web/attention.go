package web

import (
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 概况「需要处理」里的几项计数:代理额度用尽 / 快到期、定时备份的状况。
// 只在主机、非代理作用域的 /api/status 里给;60 秒缓存,惰性计算(status 每 5 秒一次,也只是一分钟查几次库,
// 和打开了多少个标签页无关)。
// 用户快到期 / 流量快用完不在这里:用户不续费,管理员也没什么可处理的,它不算「需要处理」——
// 想看这些人,用户页的筛选里有「即将到期」一类(web/assets/js/pages/users.js)。

const attentionTTL = 60 * time.Second

type attentionBackup struct {
	Auto      bool   `json:"auto"`      // 开着定时备份(backupHour ≥ 0)
	LastAt    int64  `json:"lastAt"`    // 最新一份备份文件的时间
	LastErr   string `json:"lastErr"`   // 最近一次定时备份失败的原因(成功后清空)
	LastErrAt int64  `json:"lastErrAt"` // 同上,时间
}

type attentionView struct {
	At                int64           `json:"at"`
	ResellersDepleted int64           `json:"resellersDepleted"` // 启用、额度已用尽
	ResellersExpiring int64           `json:"resellersExpiring"` // 启用、7 天内到期(还没到期)
	ResellersExpired  int64           `json:"resellersExpired"`  // 启用、过去 7 天里到期的(更早到期的不再常驻提示,代理页能看到)
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

func (s *Server) computeAttention(now time.Time) attentionView {
	ts := now.Unix()
	v := attentionView{At: ts}
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
	// 同时额度用尽又快到期的代理只算一次:「需要处理」的标题用这个数,分项仍各算各的
	s.db.Model(&model.Reseller{}).Where("enabled = ?", true).
		Where("depleted = ? OR (expiry > ? AND expiry <= ?)", true, ts-7*86400, ts+7*86400).Count(&v.ResellersAttn)
	return v
}
