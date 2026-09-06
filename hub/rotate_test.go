package hub

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 副机只在"订阅令牌换了 + 凭据换了"时才把用户当作重置过:同一份凭据换个写法、只补了协议键、新用户、被删的用户都不算。
func TestRotatedUsersDetectsOnlyRealRotation(t *testing.T) {
	node, err := database.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(node)
	node.Create(&model.User{Name: "same", Enabled: true, SubToken: "t1", Credentials: []byte(`{"hysteria2":{"name":"same","password":"p1"}}`)})
	node.Create(&model.User{Name: "rotated", Enabled: true, SubToken: "t2", Credentials: []byte(`{"hysteria2":{"name":"rotated","password":"old"}}`)})
	node.Create(&model.User{Name: "filled", Enabled: true, SubToken: "t3", Credentials: []byte(`{"hysteria2":{"name":"filled","password":"p3"},"vless":{"uuid":"u"}}`)})
	node.Create(&model.User{Name: "gone", Enabled: true, SubToken: "t4", Credentials: []byte(`{"hysteria2":{"password":"p4"}}`)})

	snap := Snapshot{Users: []model.User{
		{Name: "same", SubToken: "t1", Credentials: []byte(`{ "hysteria2": {"password": "p1", "name": "same"} }`)}, // 同一份,只是空白与键序不同
		{Name: "rotated", SubToken: "t2-new", Credentials: []byte(`{"hysteria2":{"name":"rotated","password":"new"}}`)},
		{Name: "filled", SubToken: "t3", Credentials: []byte(`{"hysteria2":{"name":"filled","password":"p3"}}`)}, // 只是本机多补了一个协议键
		{Name: "fresh", SubToken: "t5", Credentials: []byte(`{"hysteria2":{"password":"p5"}}`)},                  // 新用户
	}}
	got := RotatedUsers(node, snap)
	if len(got) != 1 || got[0] != "rotated" {
		t.Fatalf("应只识别出 rotated,实际 %v", got)
	}
	// 令牌换了但凭据没换(不会发生,但不能误断)
	snap.Users[0].SubToken = "t1-new"
	if got := RotatedUsers(node, snap); len(got) != 1 || got[0] != "rotated" {
		t.Fatalf("凭据没换就不算重置: %v", got)
	}
}
