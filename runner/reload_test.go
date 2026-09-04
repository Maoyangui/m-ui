package runner

import (
	"fmt"
	"github.com/Maoyangui/m-ui/core"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 上游热更新:增删上游只增删出站,数据面实例不重启(Box 指针不变)。
func TestReloadUpstreamsKeepsBoxRunning(t *testing.T) {
	dir := t.TempDir()
	r, err := New(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.db)
	if err := r.Start(); err != nil {
		t.Fatalf("空配置启动失败: %v", err)
	}
	defer r.Stop()
	box0 := r.core.GetInstance()
	if box0 == nil {
		t.Fatal("数据面未运行")
	}
	if _, ok := r.applied["direct"]; !ok {
		t.Fatalf("启动后应记录内置出站: %v", r.applied)
	}

	// 新增上游 → 热添加
	up := model.Upstream{Name: "hot", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":1}`)}
	r.db.Create(&up)
	if err := r.ReloadUpstreams(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.applied["hot"]; !ok {
		t.Fatal("热添加后应记录出站 hot")
	}
	if _, ok := box0.Outbound().Outbound("hot"); !ok {
		t.Fatal("运行中的数据面应有出站 hot")
	}
	if r.core.GetInstance() != box0 {
		t.Fatal("热更新不应重启数据面")
	}

	// 修改上游 → 移除再添加
	r.db.Model(&model.Upstream{}).Where("id = ?", up.Id).Update("options", []byte(`{"server":"127.0.0.1","server_port":2}`))
	if err := r.ReloadUpstreams(); err != nil {
		t.Fatal(err)
	}
	if r.applied["hot"] == "" || r.core.GetInstance() != box0 {
		t.Fatal("修改后应仍在同一数据面")
	}
	// 未变化 → 无操作,仍成功
	if err := r.ReloadUpstreams(); err != nil {
		t.Fatal(err)
	}

	// 删除上游 → 热移除
	r.db.Delete(&model.Upstream{}, up.Id)
	if err := r.ReloadUpstreams(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.applied["hot"]; ok {
		t.Fatal("删除后不应再有出站 hot")
	}
	if _, ok := box0.Outbound().Outbound("hot"); ok {
		t.Fatal("运行中的数据面不应再有出站 hot")
	}
	if r.core.GetInstance() != box0 {
		t.Fatal("全程不应重启数据面")
	}

	// 配置未变化的全量重载不应重启;强制重载才重启
	if err := r.ReloadAll(); err != nil {
		t.Fatal(err)
	}
	if r.core.GetInstance() != box0 {
		t.Fatal("配置无变化时 ReloadAll 不应重启")
	}
	if err := r.ReloadAllForce(); err != nil {
		t.Fatal(err)
	}
	if r.core.GetInstance() == box0 {
		t.Fatal("ReloadAllForce 应重启数据面")
	}
}

// 新配置起不来(端口被别的进程抢走之类)时,要用上一份可用配置把数据面拉回来,
// 而不是把所有用户晾在那儿。
func TestReloadRollsBackWhenStartFails(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	r := &Runner{db: db, core: core.NewCore(), dbPath: filepath.Join(dir, "x.db")}

	// 先起一份能用的配置(只有出站,不占端口)
	good := []byte(`{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"direct"}]}`)
	if err := r.reloadAllLocked(good); err != nil {
		t.Fatalf("初始配置应能启动: %v", err)
	}
	if !r.core.IsRunning() {
		t.Fatal("数据面应在运行")
	}

	// 再喂一份能过校验、但启动时必然失败的配置(监听一个已被本测试占住的端口)
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	bad := []byte(fmt.Sprintf(`{"log":{"disabled":true},"inbounds":[{"type":"mixed","tag":"in","listen":"::","listen_port":%d}],"outbounds":[{"type":"direct","tag":"direct"}]}`, busy))
	err = r.reloadAllLocked(bad)
	if err == nil {
		t.Fatal("坏配置应当报错")
	}
	if !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("错误里应说明已回滚: %v", err)
	}
	if !r.core.IsRunning() {
		t.Fatal("回滚后数据面应仍在运行")
	}
	r.core.Stop()
}
