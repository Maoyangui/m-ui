package ha

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"

	"gorm.io/gorm"
)

// miniAgent 只带容灾相关接口的假副机:按真实的纪元规则判定,状态与副本交给真实的 Service。
// 真实的 agent 接口(令牌 → 纪元 → 副机模式)在 web 包里另有端到端测试。
type miniAgent struct {
	t          *testing.T
	svc        *Service
	token      string
	srv        *httptest.Server
	mu         sync.Mutex
	calls      []string
	down       bool
	dropCommit bool
	revision   string
	// dieAfterHandover 收完整库数据后就失联(提交那一步没有回话)
	dieAfterHandover bool
}

func newMiniAgent(t *testing.T, svc *Service, token string) *miniAgent {
	t.Helper()
	a := &miniAgent{t: t, svc: svc, token: token}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *miniAgent) url() string { return a.srv.URL + "/app/" }

func (a *miniAgent) setDown(v bool) {
	a.mu.Lock()
	a.down = v
	a.mu.Unlock()
}

func (a *miniAgent) serve(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Path[strings.Index(r.URL.Path, "/agent/")+len("/agent/"):]
	a.mu.Lock()
	a.calls = append(a.calls, action)
	down := a.down
	a.mu.Unlock()
	if down {
		hj, ok := w.(http.Hijacker)
		if ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		http.Error(w, "down", 502)
		return
	}
	if r.Header.Get("X-Agent-Token") != a.token {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "令牌错误"})
		return
	}
	if rej := a.svc.CheckAgent(r.Header, action); rej != nil {
		w.Header().Set(HeaderNode, strconv.FormatUint(a.svc.Epoch(), 10))
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(rej)
		return
	}
	w.Header().Set(HeaderNode, strconv.FormatUint(a.svc.Epoch(), 10))
	reply := func(v interface{}, err error) {
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(v)
	}
	switch action {
	case "ha/state":
		var st State
		if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
			reply(nil, err)
			return
		}
		reply(map[string]string{"ok": "1"}, a.svc.ApplyState(st))
	case "ha/replica":
		seq, err := a.svc.ReceiveReplica(r.Body)
		reply(map[string]interface{}{"ok": "1", "seq": seq}, err)
	case "ha/drop":
		reply(map[string]string{"ok": "1"}, a.svc.DropReplica())
	case "ha/prepare":
		var req PrepareRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		rep, err := a.svc.Prepare(req)
		reply(rep, err)
	case "ha/handover":
		st, err := a.svc.ReceiveBundle(r.Body)
		reply(map[string]interface{}{"ok": "1", "users": st.Users}, err)
		a.mu.Lock()
		if a.dieAfterHandover { // 收完数据就失联
			a.down = true
		}
		a.mu.Unlock()
	case "ha/commit", "ha/abort":
		var b struct {
			Nonce string `json:"nonce"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		a.mu.Lock()
		drop := a.dropCommit
		a.mu.Unlock()
		if action == "ha/commit" && drop { // 提交请求收到了、没回话(对方没处理)
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
					return
				}
			}
		}
		if action == "ha/commit" {
			reply(map[string]string{"ok": "1"}, a.svc.Commit(b.Nonce))
		} else {
			reply(map[string]string{"ok": "1"}, a.svc.Abort(b.Nonce))
		}
	case "ha/incoming":
		reply(map[string]string{"stage": a.svc.IncomingStage()}, nil)
	case "report":
		a.mu.Lock()
		rev := a.revision
		a.mu.Unlock()
		reply(hub.Report{Version: a.svc.d.Version, CoreRunning: true, Revision: rev, HA: a.svc.Report()}, nil)
	case "apply":
		var snap hub.Snapshot
		_ = json.NewDecoder(r.Body).Decode(&snap)
		a.mu.Lock()
		a.revision = snap.Revision
		a.mu.Unlock()
		reply(map[string]string{"ok": "1", "revision": snap.Revision}, nil)
	default:
		reply(map[string]string{"ok": "1"}, nil)
	}
}

func (a *miniAgent) count(action string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if c == action {
			n++
		}
	}
	return n
}

// testHub 给一个容灾服务配上真实的 hub。
func testHub(t *testing.T, s *Service) *hub.Hub {
	t.Helper()
	h := hub.New(hub.Deps{DB: s.d.DB, Setting: s.setting, IsNode: s.IsNode, Version: s.d.Version, HA: s})
	s.SetHub(h)
	t.Cleanup(h.Stop)
	return h
}

func dbSetting(db *gorm.DB, k string) string {
	var v string
	db.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
	return v
}

func nodeByName(t *testing.T, db *gorm.DB, name string) model.Node {
	t.Helper()
	var n model.Node
	if err := db.Where("name = ?", name).First(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}
