package ha

import (
	"encoding/json"

	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// 备用发通知:副机一般没配通知,主机只把 Telegram / Webhook 设置封在备用公钥下随容灾状态发给备用(haNotify),
// 用的时候在内存里解开。令牌不落明文、不进日志。

func (s *Service) standbyNotifySettings() map[string]string {
	sealed := s.setting(KeyNotify)
	if sealed == "" {
		return nil
	}
	k, err := s.standbyKey()
	if err != nil {
		return nil
	}
	plain, err := OpenWith(sealed, k)
	if err != nil {
		return nil
	}
	m := map[string]string{}
	if json.Unmarshal(plain, &m) != nil {
		return nil
	}
	return m
}

func (s *Service) standbyNotifySetting(key string) string {
	return s.standbyNotifySettings()[key]
}

// standbyNotifier 用主机交给本机的通知设置建一个发通知的(没收到过就是 nil)。
func (s *Service) standbyNotifier() *notify.Notifier {
	m := s.standbyNotifySettings()
	if m == nil {
		return nil
	}
	return notify.New(func(k string) string { return m[k] })
}

// standbyNotify 备用发一条通知(Telegram 与 Webhook,配了哪个发哪个;在后台发,不挡判定)。
func (s *Service) standbyNotify(text string) {
	n := s.standbyNotifier()
	if n == nil {
		logger.Info("容灾:本机没有收到主机的通知设置,这条通知只写日志")
		return
	}
	n.Event("", text)
}
