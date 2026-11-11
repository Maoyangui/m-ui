package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/runner"
)

// 副机面板「重新生成令牌」= 重新配对:退出原来的容灾集群(纪元、认的主机、钉的公钥清零),新主机配上来时从头认。
// 主机删除副机时发的 agent/ha/leave 同样清零,只认当前主机发来的。
func TestRotateTokenResetsEpoch(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop()
	s := NewServer(run)
	seed := func() {
		t.Helper()
		if err := run.SetSettings(map[string]string{"nodeMode": "true", "nodeToken": "tok", ha.KeyEpoch: "2", ha.KeyMasterId: "4",
			ha.KeyMasterPub: "pub", ha.KeyGrant: `{"epoch":3}`, ha.KeyPeerKey: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	cleared := func(what string) {
		t.Helper()
		for _, k := range []string{ha.KeyEpoch, ha.KeyMasterId, ha.KeyMasterPub, ha.KeyGrant, ha.KeyPeerKey} {
			if v := s.setting(k); v != "" {
				t.Fatalf("%s:%s 要清掉,还是 %q", what, k, v)
			}
		}
	}
	seed()
	tok := s.newSession("admin")
	req := httptest.NewRequest(http.MethodPost, "/app/api/agent/rotate", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	w := httptest.NewRecorder()
	s.auth(s.handleAgentRotate)(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	cleared("重新生成令牌")

	// ha/leave:只有当前主机(纪元、主机 id 对得上)发来才清
	seed()
	call := func(epoch uint64, master uint) int {
		r := httptest.NewRequest(http.MethodPost, innerBase+"api/agent/ha/leave", nil)
		r.RemoteAddr = "198.51.100.9:1234"
		r.Header.Set("X-Agent-Token", "tok")
		ha.SetHeader(r.Header, epoch, master)
		rec := httptest.NewRecorder()
		s.handleAgent(rec, r)
		return rec.Code
	}
	if code := call(2, 9); code != http.StatusConflict || s.setting(ha.KeyEpoch) != "2" {
		t.Fatalf("不是当前主机发来的 leave 要拒: %d epoch=%s", code, s.setting(ha.KeyEpoch))
	}
	if code := call(3, 4); code != http.StatusConflict || s.setting(ha.KeyEpoch) != "2" {
		t.Fatalf("更高纪元也不能拿 leave 来清: %d", code)
	}
	if code := call(2, 4); code != http.StatusOK {
		t.Fatalf("当前主机发来的 leave: %d", code)
	}
	cleared("ha/leave")
}
