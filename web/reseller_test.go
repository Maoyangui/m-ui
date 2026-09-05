package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
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
	// 请求体里塞的计量与共享令牌必须被清掉(负数用量能把额度刷回来)
	dirty := model.User{Name: "dirty", Enabled: true, DeviceLimit: 1,
		Up: -1 << 40, TotalDown: -1 << 40, ShareToken: "chosen", ShareCreds: []byte("{}")}
	if err := s.prepareResellerUser(a.Id, &dirty, []uint{1}); err != nil {
		t.Fatal(err)
	}
	if dirty.Up != 0 || dirty.TotalDown != 0 || dirty.ShareToken != "" || dirty.ShareCreds != nil {
		t.Fatalf("代理提交的计量/共享字段应被清掉: %+v", dirty)
	}
	db.Create(&u)

	// 设备池 5:分配时不再限制"之和",代理给用户填多少都行,0 也行(运行时由数据面按池限制)
	u2 := model.User{Name: "u2", Enabled: true, DeviceLimit: 3}
	if err := s.checkResellerUser(a.Id, 0, &u2, []uint{1}); err != nil {
		t.Fatalf("分配超过池上限也应通过(运行时按池限): %v", err)
	}
	u2.DeviceLimit = 99
	if err := s.checkResellerUser(a.Id, 0, &u2, []uint{1}); err != nil {
		t.Fatalf("单个用户的上限可以比池大: %v", err)
	}
	u2.DeviceLimit = 0
	if err := s.checkResellerUser(a.Id, 0, &u2, []uint{1}); err != nil {
		t.Fatalf("用户可以不限设备,只受池限: %v", err)
	}
	u2.DeviceLimit = -1
	if err := s.checkResellerUser(a.Id, 0, &u2, []uint{1}); err == nil {
		t.Fatal("负数仍应被拒")
	}
	u2.DeviceLimit = 0
	// 用户数上限:a 已有 1 个用户,上限 1 → 新建被拒,编辑已有的不受影响;改成 2 就能建
	db.Model(&model.Reseller{}).Where("id = ?", a.Id).Update("user_limit", 1)
	if err := s.checkResellerUser(a.Id, 0, &u2, []uint{1}); err == nil || !strings.Contains(err.Error(), "用户数上限") {
		t.Fatalf("达到用户数上限应拒绝新建: %v", err)
	}
	if err := s.checkResellerUser(a.Id, u.Id, &u, []uint{1}); err != nil {
		t.Fatalf("上限只管新建,编辑已有用户应通过: %v", err)
	}
	db.Model(&model.Reseller{}).Where("id = ?", a.Id).Update("user_limit", 2)
	if err := s.checkResellerUser(a.Id, 0, &u2, []uint{1}); err != nil {
		t.Fatalf("上限放宽后应能新建: %v", err)
	}
	db.Model(&model.Reseller{}).Where("id = ?", a.Id).Update("user_limit", 0)
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
		t.Fatal("批量生成/导出不应开放给代理")
	}
	if !req("/users/batch", 1) {
		t.Fatal("批量操作要放行(由处理函数按代理过滤 id)")
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

// 额度不能被代理自己洗掉:重置/续费只是把 up/down 挪进 total_*,删号会结转,
// 只有主面板的"重置流量"抬基线才让额度回血。
func TestResellerQuotaCannotBeReset(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	rs := model.Reseller{Name: "a", Enabled: true, Volume: 10 << 30}
	db.Create(&rs)
	u := model.User{Name: "u1", Enabled: true, ResellerId: rs.Id, Up: 8 << 30}
	db.Create(&u)

	get := func() int64 {
		var cur model.Reseller
		db.First(&cur, rs.Id)
		return resellerUsed(db, cur)
	}
	if got := get(); got != 8<<30 {
		t.Fatalf("已用应为 8G: %d", got)
	}
	// 代理重置用户流量(up→total_up):额度照算
	db.Model(&model.User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{
		"total_up": gorm.Expr("total_up + up"), "up": 0})
	if got := get(); got != 8<<30 {
		t.Fatalf("重置后额度不该回血: %d", got)
	}
	// 删号:用量结转到代理
	var cur model.User
	db.First(&cur, u.Id)
	carryUsage(db, cur)
	db.Delete(&model.User{}, u.Id)
	if got := get(); got != 8<<30 {
		t.Fatalf("删号后额度不该回血: %d", got)
	}
	// 主面板重置:抬基线 → 归零
	var r2 model.Reseller
	db.First(&r2, rs.Id)
	db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("used_base", r2.UsedCarried)
	if got := get(); got != 0 {
		t.Fatalf("主面板重置后应归零: %d", got)
	}
}

// 套餐不能越权:代理套一个带未授权线路的套餐要被拒。
func TestResellerPlanRespectsLines(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Line{Name: "l1", Protocol: "hysteria2", Port: 1, Enabled: true})
	db.Create(&model.Line{Name: "l2", Protocol: "anytls", Port: 2, Enabled: true})
	rs := model.Reseller{Name: "a", Enabled: true, DeviceLimit: 4}
	db.Create(&rs)
	s.setResellerLines(rs.Id, []uint{1})
	u := model.User{Name: "u1", Enabled: true, ResellerId: rs.Id, DeviceLimit: 2}
	db.Create(&u)
	db.Create(&model.UserLine{UserId: u.Id, LineId: 1})

	bad := model.Plan{Name: "bad", ResellerId: rs.Id, DeviceLimit: 2, LineIds: []byte(`[1,2]`)}
	if err := s.checkResellerPlan(rs.Id, u, bad); err == nil {
		t.Fatal("套餐里的未授权线路应被拒")
	}
	over := model.Plan{Name: "over", ResellerId: rs.Id, DeviceLimit: 9, LineIds: []byte(`[1]`)}
	if err := s.checkResellerPlan(rs.Id, u, over); err != nil {
		t.Fatalf("套餐设备数超过池上限也应通过(运行时按池限): %v", err)
	}
	ok := model.Plan{Name: "ok", ResellerId: rs.Id, DeviceLimit: 3, LineIds: []byte(`[1]`)}
	if err := s.checkResellerPlan(rs.Id, u, model.Plan{Name: "master", LineIds: []byte(`[1]`)}); err == nil {
		t.Fatal("主面板的套餐代理不该能用")
	}
	if err := s.checkResellerPlan(rs.Id, u, ok); err != nil {
		t.Fatalf("合规套餐应通过: %v", err)
	}
}

// 代理会话不能当管理员会话用:两者存在同一张表里,而 Cookie 不区分端口,
// 代理只要把自己的令牌换个 Cookie 名塞给主面板就会被当成管理员。
func TestResellerSessionIsNotAdminSession(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db, sessions: map[string]session{}}

	rs := model.Reseller{Name: "dl", Enabled: true}
	db.Create(&rs)
	tok := s.newResellerSession(rs, false)
	if s.validSession(tok) {
		t.Fatal("代理会话不得通过主面板鉴权")
	}
	admin := s.newSession("admin")
	if !s.validSession(admin) {
		t.Fatal("管理员会话应通过")
	}
}

// 空密码首登只在认领窗口内有效,过期后必须由主面板重置。
func TestResellerClaimWindow(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db, sessions: map[string]session{}, loginFails: map[string][]int64{}}

	db.Create(&model.Reseller{Name: "dl", Enabled: true, ClaimBefore: time.Now().Unix() + 3600})
	login := func() int {
		body := strings.NewReader(`{"username":"dl","password":""}`)
		r := httptest.NewRequest("POST", "http://x/app/api/login", body)
		w := httptest.NewRecorder()
		s.handleResellerLogin(w, r)
		return w.Code
	}
	if code := login(); code != 200 {
		t.Fatalf("窗口内空密码应能首登,得 %d", code)
	}
	db.Model(&model.Reseller{}).Where("name = ?", "dl").Update("claim_before", time.Now().Unix()-1)
	if code := login(); code != 401 {
		t.Fatalf("窗口过期后应拒绝,得 %d", code)
	}
}
