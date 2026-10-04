package runner

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 整份配置没换上去时(起不来回滚、冷却期),热更新用户不能把运行中配置里没有的线路补建出来:旧配置的路由里没有它,
// 它选的上游、分流规则(含拦截)都不生效,流量会落到默认的直连。以前端口空着就真开出来了。
func TestHotUpdateDoesNotAddInboundMissingFromRunningConfig(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	r := &Runner{db: db, core: core.NewCore(), dbPath: dbPath}
	defer r.core.Stop()

	db.Create(&model.Line{Name: "ok", Protocol: "shadowsocks", Port: freePort(t), Enabled: true,
		Options: []byte(`{"method":"aes-256-gcm","password":"test-password"}`)})
	if err := r.ReloadAll(); err != nil {
		t.Fatalf("初始配置应能启动: %v", err)
	}

	// 新线路:端口空着,带上游和一条拦截规则 —— 只有整份配置换上去才对
	db.Create(&model.Upstream{Name: "up", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`)})
	port := freePort(t)
	db.Create(&model.Line{Name: "new", Protocol: "shadowsocks", Port: port, Enabled: true, UpstreamId: 1,
		Options:    []byte(`{"method":"aes-256-gcm","password":"test-password"}`),
		RouteRules: json.RawMessage(`[{"type":"domain_keyword","values":["ads"],"to":-1}]`)})

	if err := r.ReloadUsersSecure(); err != nil {
		t.Fatalf("运行配置之外的线路跳过即可,不算失败: %v", err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Fatalf("新线路被补进了旧配置(端口 %d 被占):它的上游和分流规则都不会生效: %v", port, err)
	}
	ln.Close()
}
