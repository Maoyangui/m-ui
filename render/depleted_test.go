package render

import (
	"path/filepath"
	"testing"

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
