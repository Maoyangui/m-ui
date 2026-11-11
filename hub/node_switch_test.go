package hub

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// switchEnv 两台副机("旧机器""新机器",各自的令牌),一个可切换的"域名解析":node.example 解析到哪台由 point 决定。
func switchEnv(t *testing.T, name string) (h *Hub, load func() model.Node, point func(newer bool), ping func() (string, error)) {
	t.Helper()
	db := openDB(t, name).DB
	agent := func(token, host string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Agent-Token") != token {
				http.Error(w, `{"error":"令牌错误"}`, http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"ok":"1","hostname":%q}`, host)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	oldSrv, newSrv := agent("old-token", "old"), agent("new-token", "new")
	var target atomic.Value
	target.Store(oldSrv.Listener.Addr().String())
	h = New(Deps{DB: db})
	h.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, target.Load().(string))
	}
	t.Cleanup(h.CloseIdleConnections)
	db.Create(&model.Node{Name: "直连", ApiUrl: "http://node.example:2053/app/", Token: "old-token", Enabled: true})
	load = func() model.Node {
		var n model.Node
		if err := db.First(&n, 1).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	point = func(newer bool) {
		if newer {
			target.Store(newSrv.Listener.Addr().String())
		} else {
			target.Store(oldSrv.Listener.Addr().String())
		}
	}
	ping = func() (string, error) {
		out, err := h.Ping(load())
		host, _ := out["hostname"].(string)
		return host, err
	}
	return h, load, point, ping
}

func setNodeToken(t *testing.T, h *Hub, tok string) {
	t.Helper()
	if err := h.d.DB.Model(&model.Node{}).Where("id = ?", 1).Update("token", tok).Error; err != nil {
		t.Fatal(err)
	}
}

// 生产上的"直连":换了一台 VPS,域名和 API 地址不变,只在主机上把令牌改成新机器的。之后推送成功、
// 下发外部设备租约却每 5 秒报一次 401 令牌错误 —— 改令牌不换客户端,池里到旧机器的长连接 5 秒用一次、永不空闲,
// 一直被复用;偶尔新开的连接才到了新机器。现在改了令牌就换客户端,请求全部落在新机器上。
func TestNodeSwitchByTokenGoesToNewServer(t *testing.T) {
	h, load, point, ping := switchEnv(t, "switch.db")
	if host, err := ping(); err != nil || host != "old" {
		t.Fatalf("起初应连旧机器: %q %v", host, err)
	}
	// 换机:域名解析到新机器,面板上只把令牌改成新机器的
	point(true)
	setNodeToken(t, h, "new-token")
	for i := 1; i <= 3; i++ {
		if host, err := ping(); err != nil || host != "new" {
			t.Fatalf("改令牌后第 %d 次请求应落在新机器上: %q %v", i, host, err)
		}
		var ack struct {
			OK string `json:"ok"`
		}
		if err := h.request(load(), "POST", "external-ips", map[string][]string{"u": {"1.2.3.4"}}, &ack); err != nil || ack.OK != "1" {
			t.Fatalf("改令牌后第 %d 次下发设备租约应成功(以前一直 401): %v %q", i, err, ack.OK)
		}
	}
}

// 先改了令牌、域名还没切过去:这时连到的是旧机器,401。等域名切到新机器,不能再复用那条连着旧机器的连接 ——
// 401 时把空闲连接关掉,下一次重新解析、重新连。
func TestNodeSwitchTokenBeforeDNS(t *testing.T) {
	h, _, point, ping := switchEnv(t, "switch2.db")
	if _, err := ping(); err != nil {
		t.Fatal(err)
	}
	setNodeToken(t, h, "new-token")
	if _, err := ping(); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("域名还指着旧机器时应 401: %v", err)
	}
	point(true)
	if host, err := ping(); err != nil || host != "new" {
		t.Fatalf("域名切到新机器后应立刻连上新机器: %q %v", host, err)
	}
}

// 跳过证书校验的副机:令牌没被认可(401)时不记指纹 —— 换服务器时先改了令牌、域名还指着旧机器,
// 以前一握手就把旧机器的指纹记下了,域名切过去又报"指纹变了"。认可(200)以后才记。
func TestPinnedFingerprintRememberedOnlyWhenAccepted(t *testing.T) {
	db := openDB(t, "pin-accept.db").DB
	c1 := selfSigned(t, "node")
	srv := tlsNode(t, c1) // 只认令牌 tok
	db.Create(&model.Node{Name: "az", ApiUrl: srv.URL, Token: "wrong", Insecure: true})
	h := New(Deps{DB: db})
	defer h.CloseIdleConnections()
	load := func() model.Node {
		var n model.Node
		db.First(&n, 1)
		return n
	}
	if _, err := h.Ping(load()); err == nil {
		t.Fatal("令牌不对应失败")
	}
	if fp := load().CertFP; fp != "" {
		t.Fatalf("令牌没被认可时不该记住指纹: %q", fp)
	}
	db.Model(&model.Node{}).Where("id = ?", 1).Update("token", "tok")
	if _, err := h.Ping(load()); err != nil {
		t.Fatalf("令牌对了应连上: %v", err)
	}
	if fp := load().CertFP; fp != fingerprint(c1) {
		t.Fatalf("被认可后应记住指纹: %q", fp)
	}
}

// API 地址或令牌变了就当一台新机器:旧机器的推送记录、退避、状态、上报的在线设备都作废,下一轮立即整份推送。
// 以前新机器(没应用过任何修订)报上来的修订是空,主机按"已推过"不推,最长要等 10 分钟的定期重推。
func TestNodeIdentityChangeForgetsOldServer(t *testing.T) {
	h := New(Deps{})
	n := model.Node{Id: 7, Name: "直连", ApiUrl: "http://a/app/", Token: "t1"}
	h.noteIdentity(n) // 第一次见:只记下
	seed := func() {
		h.pushed[7] = "rev"
		h.status[7] = &NodeStatus{Name: "直连", Revision: "rev", LastPush: 100}
		h.pushFail[7] = &pushFailure{rev: "rev"}
		h.remote[7] = map[string][]string{"u": {"1.1.1.1"}}
		h.remoteAt[7] = 100
	}
	seed()
	h.noteIdentity(n)
	if h.pushed[7] != "rev" || h.status[7] == nil || h.remote[7] == nil {
		t.Fatal("还是同一台机器,状态不能动")
	}
	n.Token = "t2"
	h.noteIdentity(n)
	if _, ok := h.pushed[7]; ok || h.status[7] != nil || h.pushFail[7] != nil || h.remote[7] != nil {
		t.Fatalf("换了令牌,旧机器的推送记录 / 状态 / 在线设备应作废: pushed=%v status=%v fail=%v remote=%v", h.pushed[7], h.status[7], h.pushFail[7], h.remote[7])
	}
	seed()
	n.ApiUrl = "http://b/app/"
	h.noteIdentity(n)
	if _, ok := h.pushed[7]; ok {
		t.Fatal("换了 API 地址,同样当新机器")
	}
}
