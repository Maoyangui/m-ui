package model

import "time"

// 副机证书要主机出面提醒的情况(CertBrief.Trouble)。
const (
	CertFailing  = "failing"  // 自动续期在失败
	CertExpiring = "expiring" // 正用着、面板续不了(手动 DNS / 服务器上已有的 / 关了自动续期),快到期或已过期
)

// Trouble 这张证书要不要提醒,要的话是哪种:自动续期在失败 → CertFailing;正用着又不会自动续的,手动 DNS 剩 30 天内、
// 其它剩 14 天内 → CertExpiring(会自动续的到了这一步也算:那是一直没续上)。自签证书不管。
// 老版本副机不报续期状态(Source 为空):只看线路默认证书剩不到 14 天。
func (b CertBrief) Trouble(now time.Time) string {
	if b.SelfSigned {
		return ""
	}
	if b.AutoRenew && b.RenewErr != "" {
		return CertFailing
	}
	if !b.InUse && !(b.Source == "" && b.Default) {
		return ""
	}
	limit := 14
	if b.Source == "acme" && b.Method == "manual" {
		limit = 30
	}
	if int(time.Unix(b.NotAfter, 0).Sub(now)/(24*time.Hour)) <= limit { // 按"剩 N 天"的整天数比,和证书页一个口径
		return CertExpiring
	}
	return ""
}
