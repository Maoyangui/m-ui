package importer

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// UsersSummary 只导入用户的结果。
type UsersSummary struct {
	Created  int      `json:"created"`
	Updated  int      `json:"updated"`
	Assigned int      `json:"assigned"` // 给新用户分配的线路关系数
	Skipped  []string `json:"skipped"`  // 跳过的用户(名称非法)
}

// ImportUsersOnly 只把旧面板库里的用户并入现有 m-ui 库,线路 / 上游 / 设置一律不动:
// 同名用户更新启停、配额、到期、用量、周期重置与备注(保留 m-ui 里的凭据与线路);
// 不存在的用户新建,凭据以旧库为准、缺的协议自动补齐;assignAll 为真时给新用户分配全部现有线路。
// 适合"已经在 m-ui 上建好线路,只想把老用户搬过来"的场景。
func ImportUsersOnly(from string, dst *gorm.DB, assignAll bool) (UsersSummary, error) {
	src, err := database.OpenReadOnly(from)
	if err != nil {
		return UsersSummary{}, fmt.Errorf("打开旧面板数据库: %w", err)
	}
	defer database.Close(src)
	return ImportUsersInto(src, dst, assignAll)
}

// ImportUsersInto 同 ImportUsersOnly,但源库已打开。
func ImportUsersInto(src, dst *gorm.DB, assignAll bool) (UsersSummary, error) {
	var sum UsersSummary
	if !src.Migrator().HasTable("clients") {
		return sum, fmt.Errorf("不是旧面板数据库:没有 clients 表")
	}
	var clients []oldClient
	if err := src.Raw(`SELECT id,enable,name,config,inbounds,volume,expiry,up,down,
		total_up,total_down,"desc",remark,created_at,online_at,
		auto_reset,reset_days,next_reset FROM clients ORDER BY id`).Scan(&clients).Error; err != nil {
		return sum, fmt.Errorf("读取客户端: %w", err)
	}
	var lineIds []uint
	if assignAll {
		dst.Model(&model.Line{}).Order("sort asc, id asc").Pluck("id", &lineIds)
	}
	now := time.Now().Unix()
	err := dst.Transaction(func(tx *gorm.DB) error {
		for _, c := range clients {
			name := strings.TrimSpace(c.Name)
			if name == "" || strings.ContainsAny(name, "/?#& ") {
				sum.Skipped = append(sum.Skipped, c.Name)
				continue
			}
			var existing model.User
			if err := tx.Where("name = ?", name).First(&existing).Error; err == nil {
				upd := map[string]interface{}{
					"enabled": c.Enable, "volume": c.Volume, "expiry": c.Expiry,
					"up": c.Up, "down": c.Down, "total_up": c.TotalUp, "total_down": c.TotalDown,
					"auto_reset": c.AutoReset, "reset_days": c.ResetDays, "next_reset": c.NextReset,
				}
				if c.Remark != "" {
					upd["remark"] = c.Remark
				}
				if c.Desc != "" {
					upd["desc"] = c.Desc
				}
				if err := tx.Model(&model.User{}).Where("id = ?", existing.Id).Updates(upd).Error; err != nil {
					return fmt.Errorf("更新用户 %q: %w", name, err)
				}
				sum.Updated++
				continue
			}
			u := model.User{
				Enabled: c.Enable, Name: name, Credentials: mergeCredentials(name, c.Config),
				Volume: c.Volume, Expiry: c.Expiry, Up: c.Up, Down: c.Down, TotalUp: c.TotalUp, TotalDown: c.TotalDown,
				AutoReset: c.AutoReset, ResetDays: c.ResetDays, NextReset: c.NextReset,
				Remark: c.Remark, Desc: c.Desc, CreatedAt: c.CreatedAt, OnlineAt: c.OnlineAt,
			}
			if u.CreatedAt == 0 {
				u.CreatedAt = now
			}
			if err := tx.Create(&u).Error; err != nil {
				return fmt.Errorf("写入用户 %q: %w", name, err)
			}
			if !c.Enable { // gorm 的 default:true 会把 false 当零值写成 true,显式改回
				if err := tx.Model(&model.User{}).Where("id = ?", u.Id).Update("enabled", false).Error; err != nil {
					return err
				}
			}
			sum.Created++
			for _, lid := range lineIds {
				if err := tx.Create(&model.UserLine{UserId: u.Id, LineId: lid}).Error; err != nil {
					return err
				}
				sum.Assigned++
			}
		}
		return nil
	})
	return sum, err
}

// mergeCredentials 以自动生成的全协议凭据为底,覆盖旧库里已有的协议凭据:
// 老客户端配置继续可用,新协议(tuic / anytls 等)也有凭据。
func mergeCredentials(name string, old []byte) json.RawMessage {
	base := map[string]interface{}{}
	for k, v := range creds.Generate(name) {
		base[k] = v
	}
	if filtered, err := filterCredentials(old); err == nil {
		var m map[string]interface{}
		if json.Unmarshal(filtered, &m) == nil {
			for k, v := range m {
				base[k] = v
			}
		}
	}
	b, _ := json.Marshal(base)
	return b
}
