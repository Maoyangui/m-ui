//go:build !race

// 起的是真实的内嵌数据面,-race 下跳过的原因见 agent_apply_e2e_test.go。

package web

import (
	"bytes"
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

// 主副机升级顺序不同时落地页 logo 也要到副机(真实 runner + 真实 agent 接口 + 主机的 PushNow):
//  1. 主机先升级、传了 logo,旧版副机收快照时丢了 logo、修订号序号照记;副机升级后主机重推同一份(10 分钟定期重推、
//     「推送」按钮、主机看到副机换了版本的那次重推),副机要整份应用,logo 落库;之后同一份再来才直接确认。
//  2. 降级期间旧版收了换过 logo 的快照(logo 表、本版的标记都没动),升回来后主机重推同一份,同样整份应用。
func TestAgentApplyAfterUpgradeLandsLogos(t *testing.T) {
	masterDB := openAgentTestDB(t, "master.db")
	masterDB.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	line := model.Line{Name: "probe", Protocol: "mixed", Port: freeTCPPort(t), Enabled: true, Options: json.RawMessage(`{}`)}
	masterDB.Create(&line)
	alice := model.User{Name: "alice", Enabled: true, Credentials: generateCredentials("alice")}
	masterDB.Create(&alice)
	masterDB.Create(&model.UserLine{UserId: alice.Id, LineId: line.Id})
	imgA, imgB := logoPNG(t, 120, 40, 10), logoPNG(t, 120, 40, 200)
	masterDB.Create(&model.PageLogo{ResellerId: 0, Mime: "image/png", Data: imgA, Sum: logoSum(imgA), Width: 120, Height: 40, UpdatedAt: 1})
	masterSetting := func(k string) string {
		var v string
		masterDB.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
		return v
	}

	const token = "logo-agent-token-0123456789"
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

	nodeLogo := func() []byte {
		t.Helper()
		var rows []model.PageLogo
		if err := s.db.Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ResellerId != 0 {
			return nil
		}
		return rows[0].Data
	}
	stamp := func() string {
		return Version + "#" + s.setting("hubRevision") + "#" + s.setting("hubSnapshotSequence")
	}
	push := func(what string) {
		t.Helper()
		if err := h.PushNow(nodeRow); err != nil {
			t.Fatalf("%s:推送应成功: %v", what, err)
		}
	}

	// ---- 0. 正常推一次:logo 落库,记下本版程序应用了这一份 ----
	push("首次推送")
	if !bytes.Equal(nodeLogo(), imgA) {
		t.Fatal("前提:副机应拿到主机的 logo")
	}
	if got := s.setting(agentAppliedKey); got != stamp() {
		t.Fatalf("整份应用后应记下本版程序应用了这一份: %q,应为 %q", got, stamp())
	}
	rev1 := s.setting("hubRevision")

	// ---- 1. 模拟旧版副机应用了这一份、然后升级:logo 表是空的(升级时新建),没有本版的标记,修订号序号照旧 ----
	s.db.Exec("DELETE FROM page_logos")
	s.db.Exec("DELETE FROM settings WHERE key = ?", agentAppliedKey)
	appliedAt := s.setting("hubAppliedAt")
	time.Sleep(1100 * time.Millisecond) // hubAppliedAt 精确到秒
	push("升级后主机重推同一份")
	if got := s.setting("hubRevision"); got != rev1 {
		t.Fatalf("前提:重推的是同一修订 %s,实际 %s", rev1, got)
	}
	if !bytes.Equal(nodeLogo(), imgA) {
		t.Fatal("副机升级后,主机重推的同一份快照应整份应用、logo 落库")
	}
	if s.setting("hubAppliedAt") == appliedAt || s.setting(agentAppliedKey) != stamp() {
		t.Fatal("应整份重新应用并记下本版的标记")
	}
	// 之后同一份再来:直接确认,不再整表重写
	appliedAt = s.setting("hubAppliedAt")
	time.Sleep(1100 * time.Millisecond)
	push("同一份再推")
	if got := s.setting("hubAppliedAt"); got != appliedAt {
		t.Fatalf("本版程序已完整应用过的同一份应直接确认: hubAppliedAt %s → %s", appliedAt, got)
	}

	// ---- 2. 降级期间旧版收了换过 logo 的快照 ----
	stamp1 := s.setting(agentAppliedKey)
	masterDB.Model(&model.PageLogo{}).Where("reseller_id = ?", 0).Updates(map[string]interface{}{"data": imgB, "sum": logoSum(imgB)})
	push("换 logo 后推送")
	if !bytes.Equal(nodeLogo(), imgB) {
		t.Fatal("前提:换图后副机应拿到新图")
	}
	// 改回"旧版应用了这份新快照"的样子:修订号序号是新的,logo 表还是旧图、标记还是上一份的
	s.db.Exec("DELETE FROM page_logos")
	s.db.Create(&model.PageLogo{ResellerId: 0, Mime: "image/png", Data: imgA, Sum: logoSum(imgA), Width: 120, Height: 40, UpdatedAt: 1})
	if err := run.SetSetting(agentAppliedKey, stamp1); err != nil {
		t.Fatal(err)
	}
	push("升回来后主机重推同一份")
	if !bytes.Equal(nodeLogo(), imgB) {
		t.Fatal("降级期间旧版收过的快照,升回来后应整份应用、换上新图")
	}
}
