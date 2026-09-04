package web

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 代理面板的隔离与额度:线路不能越权、设备数不能超总额、别家的用户碰不到。
func TestResellerScope(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}

	db.Create(&model.Line{Name: "香港1", Protocol: "hysteria2", Port: 443, Enabled: true})
	db.Create(&model.Line{Name: "日本2", Protocol: "anytls", Port: 444, Enabled: true})
	a := model.Reseller{Name: "a", Enabled: true, DeviceLimit: 5, Volume: 100 << 30}
	b := model.Reseller{Name: "b", Enabled: true}
	db.Create(&a)
	db.Create(&b)
	s.setResellerLines(a.Id, []uint{1}) // a 只有香港1

	// 建用户:线路必须在授权范围内
	u := model.User{Name: "u1", Enabled: true, DeviceLimit: 3}
	if err := s.prepareResellerUser(a.Id, &u, []uint{2}); err == nil {
		t.Fatal("未授权线路应被拒")
	}
	if err := s.prepareResellerUser(a.Id, &u, []uint{1}); err != nil {
		t.Fatalf("授权线路应通过: %v", err)
	}
	if u.ResellerId != a.Id || len(u.SubToken) < 20 {
		t.Fatalf("应归到代理名下并发订阅令牌: %+v", u)
	}
	db.Create(&u)

	// 设备总额 5:再建一个 3 台就超了
	u2 := model.User{Name: "u2", Enabled: true, DeviceLimit: 3}
	if err := s.checkResellerUser(a.Id, 0, &u2, []uint{1}); err == nil {
		t.Fatal("设备数超额应被拒")
	}
	u2.DeviceLimit = 2
	if err := s.checkResellerUser(a.Id, 0, &u2, []uint{1}); err != nil {
		t.Fatalf("刚好用满应通过: %v", err)
	}
	u2.DeviceLimit = 0
	if err := s.checkResellerUser(a.Id, 0, &u2, []uint{1}); err == nil {
		t.Fatal("代理有设备总额时,用户不能不限设备")
	}
	// 改自己这个用户时,先扣掉它原来的占用
	u.DeviceLimit = 5
	if err := s.checkResellerUser(a.Id, u.Id, &u, []uint{1}); err != nil {
		t.Fatalf("改自己应按扣除后的余额算: %v", err)
	}

	// 流量用尽 → 不给再建号
	db.Model(&model.User{}).Where("id = ?", u.Id).Update("up", int64(100)<<30)
	if err := s.checkResellerUser(a.Id, 0, &u2, []uint{1}); err == nil {
		t.Fatal("代理流量用尽应被拒")
	}
	db.Model(&model.User{}).Where("id = ?", u.Id).Update("up", 0)

	// 停用的代理不能再建号
	db.Model(&model.Reseller{}).Where("id = ?", b.Id).Update("enabled", false)
	if err := s.checkResellerUser(b.Id, 0, &u2, nil); err == nil {
		t.Fatal("停用的代理应被拒")
	}
}

// 越权访问:代理只能碰自己名下的用户,批量接口一概不给。
func TestResellerGuard(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.User{Name: "mine", Enabled: true, ResellerId: 1})
	db.Create(&model.User{Name: "theirs", Enabled: true, ResellerId: 2})
	db.Create(&model.User{Name: "direct", Enabled: true})

	req := func(path string, rid uint) bool {
		r := httptest.NewRequest("GET", "http://x/app/api"+path, nil)
		r = r.WithContext(withScope(r, rid))
		return s.guardScope(httptest.NewRecorder(), r)
	}
	if !req("/users/1", 1) || !req("/users/1/sub", 1) || !req("/users/1/qr", 1) {
		t.Fatal("自己的用户(含子路由)应放行")
	}
	if req("/users/2/sub", 1) {
		t.Fatal("别家用户的子路由也要拒绝")
	}
	if req("/users/2", 1) {
		t.Fatal("别的代理的用户应拒绝")
	}
	if req("/users/3", 1) {
		t.Fatal("主面板直属用户应拒绝")
	}
	if req("/users/bulk", 1) || req("/users/export", 1) {
		t.Fatal("批量接口不应开放给代理")
	}
	if !req("/users/2", 0) {
		t.Fatal("主面板不受限")
	}

	// 列表过滤
	var mine []model.User
	r := httptest.NewRequest("GET", "http://x/app/api/users", nil)
	r = r.WithContext(withScope(r, 2))
	s.scoped(r, db.Order("id asc")).Find(&mine)
	if len(mine) != 1 || mine[0].Name != "theirs" {
		t.Fatalf("列表应只剩自己的用户: %+v", mine)
	}
}
