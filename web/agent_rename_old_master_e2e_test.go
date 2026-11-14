//go:build !race

// 起真实的内嵌数据面,竞争检测下跳过的原因同 agent_apply_e2e_test.go。

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/runner"
)

// 改名时副机上原名那本(还没报完的尾巴)按主人留着、照常上报,要主机按主人计到改名后的用户头上 —— 只有 0.7.17 起的主机这样做。
// 主机还是 0.7.16(先升副机的过渡期)时它只认名字:原名之后给了新用户、推送又没送到(拉报告照常成功),老主机会把那段
// 算到新的同名用户头上。所以主机低于 0.7.17 时副机照旧按名字清;0.7.17 起的主机照旧留着。真实的 hub 推送打到真实的
// handleAgentApply,快照里的主机版本就是 Deps.Version。
func TestAgentRenameLedgerFollowsMasterVersion(t *testing.T) {
	masterDB := openAgentTestDB(t, "master.db")
	masterDB.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	line := model.Line{Name: "hot", Protocol: "shadowsocks", Port: freeTCPPort(t), Enabled: true,
		Options: json.RawMessage(`{"method":"aes-128-gcm","password":"e2e-server-key"}`)}
	masterDB.Create(&line)
	alice := model.User{Name: "alice", Enabled: true, Credentials: generateCredentials("alice")}
	masterDB.Create(&alice)
	masterDB.Create(&model.UserLine{UserId: alice.Id, LineId: line.Id})
	masterSetting := func(k string) string {
		var v string
		masterDB.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
		return v
	}

	const token = "e2e-agent-token-rename-0123456789"
	nodePath := filepath.Join(t.TempDir(), "node.db")
	if err := runner.SetSettings(nodePath, map[string]string{"nodeMode": "true", "nodeToken": token}); err != nil {
		t.Fatal(err)
	}
	run, err := runner.New(nodePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		run.Stop()
		_ = database.Close(run.DB())
	})
	s := NewServer(run)
	mux := http.NewServeMux()
	mux.HandleFunc(innerBase+"api/agent/", s.handleAgent)
	agentSrv := httptest.NewServer(mux)
	defer agentSrv.Close()
	nodeRow := model.Node{Name: "副机", ApiUrl: agentSrv.URL + "/app/", Token: token, Enabled: true, Sort: 2}
	masterDB.Create(&nodeRow)
	ledgers := func(name string) int64 {
		var n int64
		run.DB().Model(&model.AgentCounter{}).Where("user_name = ?", name).Count(&n)
		return n
	}
	rename := func(to string) {
		t.Helper()
		if err := masterDB.Model(&model.User{}).Where("id = ?", alice.Id).Update("name", to).Error; err != nil {
			t.Fatal(err)
		}
	}
	unreported := func(name string) {
		t.Helper()
		if err := model.AddAgentCounters(run.DB(), []model.AgentCounter{{UserName: name, Up: 30, Down: 30}}, 100); err != nil {
			t.Fatal(err)
		}
	}

	// 0.7.17 起的主机:原名那本按主人留着,照常上报
	h := hub.New(hub.Deps{DB: masterDB, Setting: masterSetting, IsNode: func() bool { return false }, Version: "0.7.17"})
	defer h.Stop()
	if err := h.PushNow(nodeRow); err != nil {
		t.Fatalf("首次推送应成功: %v", err)
	}
	unreported("alice")
	rename("bob")
	if err := h.PushNow(nodeRow); err != nil {
		t.Fatal(err)
	}
	if ledgers("alice") != 1 {
		t.Fatal("主机 0.7.17 起按主人计费:改名时原名那本(没报完的尾巴)应留着照常上报")
	}

	// 主机回滚到 0.7.16(或者还没升上来):它只认名字,原名那本按名字清
	old := hub.New(hub.Deps{DB: masterDB, Setting: masterSetting, IsNode: func() bool { return false }, Version: "0.7.16"})
	defer old.Stop()
	unreported("bob")
	rename("carol")
	if err := old.PushNow(nodeRow); err != nil {
		t.Fatal(err)
	}
	if n := ledgers("bob"); n != 0 {
		t.Fatalf("主机是 0.7.16、只认名字:改名后原名那本应按名字清掉,还剩 %d 本", n)
	}
	if n := ledgers("alice"); n != 0 {
		t.Fatalf("更早那个原名的账本同样按名字清掉,还剩 %d 本", n)
	}
}
