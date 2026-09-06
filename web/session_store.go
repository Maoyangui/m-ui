package web

import (
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 会话:内存 map 是缓存,库里的 sessions 表是事实。登录写库,校验先看缓存、没有再查库,
// 登出与过期从两边删。这样一键更新、重启、菜单里重启之后,管理员和代理都还在登录状态。

func (s *Server) putSession(token string, sess session) {
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]session{}
	}
	s.sessions[token] = sess
	s.mu.Unlock()
	if s.db != nil {
		s.db.Save(&model.Session{Token: token, User: sess.user, Reseller: sess.reseller, Pending: sess.pending, Exp: sess.exp.Unix()})
	}
}

// getSession 取会话;过期的顺手删掉。
func (s *Server) getSession(token string) (session, bool) {
	if token == "" {
		return session{}, false
	}
	s.mu.Lock()
	sess, ok := s.sessions[token]
	s.mu.Unlock()
	if !ok && s.db != nil {
		var row model.Session
		if err := s.db.Where("token = ?", token).First(&row).Error; err == nil {
			sess = session{user: row.User, reseller: row.Reseller, pending: row.Pending, exp: time.Unix(row.Exp, 0)}
			ok = true
			s.mu.Lock()
			if s.sessions == nil {
				s.sessions = map[string]session{}
			}
			s.sessions[token] = sess
			s.mu.Unlock()
		}
	}
	if !ok {
		return session{}, false
	}
	if time.Now().After(sess.exp) {
		s.delSession(token)
		return session{}, false
	}
	return sess, true
}

func (s *Server) delSession(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
	if s.db != nil {
		s.db.Where("token = ?", token).Delete(&model.Session{})
	}
}

// updateSession 改一个会话的字段(代理设完密码后清掉待设标记)。
func (s *Server) updateSession(token string, fn func(*session)) {
	sess, ok := s.getSession(token)
	if !ok {
		return
	}
	fn(&sess)
	s.putSession(token, sess)
}
