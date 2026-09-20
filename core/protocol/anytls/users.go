package anytls

import (
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"

	anytls "github.com/anytls/sing-anytls"
)

// UpdateUsers 热换用户表。两段式:先把新名字放进登记表,再让库换表,最后按名字比对密码、
// 关掉被移除和换了密码的用户的整条 TLS 会话。用户表没变时一个会话都不关。返回关掉的会话数。
func (h *Inbound) UpdateUsers(users []option.AnyTLSUser) (int, error) {
	creds := anytlsCreds(users)
	h.sessions.Prepare(creds)
	h.service.UpdateUsers(common.Map(users, func(it option.AnyTLSUser) anytls.User {
		return (anytls.User)(it)
	}))
	removed, changed, closed := h.sessions.Commit(creds)
	if len(removed)+len(changed) > 0 {
		h.logger.Info("用户表更新:移除 ", len(removed), " 个、凭据变更 ", len(changed), " 个,断开会话 ", closed, " 条")
	}
	return closed, nil
}

// anytlsCreds 名字 → 库实际用于认证的凭据(密码)。
func anytlsCreds(users []option.AnyTLSUser) map[string]string {
	m := make(map[string]string, len(users))
	for _, u := range users {
		m[u.Name] = u.Password
	}
	return m
}
