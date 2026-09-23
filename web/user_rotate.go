package web

import (
	"fmt"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
)

// 重置订阅链接:订阅地址换成一串新的随机令牌(不管设置里是否用用户名作地址),
// 各协议的凭据全部重新生成,临时共享一并收回。旧地址、旧凭据、旧共享地址立即失效,
// 已连上的设备被断开,用户要重新导入新地址。主面板、代理面板、外部 API(两种作用域)都走这里;
// 副机收到快照后按凭据变化自行断开旧连接(hub.RotatedUsers)。

// rotateUser 重置订阅链接:随机新令牌 + 全部凭据换新 + 收回临时共享,旧的立即失效。
//
// 每次重置都无条件换新。用户手上的链接泄露了才会来点它,返回一份"上次已经换过"的旧链接等于什么都没做。
// 0.6.10 用一个 credentialRotationPending 标记做两阶段:任一副机没确认就留着标记、下次重置跳过换凭据;
// 而没有任何后台循环会清这个标记 —— 副机失联(常态)一次,以后对这个用户的重置就都是假的,页面却报成功。
// 同一版还同步等所有副机确认:一台副机被黑洞就等 25 秒以上,然后回 400,管理员拿不到新链接;
// 代理端还能从报错里看到副机的 API 地址和面板路径。
//
// 现在的顺序:落库 → 本机热更新新凭据 → 本机断开旧凭据的连接 → 立刻返回新链接;
// 副机走正常的 5 秒同步,收到新凭据后按 RotatedUsers 自己踢线(0.6.9 就是这么做的),这里只顺手催一次、不等结果。
func (s *Server) rotateUser(u model.User) (model.User, error) {
	s.rotateMu.Lock()
	defer s.rotateMu.Unlock()
	if err := s.db.First(&u, u.Id).Error; err != nil {
		return u, err
	}
	u.SubToken = randomSubToken()
	u.Credentials = generateCredentials(u.Name)
	u.ShareToken, u.ShareCreds, u.ShareAt = "", nil, 0
	if err := s.db.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{
		"sub_token": u.SubToken, "credentials": []byte(u.Credentials),
		"share_token": "", "share_creds": nil, "share_at": 0,
	}).Error; err != nil {
		return u, err
	}
	// 0.6.10 遗留的两阶段标记顺手清掉,免得库里越积越多
	s.db.Where("key = ?", fmt.Sprintf("credentialRotationPending:%d", u.Id)).Delete(&model.Setting{})
	if s.run == nil { // 测试里没有数据面
		return u, nil
	}
	// 新凭据先热更新进本机数据面,再断开旧凭据上的连接 —— 旧凭据重连也进不来。
	// 热更新没全做成不拦着:标记留着由 secureReloadLoop 退避重试,旧凭据在已更新的入站上已经失效。
	if err := s.run.ReloadUsersSecure(); err != nil {
		logger.Warning("重置 ", u.Name, " 的凭据后热更新未全部完成(后台会重试): ", err)
	}
	closed, sessions := s.run.KickUserLocal(u.Name)
	logger.Info("已重置 ", u.Name, " 的订阅链接与凭据,本机断开 ", closed, " 条连接、", sessions, " 条会话")
	if h := s.run.Hub(); h != nil {
		go func() {
			if err := h.SyncNow(); err != nil {
				logger.Warning("重置 ", u.Name, " 后向副机同步未全部确认(同步循环会继续重推): ", err)
			}
		}()
	}
	return u, nil
}
