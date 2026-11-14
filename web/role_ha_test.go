package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 面板上切主副角色和主机容灾(审计中危):开过容灾的集群(纪元 ≥ 1)里把副机直接改成主机会让别的副机一律拒收 —— 拒绝并指路;
// 主机改成副机要先清掉本机的容灾状态(和菜单一样);没开过容灾的照旧能切。
func TestRoleToggleRespectsHA(t *testing.T) {
	post := func(s *Server, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleSettings(rec, httptest.NewRequest(http.MethodPost, "/app/api/settings", strings.NewReader(body)))
		return rec
	}
	node := newLDRunner(t, "node.db", map[string]string{"nodeMode": "true", "nodeToken": "tok-0123456789", "haEpoch": "3"})
	if rec := post(NewServer(node), `{"nodeMode":"false"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "设为主机") || !strings.Contains(rec.Body.String(), "m-ui ha reset") {
		t.Fatalf("开过容灾的副机不能直接改成主机: %d %s", rec.Code, rec.Body.String())
	}
	if !node.IsNode() || node.HA().Epoch() != 3 {
		t.Fatal("拒绝时什么都不改")
	}
	master := newLDRunner(t, "master.db", map[string]string{"haEpoch": "2"})
	if rec := post(NewServer(master), `{"nodeMode":"true"}`); rec.Code != http.StatusOK {
		t.Fatalf("主机改成副机照旧可以: %d %s", rec.Code, rec.Body.String())
	}
	if !master.IsNode() || master.HA().Epoch() != 0 {
		t.Fatalf("主机改成副机要清掉本机的容灾状态(纪元归零),实际纪元 %d", master.HA().Epoch())
	}
	plain := newLDRunner(t, "plain.db", map[string]string{"nodeMode": "true", "nodeToken": "tok-0123456789"})
	if rec := post(NewServer(plain), `{"nodeMode":"false"}`); rec.Code != http.StatusOK || plain.IsNode() {
		t.Fatalf("没开过容灾的副机照旧能改成主机: %d %s", rec.Code, rec.Body.String())
	}
}
