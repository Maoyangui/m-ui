//go:build !race

// 和 agent_apply_e2e_test.go 同一组(用到那边的 agentReport),竞争检测下同样跳过。

package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/runner"
)

// 副机报告里的账本连同纪元一起读:原主机变为副机时清空重开的那本账(LedgerFreshKey 等于当前纪元)报 ledgerFresh,
// 新主机据此从 0 计;纪元换过之后(比如从备份还原)不再声明。
func TestAgentReportLedgerFresh(t *testing.T) {
	const token = "e2e-agent-token-fresh-0123456789"
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
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rep := agentReport(t, srv.URL, token)
	if rep.LedgerEpoch == "" || rep.LedgerFresh {
		t.Fatalf("启动时生成的纪元不是清空重开的: %+v", rep)
	}
	if err := run.SetSetting(hub.LedgerFreshKey, rep.LedgerEpoch); err != nil {
		t.Fatal(err)
	}
	if err := model.AddAgentCounters(run.DB(), []model.AgentCounter{{UserName: "alice", Up: 7, Down: 7}}, 100); err != nil {
		t.Fatal(err)
	}
	rep = agentReport(t, srv.URL, token)
	if !rep.LedgerFresh || len(rep.Counters) != 1 || rep.Counters[0].Down != 7 {
		t.Fatalf("声明了清空重开的那本账要报 ledgerFresh,连同账本: %+v", rep)
	}
	if err := run.SetSetting(hub.LedgerEpochKey, hub.NewLedgerEpoch()); err != nil { // 纪元换了(还原备份)
		t.Fatal(err)
	}
	if rep = agentReport(t, srv.URL, token); rep.LedgerFresh {
		t.Fatalf("纪元换过之后不再声明清空重开: %+v", rep)
	}
}
