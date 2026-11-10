//go:build !race

// 起的是真实的内嵌数据面,-race 下跳过的原因见 agent_apply_e2e_test.go。

package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/runner"
)

// 副机应用快照的几条新路径(审计 I4),真实 runner + 真实 agent 接口:
//  1. 同一修订、同一序号、没有待重载也没有待踢线:直接确认,不再整表删了重插、重载一遍(hubAppliedAt 和重载记录都不变);
//     报告里带上序号、待踢线与"收得了 gzip";
//  2. gzip 压缩的请求体照常应用;
//  3. 排队期间主机已经放弃的请求(连接断了)不再整表重写。
func TestAgentApplySameRevisionIsNoop(t *testing.T) {
	masterDB := openAgentTestDB(t, "master.db")
	masterDB.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	line := model.Line{Name: "probe", Protocol: "mixed", Port: freeTCPPort(t), Enabled: true, Options: json.RawMessage(`{}`)}
	masterDB.Create(&line)
	alice := model.User{Name: "alice", Enabled: true, Credentials: generateCredentials("alice")}
	masterDB.Create(&alice)
	masterDB.Create(&model.UserLine{UserId: alice.Id, LineId: line.Id})
	masterSetting := func(k string) string {
		var v string
		masterDB.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
		return v
	}

	const token = "scale-agent-token-0123456789"
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
	defer h.Stop()
	nodeRow := model.Node{Name: "副机", ApiUrl: agentSrv.URL + "/app/", Token: token, Enabled: true, Sort: 2}
	masterDB.Create(&nodeRow)

	// ---- 1. 同一份推两次:第二次直接确认 ----
	if err := h.PushNow(nodeRow); err != nil {
		t.Fatalf("首次推送应成功: %v", err)
	}
	rep := agentReport(t, agentSrv.URL, token)
	if rep.Sequence == 0 || !rep.ApplyGzip || rep.KickPending || rep.ReloadPending {
		t.Fatalf("报告应带序号、收得了 gzip、没有待踢线和待重载: %+v", rep)
	}
	appliedAt, reload := s.setting("hubAppliedAt"), run.ReloadStatus()
	time.Sleep(1100 * time.Millisecond) // hubAppliedAt 精确到秒
	if err := h.PushNow(nodeRow); err != nil {
		t.Fatalf("同一份再推应直接确认: %v", err)
	}
	if got := s.setting("hubAppliedAt"); got != appliedAt {
		t.Fatalf("同一修订同一序号不该再整表重写: hubAppliedAt %s → %s", appliedAt, got)
	}
	if got := run.ReloadStatus(); got.At != reload.At || got.Op != reload.Op {
		t.Fatalf("同一修订同一序号不该再重载数据面: %+v → %+v", reload, got)
	}

	// ---- 2. gzip 压缩的请求体 ----
	post := func(ctx context.Context, snap hub.Snapshot, gz bool) (int, string, error) {
		b, _ := json.Marshal(snap)
		if gz {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			zw.Write(b)
			zw.Close()
			b = buf.Bytes()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, agentSrv.URL+"/app/api/agent/apply", bytes.NewReader(b))
		req.Header.Set("X-Agent-Token", token)
		if gz {
			req.Header.Set("Content-Encoding", "gzip")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), nil
	}
	next := func(deviceLimit int) hub.Snapshot {
		t.Helper()
		masterDB.Model(&model.User{}).Where("id = ?", alice.Id).Update("device_limit", deviceLimit)
		snap, err := hub.BuildSnapshot(masterDB, masterSetting)
		if err != nil {
			t.Fatal(err)
		}
		snap.SelfNodeId, snap.Sequence = nodeRow.Id, uint64(time.Now().UnixNano())
		return snap
	}
	gzSnap := next(4)
	if code, body, err := post(context.Background(), gzSnap, true); err != nil || code != http.StatusOK || !strings.Contains(body, gzSnap.Revision) {
		t.Fatalf("gzip 请求体应照常应用: %d %s %v", code, body, err)
	}
	if got := s.setting("hubRevision"); got != gzSnap.Revision {
		t.Fatalf("gzip 推来的修订应已落库: %s", got)
	}

	// ---- 3. 排队期间主机放弃了:不再整表重写 ----
	stale := next(5)
	s.agentApplyMu.Lock() // 上一份还在应用
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := post(ctx, stale, false); err == nil {
		s.agentApplyMu.Unlock()
		t.Fatal("前提:应用锁被占着,主机这头应等到超时")
	}
	time.Sleep(200 * time.Millisecond) // 让副机察觉连接断了
	s.agentApplyMu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := s.setting("hubRevision"); got != gzSnap.Revision {
			t.Fatalf("主机已经放弃的请求不该再应用: hubRevision=%s", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
