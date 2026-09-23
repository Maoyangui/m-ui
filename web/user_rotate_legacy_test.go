package web

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 0.6.10 用 credentialRotationPending:<id> 做两阶段重置:任一副机没确认就留着标记、下次重置跳过换凭据,
// 而没有任何后台循环会清这个标记。副机失联(常态)一次,以后对这个用户的重置就都是假的 —— 页面报成功,
// 返回的却是上一次的链接。这条钉住:不管库里有没有遗留标记,每次重置都必须换新,并把标记清掉。
func TestRotateUserIgnoresLegacyPendingMarker(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	u := model.User{Name: "carol", Enabled: true, SubToken: "cccccccccccccccccccccccc", Credentials: generateCredentials("carol")}
	db.Create(&u)
	key := fmt.Sprintf("credentialRotationPending:%d", u.Id)
	db.Create(&model.Setting{Key: key, Value: "true"})

	before := string(u.Credentials)
	nu, err := s.rotateUser(u)
	if err != nil {
		t.Fatalf("重置不该因为遗留标记失败: %v", err)
	}
	if nu.SubToken == "cccccccccccccccccccccccc" || string(nu.Credentials) == before {
		t.Fatal("遗留的两阶段标记不能让重置变成空操作:令牌与凭据都必须换新")
	}
	var left int64
	db.Model(&model.Setting{}).Where("key = ?", key).Count(&left)
	if left != 0 {
		t.Fatal("遗留标记应被清掉")
	}
	// 再来一次:照样换新
	again, err := s.rotateUser(nu)
	if err != nil {
		t.Fatal(err)
	}
	if again.SubToken == nu.SubToken || string(again.Credentials) == string(nu.Credentials) {
		t.Fatal("每次重置都要换新")
	}
}
