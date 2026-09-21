package web

import (
	"fmt"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 重置订阅链接:订阅地址换成一串新的随机令牌(不管设置里是否用用户名作地址),
// 各协议的凭据全部重新生成,临时共享一并收回。旧地址、旧凭据、旧共享地址立即失效,
// 已连上的设备被断开,用户要重新导入新地址。主面板、代理面板、外部 API(两种作用域)都走这里;
// 副机收到快照后按凭据变化自行断开旧连接(hub.RotatedUsers)。

// rotateUser 落库并刷新本机数据面;返回更新后的用户。审计由调用方按各自的操作者记。
func (s *Server) rotateUser(u model.User) (model.User, error) {
	s.rotateMu.Lock()
	defer s.rotateMu.Unlock()
	if err := s.db.First(&u, u.Id).Error; err != nil {
		return u, err
	}
	pendingKey := fmt.Sprintf("credentialRotationPending:%d", u.Id)
	var pending string
	if err := s.db.Raw("SELECT value FROM settings WHERE key = ?", pendingKey).Scan(&pending).Error; err != nil {
		return u, fmt.Errorf("读取凭据轮换状态失败: %w", err)
	}
	if pending != "true" {
		if s.run == nil {
			u.SubToken = randomSubToken()
			u.Credentials = generateCredentials(u.Name)
			u.ShareToken, u.ShareCreds, u.ShareAt = "", nil, 0
			if err := s.db.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{
				"sub_token": u.SubToken, "credentials": []byte(u.Credentials),
				"share_token": "", "share_creds": nil, "share_at": 0,
			}).Error; err != nil {
				return u, err
			}
			return u, nil
		}
		u.SubToken = randomSubToken()
		u.Credentials = generateCredentials(u.Name)
		u.ShareToken, u.ShareCreds, u.ShareAt = "", nil, 0
		if err := s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{
				"sub_token": u.SubToken, "credentials": []byte(u.Credentials),
				"share_token": "", "share_creds": nil, "share_at": 0,
			}).Error; err != nil {
				return err
			}
			return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&model.Setting{Key: pendingKey, Value: "true"}).Error
		}); err != nil {
			return u, fmt.Errorf("保存新凭据及轮换状态失败: %w", err)
		}
		pending = "true"
	}
	if s.run == nil { // 测试里没有数据面
		return u, nil
	}
	if err := s.run.ReloadUsersSecure(); err != nil {
		return u, err
	}
	if h := s.run.Hub(); h != nil {
		if err := h.SyncNow(); err != nil {
			return u, fmt.Errorf("新凭据已保存，本机已撤销但副机未确认，将自动重试: %w", err)
		}
	}
	res := s.run.KickUserAll(u.Name)
	if res.Failed > 0 {
		return u, fmt.Errorf("新凭据已应用，但 %d 台副机尚未确认踢线，请重试", res.Failed)
	}
	if err := s.db.Where("key = ?", pendingKey).Delete(&model.Setting{}).Error; err != nil {
		// 新凭据已经生效但状态清理失败时保留 pending 标记，下一轮
		// 会重复确认副机与踢线，不把“已完成”误记成可丢弃状态。
		return u, fmt.Errorf("凭据已应用但清理轮换状态失败: %w", err)
	}
	logger.Info("已重置 ", u.Name, " 的订阅链接与凭据,断开 ", res.Closed, " 条连接")
	return u, nil
}
