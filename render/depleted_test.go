package render

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func TestDepletedResellerUsersNotRendered(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Reseller{Name: "r", Enabled: true, Volume: 1})
	db.Create(&model.User{Name: "u", Enabled: true, ResellerId: 1, Credentials: []byte(`{"shadowsocks":{"password":"x"}}`)})
	users, _ := loadLineUsers(db, 0)
	_ = users
	db.Create(&model.Line{Name: "ss", Protocol: "shadowsocks", Port: 30012, Enabled: true, Options: []byte(`{"method":"aes-256-gcm","password":"x"}`)})
	db.Exec("INSERT INTO user_lines (user_id, line_id) VALUES (1, 1)")
	db.Create(&model.ResellerLine{ResellerId: 1, LineId: 1}) // 代理用户只能用授权给代理的线路
	byLine, err := loadLineUsers(db, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(byLine[1]) != 1 {
		t.Fatalf("额度未用尽时用户应下发: %v", byLine)
	}
	db.Model(&model.Reseller{}).Where("id = ?", 1).Update("depleted", true)
	byLine, _ = loadLineUsers(db, 0)
	if len(byLine[1]) != 0 {
		t.Fatalf("代理额度用尽后用户不该下发: %v", byLine)
	}
}

// 用户自己到期同样不下发:主机每分钟会把到期用户停用再推快照,但主机失联时副机只靠渲染层这一道(审计 M035)。
// 不限期(0)与没到期的照常。
func TestExpiredUsersNotRendered(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	now := time.Now().Unix()
	db.Create(&model.Line{Name: "ss", Protocol: "shadowsocks", Port: 30013, Enabled: true, Options: []byte(`{"method":"aes-256-gcm","password":"x"}`)})
	for i, exp := range []int64{0, now + 3600, now - 1} {
		u := model.User{Name: fmt.Sprintf("u%d", i), Enabled: true, Expiry: exp, Credentials: []byte(`{"shadowsocks":{"password":"x"}}`)}
		db.Create(&u)
		db.Create(&model.UserLine{UserId: u.Id, LineId: 1})
	}
	byLine, err := loadLineUsers(db, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, u := range byLine[1] {
		names = append(names, u.Name)
	}
	if len(names) != 2 || names[0] != "u0" || names[1] != "u1" {
		t.Fatalf("应只下发不限期与未到期的用户,实际 %v", names)
	}
}

// 临时共享关了(全局开关,或代理关了「允许临时共享」)就不再下发共享凭据:以前只是不让新生成,已有的共享照样能连(审计 MB25)。
func TestShareCredsFollowSwitches(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Line{Name: "ss", Protocol: "shadowsocks", Port: 30014, Enabled: true, Options: []byte(`{"method":"aes-256-gcm","password":"x"}`)})
	rs := model.Reseller{Name: "r", Enabled: true}
	db.Create(&rs)
	db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 1})
	for _, u := range []model.User{
		{Name: "own", Enabled: true, ShareToken: "t1", ShareCreds: []byte(`{"shadowsocks":{"password":"s1"}}`), Credentials: []byte(`{"shadowsocks":{"password":"p1"}}`)},
		{Name: "sub", Enabled: true, ResellerId: rs.Id, ShareToken: "t2", ShareCreds: []byte(`{"shadowsocks":{"password":"s2"}}`), Credentials: []byte(`{"shadowsocks":{"password":"p2"}}`)},
	} {
		db.Create(&u)
		db.Create(&model.UserLine{UserId: u.Id, LineId: 1})
	}
	names := func() map[string]bool {
		by, err := loadLineUsers(db, 0)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, u := range by[1] {
			out[u.Name] = true
		}
		return out
	}
	if n := names(); !n["own#share"] || !n["sub#share"] {
		t.Fatalf("开关都开着时共享凭据应下发: %v", n)
	}
	db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("share_on", false)
	if n := names(); !n["own#share"] || n["sub#share"] || !n["sub"] {
		t.Fatalf("代理关了允许共享:只撤他名下用户的共享凭据,本人凭据照旧: %v", n)
	}
	db.Create(&model.Setting{Key: "subShareEnabled", Value: "false"})
	if n := names(); n["own#share"] || n["sub#share"] || !n["own"] {
		t.Fatalf("全局关了临时共享:所有共享凭据都不该下发: %v", n)
	}
}
