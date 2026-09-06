package web

import (
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/jobs"

	"gorm.io/gorm"
)

// 谁在改 users.enabled:管理员 / 代理 / 外部 API 的手动启停、配额判定、周期重置、补量、延期、套餐。
// 以前它们互相覆盖:手动停掉的人一清流量就复活,代理超额把名下全部停掉、补量又整体复活。
// 现在停用必带原因(model.DisabledReason),自动恢复只对自动原因(超量 / 到期)生效。

// autoEnable 超量 / 到期被停的用户回到范围内后自动启用;手动停用的不碰。返回是否改了。
func autoEnable(u *model.User, now int64) bool {
	if u.Enabled || u.DisabledReason == model.DisabledManual {
		return false
	}
	if u.Volume > 0 && u.Up+u.Down >= u.Volume {
		return false
	}
	if u.Expiry > 0 && u.Expiry < now {
		return false
	}
	u.Enabled, u.DisabledReason = true, ""
	return true
}

// setEnabled 手动启停:停用记 manual,启用清原因。
func (s *Server) setEnabled(db *gorm.DB, ids []uint, on bool) (int64, error) {
	upd := map[string]interface{}{"enabled": on, "disabled_reason": ""}
	if !on {
		upd["disabled_reason"] = model.DisabledManual
	}
	res := db.Model(&model.User{}).Where("id IN ?", ids).Updates(upd)
	return res.RowsAffected, res.Error
}

// resetUsage 本周期用量清零(并入历史累计),超量被停的自动恢复,并允许用量告警再次发出。
func (s *Server) resetUsage(db *gorm.DB, id uint) error {
	if err := db.Model(&model.User{}).Where("id = ?", id).Updates(map[string]interface{}{
		"total_up": gorm.Expr("total_up + up"), "total_down": gorm.Expr("total_down + down"), "up": 0, "down": 0,
	}).Error; err != nil {
		return err
	}
	var u model.User
	if err := db.First(&u, id).Error; err != nil {
		return err
	}
	if autoEnable(&u, time.Now().Unix()) {
		if err := db.Model(&model.User{}).Where("id = ?", id).Updates(map[string]interface{}{"enabled": true, "disabled_reason": ""}).Error; err != nil {
			return err
		}
	}
	s.forgetQuotaAlert(u.Name)
	return nil
}

// extendExpiry 到期顺延 N 天(原到期未过就从原到期算),到期被停的自动恢复。
func (s *Server) extendExpiry(db *gorm.DB, u model.User, days int, now int64) error {
	base := now
	if u.Expiry > now {
		base = u.Expiry
	}
	u.Expiry = base + int64(days)*86400
	autoEnable(&u, now)
	return db.Model(&model.User{}).Where("id = ?", u.Id).Select("expiry", "enabled", "disabled_reason").Updates(u).Error
}

// forgetQuotaAlert 用量清零后允许"流量告急"再次提醒(去重键 24 小时才过期)。
func (s *Server) forgetQuotaAlert(name string) {
	if s.run != nil {
		s.run.Notifier().Forget("quota:" + name)
	}
}

// refreshReseller 改了代理额度或重置了流量之后,立刻重算"额度用尽"标记,不等下一分钟的判定。
func (s *Server) refreshReseller(id uint) {
	jobs.RefreshReseller(s.db, id)
}
