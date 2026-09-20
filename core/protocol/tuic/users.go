package tuic

import (
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"

	"github.com/gofrs/uuid/v5"
)

// UpdateUsers 热换用户表。先校验全部 UUID,任何一个不合法就整表不动、登记表也不动;
// 然后两段式换表,关掉被移除和换了凭据(UUID 或密码)的用户的整条会话。返回关掉的会话数。
func (h *Inbound) UpdateUsers(users []option.TUICUser) (int, error) {
	userList := make([]string, 0, len(users))
	userUUIDList := make([][16]byte, 0, len(users))
	userPasswordList := make([]string, 0, len(users))
	for index, user := range users {
		if user.UUID == "" {
			return 0, E.New("missing uuid for user ", index)
		}
		userUUID, err := uuid.FromString(user.UUID)
		if err != nil {
			return 0, E.Cause(err, "invalid uuid for user ", index)
		}
		userList = append(userList, user.Name)
		userUUIDList = append(userUUIDList, userUUID)
		userPasswordList = append(userPasswordList, user.Password)
	}
	creds := tuicCreds(userList, userUUIDList, userPasswordList)
	h.sessions.Prepare(creds)
	h.server.UpdateUsers(userList, userUUIDList, userPasswordList)
	removed, changed, closed := h.sessions.Commit(creds)
	if len(removed)+len(changed) > 0 {
		h.logger.Info("用户表更新:移除 ", len(removed), " 个、凭据变更 ", len(changed), " 个,断开会话 ", closed, " 条")
	}
	return closed, nil
}

// tuicCreds 名字 → 库实际用于认证的凭据(解析后的 UUID 字节 + 密码;UUID 大小写不同算同一个)。
func tuicCreds(names []string, uuids [][16]byte, passwords []string) map[string]string {
	m := make(map[string]string, len(names))
	for i, name := range names {
		m[name] = string(uuids[i][:]) + "\x00" + passwords[i]
	}
	return m
}
