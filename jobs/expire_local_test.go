package jobs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 副机按本机时间撤下到期的用户 / 代理(审计 M022 / M035;用户 2026-09-29 拍板):主机失联时什么都不推,
// 代理到期也不改快照修订号,数据面不会自己重载。第一轮只记下(启动时的渲染已经撤下了);出现新到期的才热更新;
// 集合缩小(续费)不重载。主机不走这一路。
func TestNodeExpiresLocally(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	now := time.Now().Unix()
	db.Create(&model.User{Name: "old", Enabled: true, Expiry: now - 3600, Credentials: []byte(`{}`)})
	soon := model.User{Name: "soon", Enabled: true, Expiry: now + 3600, Credentials: []byte(`{}`)}
	db.Create(&soon)
	rs := model.Reseller{Name: "rs", Enabled: true, Expiry: now + 3600}
	db.Create(&rs)

	reloads := 0
	isNode := true
	s := New(Deps{DB: db, IsNode: func() bool { return isNode }, ReloadUsers: func() error { reloads++; return nil }})
	s.runDeplete()
	if reloads != 0 {
		t.Fatalf("第一轮只该记下,启动时的渲染已经按到期时间撤下了,实际重载 %d 次", reloads)
	}
	db.Model(&model.User{}).Where("id = ?", soon.Id).Update("expiry", now-1)
	s.runDeplete()
	if reloads != 1 {
		t.Fatalf("用户到期应热更新一次,实际 %d 次", reloads)
	}
	s.runDeplete()
	if reloads != 1 {
		t.Fatalf("没有新到期的不该再重载,实际 %d 次", reloads)
	}
	db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("expiry", now-1)
	s.runDeplete()
	if reloads != 2 {
		t.Fatalf("代理到期应热更新一次,实际累计 %d 次", reloads)
	}
	db.Model(&model.User{}).Where("id = ?", soon.Id).Update("expiry", now+86400) // 续费:集合缩小
	s.runDeplete()
	if reloads != 2 {
		t.Fatalf("续费(集合缩小)不该重载,快照应用时已重载,实际累计 %d 次", reloads)
	}
}
