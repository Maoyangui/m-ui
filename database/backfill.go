package database

import (
	"encoding/json"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// planBackfillKey 老用户归入套餐只做一次。
const planBackfillKey = "planBackfillDone"

// backfillUserPlans 升级到记录「所属套餐」的版本时,给老用户补上归属:限速规则要能按套餐选到现有用户。
// 老版本没记过,只能推断,宁缺毋滥:
//  1. 操作记录里最后一次给这个用户套用的套餐;
//  2. 否则额度与限速和某一个套餐完全一样,且只有这一个对得上。
//
// 推断不出来的留空,管理员可以在用户编辑框或批量「只归入这个套餐」里补。副机没有套餐表,不做。
func backfillUserPlans(db *gorm.DB) {
	var done, nodeMode string
	db.Raw("SELECT value FROM settings WHERE key = ?", planBackfillKey).Scan(&done)
	db.Raw("SELECT value FROM settings WHERE key = ?", "nodeMode").Scan(&nodeMode)
	if done == "true" || strings.EqualFold(nodeMode, "true") {
		return
	}
	var plans []model.Plan
	if db.Find(&plans).Error != nil {
		return
	}
	var users []model.User
	if db.Select("id, name, reseller_id, plan_id, volume, device_limit, speed_up, speed_down, auto_reset, reset_days").
		Where("plan_id = 0").Find(&users).Error != nil {
		return
	}
	byName := map[uint]map[string]uint{} // 归属 → 套餐名 → id
	for _, p := range plans {
		if byName[p.ResellerId] == nil {
			byName[p.ResellerId] = map[string]uint{}
		}
		byName[p.ResellerId][p.Name] = p.Id
	}
	logged := planFromAudit(db)
	err := db.Transaction(func(tx *gorm.DB) error {
		for _, u := range users {
			id := byName[u.ResellerId][logged[u.Name]]
			if id == 0 {
				id = planByLimits(u, plans)
			}
			if id == 0 {
				continue
			}
			if err := tx.Model(&model.User{}).Where("id = ?", u.Id).Update("plan_id", id).Error; err != nil {
				return err
			}
		}
		return tx.Create(&model.Setting{Key: planBackfillKey, Value: "true"}).Error
	})
	_ = err // 失败下次启动再来一遍,不影响启动
}

// planFromAudit 操作记录里每个用户最后一次套用的套餐名。动作的写法见套用套餐的几处审计:
// plan:renew:<名字>、plan:extend:<名字>、create:plan:<名字>、update:plan:<名字>。
func planFromAudit(db *gorm.DB) map[string]string {
	out := map[string]string{}
	var rows []model.Change
	if db.Select("action, obj").Where("key = ? AND action LIKE ?", "user", "%plan:%").Order("id asc").Find(&rows).Error != nil {
		return out
	}
	for _, r := range rows {
		name := planNameInAction(r.Action)
		if name == "" {
			continue
		}
		var user string
		if json.Unmarshal(r.Obj, &user) == nil && user != "" {
			out[user] = name
		}
	}
	return out
}

func planNameInAction(action string) string {
	if strings.HasPrefix(action, "plan:") {
		parts := strings.SplitN(action, ":", 3)
		if len(parts) == 3 {
			return parts[2]
		}
		return ""
	}
	if i := strings.Index(action, ":plan:"); i >= 0 {
		return action[i+len(":plan:"):]
	}
	return ""
}

// planByLimits 额度与限速和某个套餐完全一样,且只有这一个对得上才算;什么限制都没有的套餐不认(谁都对得上)。
func planByLimits(u model.User, plans []model.Plan) uint {
	var found uint
	for _, p := range plans {
		if p.ResellerId != u.ResellerId {
			continue
		}
		if p.VolumeGB == 0 && p.DeviceLimit == 0 && p.SpeedUp == 0 && p.SpeedDown == 0 {
			continue
		}
		if u.Volume != model.GBToBytes(p.VolumeGB) || u.DeviceLimit != p.DeviceLimit || u.SpeedUp != p.SpeedUp || u.SpeedDown != p.SpeedDown {
			continue
		}
		if u.AutoReset != p.AutoReset || (p.AutoReset && u.ResetDays != p.ResetDays) {
			continue
		}
		if found != 0 {
			return 0 // 对得上的不止一个,猜不准
		}
		found = p.Id
	}
	return found
}
