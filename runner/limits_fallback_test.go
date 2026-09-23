package runner

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// E:读库失败时沿用上一份策略、数据面不停、后台重试。
// 0.6.9 读库失败只 return —— 重启后的新 Box 上 Limiter 是空表,所有限额放开;0.6.10 直接 Stop 整机。
// 这条钉住中间那个点:重载后的新 Box 拿到的是上一份策略而不是空表,limitsPending 被打上,数据面在跑。
func TestApplyLimitsKeepsLastSpecsWhenDBFails(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.User{Name: "u", Enabled: true, DeviceLimit: 1, Credentials: []byte(`{}`)})
	r := &Runner{db: db, core: core.NewCore(), dbPath: filepath.Join(dir, "x.db")}
	defer r.core.Stop()

	good := []byte(`{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"direct"}]}`)
	if err := r.reloadAllLocked(good); err != nil {
		t.Fatalf("初始配置应能启动: %v", err)
	}
	lim := r.core.GetInstance().Limiter()
	if !lim.AllowConn("u", "1.1.1.1") || lim.AllowConn("u", "2.2.2.2") {
		t.Fatal("前提:上限 1 的策略已经生效")
	}
	if r.lastSpecs == nil {
		t.Fatal("成功读到的策略应被记住")
	}

	// 库坏了:users 表没了
	if err := db.Exec("DROP TABLE users").Error; err != nil {
		t.Fatal(err)
	}
	if err := r.reloadAllLockedForce(good); err != nil {
		t.Fatalf("策略读不到不该让重载失败,更不该停机: %v", err)
	}
	if !r.core.IsRunning() {
		t.Fatal("读库失败时数据面必须继续跑")
	}
	if !r.limitsPending.Load() {
		t.Fatal("读库失败要记下待重试")
	}
	lim = r.core.GetInstance().Limiter()
	if !lim.AllowConn("u", "1.1.1.1") || lim.AllowConn("u", "2.2.2.2") {
		t.Fatal("新 Box 上应装回上一份策略(上限 1),而不是空表放开一切")
	}

	// 库好了:重试成功
	if err := db.AutoMigrate(&model.User{}); err != nil {
		t.Fatal(err)
	}
	db.Create(&model.User{Name: "u", Enabled: true, DeviceLimit: 3, Credentials: []byte(`{}`)})
	if err := r.applyLimits(); err != nil {
		t.Fatalf("库恢复后应能读到: %v", err)
	}
	if r.lastSpecs["u"].DeviceLimit != 3 {
		t.Fatalf("重试成功后应刷新到新策略,实际 %+v", r.lastSpecs["u"])
	}
}

// B:撤销凭据的热更新有一处入站失败,不再停掉整台机器;不在运行配置里的入站补建失败只告警。
// 0.6.10 在这里 core.Stop(),之后每 5 秒"起旧配置 → 判失败 → 再停",主机在管理员修好前一直不可用。
func TestReloadUsersSecureKeepsRunningOnPartialFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	r := &Runner{db: db, core: core.NewCore(), dbPath: dbPath}
	defer r.core.Stop()

	// 一条能起来的线路(单用户 ss,密码在线路选项里)
	okPort := freePort(t)
	db.Create(&model.Line{Name: "ok", Protocol: "shadowsocks", Port: okPort, Enabled: true,
		Options: []byte(`{"method":"aes-256-gcm","password":"test-password"}`)})
	if err := r.ReloadAll(); err != nil {
		t.Fatalf("初始配置应能启动: %v", err)
	}
	box0 := r.core.GetInstance()

	// 再加一条端口被占、起不来的线路 —— 它本来就没在服务
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	db.Create(&model.Line{Name: "busy", Protocol: "shadowsocks", Port: busy, Enabled: true,
		Options: []byte(`{"method":"aes-256-gcm","password":"test-password"}`)})

	err = r.ReloadUsersSecure()
	if !r.core.IsRunning() {
		t.Fatalf("撤销凭据的热更新有一处失败,也不能停掉整台机器(err=%v)", err)
	}
	if r.core.GetInstance() != box0 {
		t.Fatal("热更新不该换 Box")
	}
	if err != nil && strings.Contains(err.Error(), "已停止本机数据面") {
		t.Fatalf("不该再有停机的字样: %v", err)
	}
	// 起不来的那条本来就不在运行配置里,它的失败不该把整次热更新判成失败
	if err != nil {
		t.Fatalf("不在运行配置里的入站补建失败只该告警,实际返回错误: %v", err)
	}
}
