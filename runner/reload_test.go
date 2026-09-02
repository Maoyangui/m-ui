package runner

import (
	"path/filepath"
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
