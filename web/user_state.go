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

// ensureDisabled 建号时勾了停用:gorm 的 default:true 把 Create 里的 false 写成了 true(结构体里也被回填成 true),
// 插入后按记下的本意写回,并把结构体改回来给响应用。
func ensureDisabled(db *gorm.DB, u *model.User, disabled bool) error {
	if !disabled {
		return nil
	}
	u.Enabled, u.DisabledReason = false, model.DisabledManual
	return db.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{
		"enabled": false, "disabled_reason": model.DisabledManual,
	}).Error
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

// extendExpiry 到期顺延 N 天(原到期未过就从原到期算),到期被停的自动恢复;手动停用的仍停着。
// 不限期的不动、返回 false:以前也从现在起算,批量延期 30 天等于给它们设了 30 天后到期(审计 MB11)。
func (s *Server) extendExpiry(db *gorm.DB, u model.User, days int, now int64) (bool, error) {
	if u.Expiry == 0 {
		return false, nil
	}
	base := now
	if u.Expiry > now {
		base = u.Expiry
	}
	u.Expiry = base + int64(days)*86400
	autoEnable(&u, now)
	return true, db.Model(&model.User{}).Where("id = ?", u.Id).Select("expiry", "enabled", "disabled_reason").Updates(u).Error
}

// 流量时序按用户名记(resource=user, tag=用户名):删号要一并删掉,改名要跟着搬。以前都不管,以后同名的新用户
// 在落地页「用量情况」里看得到前一个人的流量历史,改了名的人反而看不到自己的(审计 MB28)。
func dropUserStats(tx *gorm.DB, name string) error {
	return tx.Where("resource = ? AND tag = ?", "user", name).Delete(&model.Stats{}).Error
}

func moveUserStats(tx *gorm.DB, from, to string) error {
	if from == to {
		return nil
	}
	if err := dropUserStats(tx, to); err != nil { // 新名字下若有前人留下的孤儿行,先清掉,免得并到一起
		return err
	}
	return tx.Model(&model.Stats{}).Where("resource = ? AND tag = ?", "user", from).Update("tag", to).Error
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
