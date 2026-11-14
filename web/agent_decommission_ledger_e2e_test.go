//go:build !race

// 起真实的内嵌数据面,竞争检测下跳过的原因同 agent_apply_e2e_test.go。

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/runner"
)

// 停用副机时本版副机不清账本(PruneAgentCounters 遇到空用户表不动),确认下线通知时回 ledgerKept 告诉主机;
// 主机据此不给它的游标打"账本已清空"标记(只有老副机才会把账本整本清掉)。真实的 hub.Decommission 打到真实的
// handleAgentApply:重新启用后同一本账真的回退(断电丢了最后几笔),仍按账本回退对齐不计 ——
// 标记错打上的话,会当成从 0 重开的一本把整本再计一遍。
func TestAgentDecommissionKeepsLedgerEndToEnd(t *testing.T) {
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

	const token = "e2e-agent-token-ledger-0123456789"
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

	h := hub.New(hub.Deps{DB: masterDB, Setting: masterSetting, IsNode: func() bool { return false }, Version: Version})
	nodeRow := model.Node{Name: "副机", ApiUrl: agentSrv.URL + "/app/", Token: token, Enabled: true, Sort: 2}
	masterDB.Create(&nodeRow)
	if err := h.PushNow(nodeRow); err != nil {
		t.Fatalf("首次推送应成功: %v", err)
	}
	usage := func() int64 {
		t.Helper()
		var u model.User
		masterDB.First(&u, alice.Id)
		return u.Down
	}
	merge := func() hub.MergeResult {
		t.Helper()
		rep := agentReport(t, agentSrv.URL, token)
		res, err := hub.MergeCounters(masterDB, nodeRow.Id, nodeRow.Name, rep.LedgerEpoch, rep.Counters, time.Now().Unix(), 60, 1)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// 副机上一本升级前的老账本(不记主人):主机只能按计数猜,标记打错了才会有影响
	if err := run.DB().Create(&model.AgentCounter{UserName: "alice", Up: 1000, Down: 1000}).Error; err != nil {
		t.Fatal(err)
	}
	merge() // 这台第一次报纪元、还没有游标:只建基线
	if err := model.AddAgentCounters(run.DB(), []model.AgentCounter{{UserName: "alice", Up: 50, Down: 50}}, 100); err != nil {
		t.Fatal(err)
	}
	merge()
	if got := usage(); got != 50 {
		t.Fatalf("建基线后按增量,应是 50,实际 %d", got)
	}

	// 停用:推空用户表,副机确认
	before, err := hub.BuildSnapshot(masterDB, masterSetting)
	if err != nil {
		t.Fatal(err)
	}
	masterDB.Model(&model.Node{}).Where("id = ?", nodeRow.Id).Update("enabled", false)
	if err := h.Decommission(nodeRow, before); err != nil {
		t.Fatalf("下线通知应送到: %v", err)
	}
	var users, ledgers int64
	run.DB().Model(&model.User{}).Count(&users)
	run.DB().Model(&model.AgentCounter{}).Count(&ledgers)
	if users != 0 || ledgers != 1 {
		t.Fatalf("副机应用了空用户表(剩 %d 个用户),账本应留着(剩 %d 本)", users, ledgers)
	}
	var cur model.TrafficCursor
	masterDB.Where("node_id = ? AND user_name = ?", nodeRow.Id, "alice").First(&cur)
	if cur.Cleared {
		t.Fatalf("本版副机停用时不清账本,主机不该标记账本已清空: %+v", cur)
	}

	// 重新启用:推回完整快照;副机断电丢了最后几笔,同一本账计数变小
	masterDB.Model(&model.Node{}).Where("id = ?", nodeRow.Id).Update("enabled", true)
	nodeRow.Enabled = true
	if err := h.PushNow(nodeRow); err != nil {
		t.Fatalf("重新启用后推送应成功: %v", err)
	}
	if err := run.DB().Model(&model.AgentCounter{}).Where("user_name = ?", "alice").
		Updates(map[string]interface{}{"up": 1020, "down": 1020}).Error; err != nil {
		t.Fatal(err)
	}
	res := merge()
	if res.Regressed != 1 || res.Reopened != 0 || usage() != 50 {
		t.Fatalf("同一本账回退应对齐不计、告警,应仍是 50,实际 %d(%+v)", usage(), res)
	}
}
