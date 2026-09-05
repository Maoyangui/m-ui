package render

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 临时共享的凭据以 "本人名#share" 进入入站:core 的追踪器记账时去掉后缀归到本人,
// 所以借用者的流量与设备数都算在本人名下;名字不同又让取消共享时只断借用者的连接。
// 取消共享后这份凭据必须立刻从配置里消失。
func TestShareCredsRenderedUnderOwnerName(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)

	line := model.Line{Name: "hy2", Protocol: "hysteria2", Port: 20101, Enabled: true}
	db.Create(&line)
	u := model.User{Name: "alice", Enabled: true,
		Credentials: []byte(`{"hysteria2":{"password":"own"}}`),
		ShareToken:  "tok", ShareCreds: []byte(`{"hysteria2":{"password":"lent"}}`)}
	db.Create(&u)
	db.Create(&model.UserLine{UserId: u.Id, LineId: line.Id})

	users := func() []map[string]interface{} {
		byLine, err := loadLineUsers(db)
		if err != nil {
			t.Fatal(err)
		}
		out, err := renderUsers(line, map[string]interface{}{}, byLine[line.Id])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	got := users()
	if len(got) != 2 {
		t.Fatalf("共享开启时应有两份凭据: %v", got)
	}
	if got[0]["name"] != "alice" || got[1]["name"] != "alice"+model.ShareSuffix {
		t.Fatalf("本人凭据叫 alice、共享凭据叫 alice#share: %v", got)
	}
	if model.Owner(got[1]["name"].(string)) != "alice" {
		t.Fatal("记账名应去掉后缀归到本人")
	}
	if got[0]["password"] != "own" || got[1]["password"] != "lent" {
		t.Fatalf("两份凭据应不同: %v", got)
	}

	// 取消共享
	db.Model(&model.User{}).Where("id = ?", u.Id).
		Updates(map[string]interface{}{"share_token": "", "share_creds": json.RawMessage(nil)})
	if got = users(); len(got) != 1 || got[0]["password"] != "own" {
		t.Fatalf("取消后只应剩本人凭据: %v", got)
	}
}
