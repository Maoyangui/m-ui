package hub

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 换了一台 VPS 接着用同一行(域名、API 地址不变,只改令牌):新机器是新装的,带一本新的流量账本(新纪元)。
// 主机对换了纪元的账本,第一份成功并入的报告只建基线、不计增量(见 ledgerEpochState),所以
// "从新机器开始服务到主机第一次并入它的报告"这段流量不计。这里钉住两件事:
//   - 改令牌后的第一轮同步里,新机器先收到整份推送(拿到用户表才开始服务)、紧接着同一轮就被拉报告,
//     不计的只有这两个请求之间新机器跑的流量;
//   - 基线之后新机器上的流量照常全额计入。
//
// 旧机器上最后一次被拉报告之后的流量拉不到了(这一行已经指向新机器),这一段也不计。
func TestNodeSwitchNewLedgerBaselineRightAfterFirstPush(t *testing.T) {
	db := openDB(t, "switch-ledger.db").DB
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})

	type agent struct {
		mu       sync.Mutex
		token    string
		epoch    string
		hasUsers bool     // 收到过带用户的推送才开始服务(新装的机器在那之前没有用户)
		up, down int64    // 这本账的累计计数
		calls    []string // apply / report 的先后
		rev      string
	}
	serve := func(a *agent) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Agent-Token") != a.token {
				http.Error(w, `{"error":"令牌错误"}`, http.StatusUnauthorized)
				return
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			switch {
			case strings.HasSuffix(r.URL.Path, "/agent/apply"):
				var snap Snapshot
				_ = json.NewDecoder(r.Body).Decode(&snap)
				a.calls = append(a.calls, "apply")
				a.rev = snap.Revision
				a.hasUsers = a.hasUsers || len(snap.Users) > 0
				json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
			case strings.HasSuffix(r.URL.Path, "/agent/report"):
				a.calls = append(a.calls, "report")
				rep := Report{Version: "t", CoreRunning: true, Revision: a.rev, LedgerEpoch: a.epoch}
				if a.hasUsers {
					rep.Counters = []model.AgentCounter{{UserName: "alice", Up: a.up, Down: a.down}}
				}
				json.NewEncoder(w).Encode(rep)
			default:
				w.Write([]byte(`{"ok":"1"}`))
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	oldA := &agent{token: "old-token", epoch: "E-old"}
	newA := &agent{token: "new-token", epoch: "E-new"}
	oldSrv, newSrv := serve(oldA), serve(newA)
	var target atomic.Value
	target.Store(oldSrv.Listener.Addr().String())

	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	h.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, target.Load().(string))
	}
	t.Cleanup(h.CloseIdleConnections)
	db.Create(&model.Node{Name: "直连", ApiUrl: "http://node.example:2053/app/", Token: "old-token", Enabled: true})
	round := func() { h.tick(); h.settle() }
	usage := func() (int64, int64) {
		var u model.User
		db.Where("name = ?", "alice").First(&u)
		return u.Up, u.Down
	}
	traffic := func(a *agent, up, down int64) {
		a.mu.Lock()
		a.up += up
		a.down += down
		a.mu.Unlock()
	}

	// 旧机器:第一份只建基线(新加的副机),之后照常计
	round()
	traffic(oldA, 100, 200)
	round()
	if up, down := usage(); up != 100 || down != 200 {
		t.Fatalf("旧机器上的流量应计入: %d/%d", up, down)
	}
	// 旧机器又跑了一段,还没被拉报告就换机了:这段拉不到,不计
	traffic(oldA, 7, 9)

	// 换机:域名解析到新机器,面板上只把令牌改成新机器的
	target.Store(newSrv.Listener.Addr().String())
	if err := db.Model(&model.Node{}).Where("name = ?", "直连").Update("token", "new-token").Error; err != nil {
		t.Fatal(err)
	}
	round()
	newA.mu.Lock()
	calls, served := append([]string(nil), newA.calls...), newA.hasUsers
	newA.mu.Unlock()
	if !served || len(calls) < 2 || calls[0] != "apply" || calls[1] != "report" {
		t.Fatalf("改令牌后的第一轮应先给新机器整份推送、同一轮紧接着拉报告,实际 %v(收到用户表 %v)", calls, served)
	}
	if up, down := usage(); up != 100 || down != 200 {
		t.Fatalf("新账本的第一份报告只建基线,不该计入: %d/%d", up, down)
	}
	// 基线之后,新机器上的流量全额计入
	traffic(newA, 40, 50)
	round()
	if up, down := usage(); up != 140 || down != 250 {
		t.Fatalf("新机器建好基线之后的流量应全额计入: %d/%d", up, down)
	}
}
