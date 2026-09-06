package web

import (
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
)

// 重置订阅链接:订阅地址换成一串新的随机令牌(不管设置里是否用用户名作地址),
// 各协议的凭据全部重新生成,临时共享一并收回。旧地址、旧凭据、旧共享地址立即失效,
// 已连上的设备被断开,用户要重新导入新地址。主面板、代理面板、外部 API(两种作用域)都走这里;
// 副机收到快照后按凭据变化自行断开旧连接(hub.RotatedUsers)。

// rotateUser 落库并刷新本机数据面;返回更新后的用户。审计由调用方按各自的操作者记。
func (s *Server) rotateUser(u model.User) (model.User, error) {
	u.SubToken = randomSubToken()
	u.Credentials = generateCredentials(u.Name)
	u.ShareToken, u.ShareCreds, u.ShareAt = "", nil, 0
	if err := s.db.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{
		"sub_token": u.SubToken, "credentials": []byte(u.Credentials),
		"share_token": "", "share_creds": nil, "share_at": 0,
	}).Error; err != nil {
		return u, err
	}
	if s.run == nil { // 测试里没有数据面
		return u, nil
	}
	name := u.Name
	go func() { // 先把新凭据热更新进数据面,再断开旧凭据上的连接:旧凭据重连也进不来
		if err := s.run.ReloadUsers(); err != nil {
			logger.Warning("重置订阅链接后热更新用户失败: ", err)
			return
		}
		logger.Info("已重置 ", name, " 的订阅链接与凭据,断开 ", s.run.KickUser(name), " 条连接")
	}()
	return u, nil
}
