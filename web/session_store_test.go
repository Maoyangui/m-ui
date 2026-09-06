package web

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
)

// 会话落库:换一个 Server 实例(等于进程重启)仍然认得登录态;登出后两边都失效。
func TestSessionSurvivesRestart(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	a := &Server{db: db}
	tok := a.newSession("admin")
	if !a.validSession(tok) {
		t.Fatal("刚登录的会话应有效")
	}
	b := &Server{db: db} // "重启"后的新实例:内存里什么都没有
	if !b.validSession(tok) {
		t.Fatal("重启后会话应从库里找回来")
	}
	b.delSession(tok) // 登出:缓存与库一起删
	if b.validSession(tok) {
		t.Fatal("登出后会话应失效")
	}
	c := &Server{db: db} // 再"重启"一次:库里也没有了
	if c.validSession(tok) {
		t.Fatal("登出后的会话不该再从库里找回来")
	}
}
