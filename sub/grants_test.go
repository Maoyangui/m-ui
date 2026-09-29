package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 主面板收回代理的某条线路后,代理名下用户的订阅与落地页立即不再出现它(用户自己的分配不删,重新授权即回来)。
func TestSubscriptionFollowsResellerGrant(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Line{Name: "日本2", Protocol: "anytls", Port: 444, Enabled: true})
	rs := model.Reseller{Name: "r", Enabled: true, PageEnabled: true}
	db.Create(&rs)
	db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 1})
	db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 2})
	u := model.User{Name: "ru", Enabled: true, ResellerId: rs.Id, SubToken: "tok-ru-0123456789abcdef",
		Credentials: []byte(`{"hysteria2":{"password":"p"},"anytls":{"password":"p"}}`)}
	db.Create(&u)
	db.Create(&model.UserLine{UserId: u.Id, LineId: 1})
	db.Create(&model.UserLine{UserId: u.Id, LineId: 2})

	if body := doReq(s, "GET", "/sub/"+u.SubToken+"?format=clash", "clash").Body.String(); !strings.Contains(body, "日本2") {
		t.Fatal("授权内的线路应出现在订阅里")
	}
	db.Where("reseller_id = ? AND line_id = ?", rs.Id, 2).Delete(&model.ResellerLine{})
	for _, ua := range []string{"clash", "v2rayN", browserUA} {
		w := doReq(s, "GET", "/sub/"+u.SubToken+"?format=clash", ua)
		if w.Code != 200 || strings.Contains(w.Body.String(), "日本2") || !strings.Contains(w.Body.String(), "香港1") {
			t.Fatalf("收回后 %s 拿到的内容不该再有日本2(得 %d)", ua, w.Code)
		}
	}
	// 主面板用户不受代理授权影响
	db.Create(&model.UserLine{UserId: 1, LineId: 2})
	db.Model(&model.User{}).Where("id = ?", 1).Update("credentials", u.Credentials)
	if body := doReq(s, "GET", "/sub/alice?format=clash", "clash").Body.String(); !strings.Contains(body, "日本2") {
		t.Fatal("主面板用户的线路不该被过滤")
	}
}
