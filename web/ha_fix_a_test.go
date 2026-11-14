package web

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/runner"
)

// 副机面板「重新生成令牌」= 重新配对:退出原来的容灾集群(纪元、认的主机、钉的公钥清零),新主机配上来时从头认。
// 主机删除副机时发的 agent/ha/leave 同样清零,只认当前主机发来的:请求头对得上还不够(只拿到副机令牌的人也能读出纪元与主机),
// 正文要当前主机用本纪元钉住的那把签名密钥签过名。
func TestRotateTokenResetsEpoch(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop()
	s := NewServer(run)
	key, err := ha.SignKey(ha.NewSignKey())
	if err != nil {
		t.Fatal(err)
	}
	seed := func() {
		t.Helper()
		if err := run.SetSettings(map[string]string{"nodeMode": "true", "nodeToken": "tok", ha.KeyEpoch: "2", ha.KeyMasterId: "4",
			ha.KeyMasterPub: ha.PubOf(key), ha.KeyGrant: `{"epoch":3}`, ha.KeyPeerKey: "k"}); err != nil {
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

	// ha/leave:只有当前主机(纪元、主机 id 对得上)发来、正文是它签的才清
	seed()
	sign := func(k ed25519.PrivateKey, action string, epoch uint64, master uint) string {
		body, _ := json.Marshal(map[string]interface{}{"action": action, "node": s.haSvc().SelfId(), "epoch": epoch, "master": master, "ts": time.Now().Unix()})
		sig := ed25519.Sign(k, append([]byte("m-ui/ha/ctl/v1\n"), body...))
		b, _ := json.Marshal(map[string]interface{}{"v": 1, "body": json.RawMessage(body), "sig": base64.StdEncoding.EncodeToString(sig)})
		return string(b)
	}
	call := func(path string, epoch uint64, master uint, body string) int {
		r := httptest.NewRequest(http.MethodPost, innerBase+"api/agent/"+path, strings.NewReader(body))
		r.RemoteAddr = "198.51.100.9:1234"
		r.Header.Set("X-Agent-Token", "tok")
		ha.SetHeader(r.Header, epoch, master)
		rec := httptest.NewRecorder()
		s.handleAgent(rec, r)
		return rec.Code
	}
	good := sign(key, "ha/leave", 2, 4)
	if code := call("ha/leave", 2, 9, good); code != http.StatusConflict || s.setting(ha.KeyEpoch) != "2" {
		t.Fatalf("不是当前主机发来的 leave 要拒: %d epoch=%s", code, s.setting(ha.KeyEpoch))
	}
	if code := call("ha/leave", 3, 4, good); code != http.StatusConflict || s.setting(ha.KeyEpoch) != "2" {
		t.Fatalf("更高纪元也不能拿 leave 来清: %d", code)
	}
	// 只拿到副机令牌:请求头对得上,正文没签名 / 自己签的 —— 不清
	evil, _ := ha.SignKey(ha.NewSignKey())
	for _, body := range []string{"", sign(evil, "ha/leave", 2, 4), sign(key, "ha/drop", 2, 4), sign(key, "ha/leave", 1, 4)} {
		if code := call("ha/leave", 2, 4, body); code != http.StatusBadRequest || s.setting(ha.KeyEpoch) != "2" || s.setting(ha.KeyMasterPub) == "" {
			t.Fatalf("没有当前主机的签名不能清纪元: %d epoch=%s", code, s.setting(ha.KeyEpoch))
		}
	}
	if code := call("ha/drop", 2, 4, ""); code != http.StatusBadRequest {
		t.Fatalf("删副本同样要主机签名: %d", code)
	}
	if code := call("ha/drop", 2, 4, sign(key, "ha/drop", 2, 4)); code != http.StatusOK {
		t.Fatalf("当前主机签的删副本: %d", code)
	}
	if code := call("ha/leave", 2, 4, good); code != http.StatusOK {
		t.Fatalf("当前主机签的 leave: %d", code)
	}
	cleared("ha/leave")
}
