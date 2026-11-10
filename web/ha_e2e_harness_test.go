package web

import (
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/runner"

	"gorm.io/gorm"
)

// 主机容灾与设为主机的进程内多实例集成测试的底座:每台都是真实的 runner + 真实的面板接口(agent、ha),挂在各自的
// TLS httptest 上,主机用真实的 hub 同步。"重启"= 关掉库、换上待换的新库(ha.ApplyPending)、重新 runner.New,
// 地址和证书不变(和真机重启一样)。时间用可拨的钟(租约过期、超时)。完整流程在 ha_e2e_test.go。

type haClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *haClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *haClock) add(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

type haInst struct {
	t        *testing.T
	name     string
	path     string
	token    string
	ts       *httptest.Server
	clk      *haClock
	mu       sync.Mutex
	run      *runner.Runner
	srv      *Server
	down     bool
	fail     string // 这个 agent 动作回 500(模拟中途失败)
	restarts int
}

func newHAInst(t *testing.T, name string, node bool, clk *haClock) *haInst {
	t.Helper()
	dir := t.TempDir()
	in := &haInst{t: t, name: name, path: filepath.Join(dir, "m-ui.db"), token: "tok-" + strings.ToLower(name), clk: clk}
	in.ts = httptest.NewUnstartedServer(http.HandlerFunc(in.serve))
	in.ts.StartTLS()
	t.Cleanup(in.ts.Close)
	certPath, keyPath := filepath.Join(dir, "panel.crt"), filepath.Join(dir, "panel.key")
	f, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: in.ts.Certificate().Raw})
	f.Close()
	os.WriteFile(keyPath, []byte("unused"), 0o600)
	port := in.ts.Listener.Addr().(*net.TCPAddr).Port
	kv := map[string]string{"publicIp": "127.0.0.1", "webPort": strconv.Itoa(port), "webPath": "/app/",
		"webCertFile": certPath, "webKeyFile": keyPath, "nodeToken": in.token}
	if node {
		kv["nodeMode"] = "true"
	}
	if err := runner.SetSettings(in.path, kv); err != nil {
		t.Fatal(err)
	}
	in.start()
	t.Cleanup(in.stop)
	return in
}

func (in *haInst) url() string { return in.ts.URL + "/app/" }

func (in *haInst) start() {
	in.t.Helper()
	if _, err := ha.ApplyPending(in.path); err != nil {
		in.t.Fatal(err)
	}
	run, err := runner.New(in.path)
	if err != nil {
		in.t.Fatal(err)
	}
	run.SetRestartFunc(func(time.Duration) {
		in.mu.Lock()
		in.restarts++
		in.mu.Unlock()
	})
	run.HA().SetNow(in.clk.now)
	in.mu.Lock()
	in.run, in.srv = run, NewServer(run)
	in.mu.Unlock()
}

func (in *haInst) stop() {
	in.mu.Lock()
	run := in.run
	in.run, in.srv = nil, nil
	in.mu.Unlock()
	if run == nil {
		return
	}
	run.Stop()
	run.Hub().Stop()
	run.HA().Stop()
	_ = database.Close(run.DB())
}

// restart 模拟重启:关掉、换上待换的新库、重新起来。
func (in *haInst) restart() {
	in.t.Helper()
	in.stop()
	in.mu.Lock()
	in.restarts = 0
	in.mu.Unlock()
	in.start()
}

func (in *haInst) restarted() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.restarts > 0
}

func (in *haInst) setDown(v bool) {
	in.mu.Lock()
	in.down = v
	in.mu.Unlock()
}

func (in *haInst) setFail(action string) {
	in.mu.Lock()
	in.fail = action
	in.mu.Unlock()
}

func (in *haInst) R() *runner.Runner {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.run
}

func (in *haInst) HA() *ha.Service { return in.R().HA() }

func (in *haInst) DB() *gorm.DB { return in.R().DB() }

func (in *haInst) serve(w http.ResponseWriter, r *http.Request) {
	in.mu.Lock()
	s, down, fail := in.srv, in.down, in.fail
	in.mu.Unlock()
	if down || s == nil {
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	if fail != "" && strings.HasSuffix(r.URL.Path, "/agent/"+fail) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "模拟失败"})
		return
	}
	switch p := r.URL.Path; {
	case strings.HasPrefix(p, innerBase+"api/agent/"):
		s.handleAgent(w, r)
	case strings.HasPrefix(p, innerBase+"api/ha/"):
		s.handleHASub(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (in *haInst) setting(k string) string {
	var v string
	in.DB().Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
	return v
}

func (in *haInst) isNode() bool { return in.setting("nodeMode") == "true" }

func (in *haInst) sync() { in.R().Hub().RunOnce() }

// localId 本机在服务器表里的 id。
func (in *haInst) localId() uint {
	var n model.Node
	in.DB().Where("is_local = ?", true).First(&n)
	return n.Id
}

// addNode 主机的服务器表里加一台副机(和面板上"加副机"一样:API 地址 + 令牌,证书指纹首次连接时记住)。
func addNode(t *testing.T, m, n *haInst, sort int) uint {
	t.Helper()
	row := model.Node{Name: n.name, ApiUrl: n.url(), Token: n.token, Insecure: true, Enabled: true, Sort: sort}
	if err := m.DB().Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.Id
}

// seedHAMaster 主机上的业务数据:管理员、代理(带密码)、套餐、用户(带用量)、订阅域名与端口。
func seedHAMaster(t *testing.T, m *haInst) {
	t.Helper()
	db := m.DB()
	db.Model(&model.Node{}).Where("is_local = ?", true).Updates(map[string]interface{}{"name": "HK", "sort": 1})
	hash, _ := hashPassword("boss-pass")
	db.Where("1 = 1").Delete(&model.Admin{})
	db.Create(&model.Admin{Username: "boss", Password: hash})
	db.Create(&model.Reseller{Name: "agent", Password: "$2a$10$reseller", ApiToken: "rs-api", Enabled: true})
	db.Create(&model.Plan{Name: "月付", Days: 30, VolumeGB: 100})
	alice := model.User{Name: "alice", Enabled: true, Credentials: generateCredentials("alice"), Up: 1000, Down: 2000}
	db.Create(&alice)
	db.Create(&model.User{Name: "bob", Enabled: true, Credentials: generateCredentials("bob")})
	for k, v := range map[string]string{"subDomain": "sub.example.com", "subPort": "2096", "subPath": "/s/", "apiToken": "api-secret", "timezone": "Asia/Shanghai"} {
		db.Exec("INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", k, v)
	}
}

// haAudited 库里有没有这条主机容灾的审计。
func haAudited(in *haInst, action string) bool {
	var n int64
	in.DB().Model(&model.Change{}).Where("key = ? AND action = ?", "ha", action).Count(&n)
	return n > 0
}

func waitSwitch(t *testing.T, m *haInst) ha.SwitchState {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		st := m.HA().Switch()
		if !st.Active() && !m.HA().Switching() {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("设为主机一直没结束: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// checkMasterData 新主机的库里是原主机的业务数据(json:"-" 的密码、令牌原样带过来)。
func checkMasterData(t *testing.T, in *haInst, wantUp int64) {
	t.Helper()
	db := in.DB()
	var admin model.Admin
	if err := db.Where("username = ?", "boss").First(&admin).Error; err != nil || !checkPassword("boss-pass", admin.Password) {
		t.Fatalf("%s:管理员要带过来(密码不变): %v", in.name, err)
	}
	var rs model.Reseller
	if err := db.Where("name = ?", "agent").First(&rs).Error; err != nil || rs.Password != "$2a$10$reseller" || rs.ApiToken != "rs-api" {
		t.Fatalf("%s:代理与它的密码、API 令牌要带过来: %+v %v", in.name, rs, err)
	}
	var plans int64
	db.Model(&model.Plan{}).Count(&plans)
	var alice model.User
	db.Where("name = ?", "alice").First(&alice)
	if plans != 1 || alice.Up < wantUp {
		t.Fatalf("%s:套餐、用户用量要带过来: plans=%d alice=%+v", in.name, plans, alice)
	}
	if in.setting("apiToken") != "api-secret" {
		t.Fatalf("%s:全局设置要带过来", in.name)
	}
	var sessions int64
	db.Model(&model.Session{}).Count(&sessions)
	if sessions != 0 {
		t.Fatalf("%s:会话不带(所有人重新登录)", in.name)
	}
}

// 同伴接口不要会话,但只认集群密钥签名;按来源限流。
func TestHAPeerEndpointAuth(t *testing.T) {
	clk := &haClock{}
	tw := newHAInst(t, "TW", true, clk)
	call := func() int {
		req := httptest.NewRequest(http.MethodPost, "/app/api/ha/peer/status", strings.NewReader("{}"))
		req.RemoteAddr = "198.51.100.7:1234"
		req.Header.Set("X-Peer-Node", "3")
		req.Header.Set("X-Peer-Ts", strconv.FormatInt(time.Now().Unix(), 10))
		req.Header.Set("X-Peer-Sig", "00")
		w := httptest.NewRecorder()
		tw.srv.handleHASub(w, req)
		return w.Code
	}
	if code := call(); code != http.StatusUnauthorized {
		t.Fatalf("签名不对应 401: %d", code)
	}
	limited := false
	for i := 0; i < 20; i++ {
		if call() == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("同一来源连续请求要限流")
	}
}
