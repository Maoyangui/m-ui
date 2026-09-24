//go:build !race

// 这两条测试起的是真实的内嵌数据面。竞争检测(-race)下会稳定撞上上游库自己的竞争,与 m-ui 无关:
//   - sing-box route.(*NetworkManager).Start 写接口表,同时它自己的接口监听 goroutine 在 updateInterface 里读;
//   - sing common/bufio.(*CachedConn).Close 读 c.buffer,同时拷贝 goroutine 的 Read 在写它(踢线关连接时)。
// 普通 go test 照跑(CI 的 test 步骤),只在 -race 那一步跳过。m-ui 自己在这里暴露出来的日志级别竞争已修(core/log.go)。

package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"

	"github.com/sagernet/sing-shadowsocks/shadowaead"
	M "github.com/sagernet/sing/common/metadata"
	"gorm.io/gorm"
)

// 带数据面的「重置凭据」核验:硬要求 R3 —— 重置必须真吊销,旧凭据在**运行中的**数据面里立刻不可用。
//
// user_rotate_test.go / user_rotate_legacy_test.go 只看库:令牌与凭据换没换。库换了、数据面没换,
// 泄露出去的旧链接照样能连,页面却报成功 —— 0.6.10 的两阶段标记就是这么把重置做成了空操作。
// 这条起一个真实的 runner(多用户 shadowsocks 入站,回环随机端口),用真实握手取证:
//
//	(a) 库里 alice 的凭据换新(新 ≠ 旧);
//	(b) 数据面里旧口令握手被拒、新口令握手能通(shadowsocks 每条连接各自鉴权,读入站用户表最可信的办法就是握手);
//	(c) alice 重置前已连着的那条连接被断开,limiter 里她的在线 IP 被清掉、连接计数归零;
//	(d) 整个过程没有全量重启数据面:另一个用户 bob 重置前建的连接照常回显、数据面运行秒数不回退。
//
// 只用回环地址,不联外网;协议选 shadowsocks 是因为它不需要证书、握手能用纯 Go 客户端做完。
func TestRotateUserRevokesOldCredentialsInRunningDataPlane(t *testing.T) {
	dir := t.TempDir()
	run, err := runner.New(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	db := run.DB()
	t.Cleanup(func() {
		// rotateUser 结束时会异步 SyncNow 一次(没有副机,读一遍库就返回);给它一点时间再关库
		time.Sleep(300 * time.Millisecond)
		database.Close(db)
	})
	// 隧道要能连回本机的 echo 服务:默认的私网屏蔽规则得关掉
	if err := run.SetSetting("allowPrivate", "true"); err != nil {
		t.Fatal(err)
	}
	port := rdFreePort(t)
	line := model.Line{Name: "ss", Protocol: "shadowsocks", Port: port, Enabled: true, Options: []byte(`{"method":"aes-256-gcm"}`)}
	if err := db.Create(&line).Error; err != nil {
		t.Fatal(err)
	}
	alice := model.User{Name: "alice", Enabled: true, DeviceLimit: 3, Credentials: generateCredentials("alice")}
	bob := model.User{Name: "bob", Enabled: true, Credentials: generateCredentials("bob")}
	for _, u := range []*model.User{&alice, &bob} {
		if err := db.Create(u).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.UserLine{UserId: u.Id, LineId: line.Id}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Start(); err != nil {
		t.Fatalf("数据面起不来: %v", err)
	}
	t.Cleanup(run.Stop)
	if !run.CoreRunning() {
		t.Fatal("前提:数据面在运行")
	}
	s := NewServer(run)
	echoAddr := rdEchoServer(t)
	dial := func(password string) (net.Conn, error) { return rdDial(port, password, echoAddr) }

	oldPass := rdShadowsocksPassword(t, alice.Credentials)
	bobPass := rdShadowsocksPassword(t, bob.Credentials)

	// ---- 1. 旧凭据能用:alice、bob 各建一条真实连接,limiter 里登记上 alice 的设备 ----
	connA := rdMustEcho(t, func() (net.Conn, error) { return dial(oldPass) }, "重置前 alice 的旧口令应能连")
	defer connA.Close()
	connB := rdMustEcho(t, func() (net.Conn, error) { return dial(bobPass) }, "重置前 bob 应能连")
	defer connB.Close()
	if ips := run.OnlineIPs("alice"); len(ips) == 0 {
		t.Fatal("前提:alice 的设备应已在 limiter 里登记为在线")
	}
	if n := run.ConnCounts()["alice"]; n != 1 {
		t.Fatalf("前提:alice 应有 1 条被追踪的连接,实际 %d", n)
	}
	// 数据面运行秒数至少走到 1,之后才能用"秒数不回退"证明没重启(重启后的新实例从 0 起)
	for deadline := time.Now().Add(3 * time.Second); run.Uptime() < 1; {
		if time.Now().After(deadline) {
			t.Fatal("数据面运行秒数没有增长")
		}
		time.Sleep(50 * time.Millisecond)
	}
	uptimeBefore := run.Uptime()

	// ---- 2. 走面板的重置接口 ----
	req := httptest.NewRequest("POST", "http://x/app/api/users/"+strconv.Itoa(int(alice.Id))+"/rotate", nil)
	w := httptest.NewRecorder()
	s.handleUserItem(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"link"`) {
		t.Fatalf("重置应返回新的订阅地址: %d %s", w.Code, w.Body.String())
	}

	// ---- (a) 库里换新 ----
	var after model.User
	if err := db.First(&after, alice.Id).Error; err != nil {
		t.Fatal(err)
	}
	newPass := rdShadowsocksPassword(t, after.Credentials)
	if string(after.Credentials) == string(alice.Credentials) || newPass == oldPass {
		t.Fatal("(a) 重置后凭据必须换新")
	}
	if len(after.SubToken) < 20 {
		t.Fatalf("(a) 订阅地址应换成随机令牌: %q", after.SubToken)
	}

	// ---- (c) 已连着的设备被断开,登记被清 ----
	rdMustFail(t, connA, "(c) alice 重置前的连接必须被断开")
	if n := run.ConnCounts()["alice"]; n != 0 {
		t.Fatalf("(c) 踢线后 alice 不该还有被追踪的连接,实际 %d", n)
	}
	if ips := run.OnlineIPs("alice"); len(ips) != 0 {
		t.Fatalf("(c) 踢线后 limiter 里 alice 的在线 IP 应被清掉,实际 %v", ips)
	}

	// ---- (b) 数据面里旧口令已不存在、新口令存在:用真实握手取证 ----
	rdMustNotConnect(t, func() (net.Conn, error) { return dial(oldPass) }, "(b) 旧口令在运行中的数据面里必须立刻被拒")
	connA2 := rdMustEcho(t, func() (net.Conn, error) { return dial(newPass) }, "(b) 新口令应当已经热更新进数据面")
	defer connA2.Close()
	if ips := run.OnlineIPs("alice"); len(ips) == 0 {
		t.Fatal("(b) 新口令连上后 alice 的设备应重新登记为在线")
	}

	// ---- (d) 没有全量重启:旁观者 bob 的老连接照常,运行秒数不回退 ----
	if err := rdEcho(connB, "bob-still-here"); err != nil {
		t.Fatalf("(d) 重置 alice 不该影响 bob 已建立的连接(全量重启才会断它): %v", err)
	}
	if n := run.ConnCounts()["bob"]; n != 1 {
		t.Fatalf("(d) bob 的连接应仍被追踪,实际 %d", n)
	}
	if !run.CoreRunning() {
		t.Fatal("(d) 数据面必须仍在运行")
	}
	if up := run.Uptime(); up < uptimeBefore {
		t.Fatalf("(d) 数据面运行秒数回退(%d → %d):实例被重建了", uptimeBefore, up)
	}
	if st := run.ReloadStatus(); !st.OK || st.Op != "撤销凭据" {
		t.Fatalf("(d) 重置应走「撤销凭据」的热更新路径且成功,实际 %+v", st)
	}
	if v := rdSetting(db, "secureReloadPending"); v != "" {
		t.Fatalf("热更新成功后待重试标记应清掉,实际 %q", v)
	}
}

// rdShadowsocksPassword 取出用户凭据里 shadowsocks(AEAD 算法)那一份口令 —— 渲染进入站的就是它。
func rdShadowsocksPassword(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var creds map[string]map[string]interface{}
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatalf("凭据解析失败: %v", err)
	}
	pw, _ := creds["shadowsocks"]["password"].(string)
	if pw == "" {
		t.Fatalf("凭据里没有 shadowsocks 口令: %s", raw)
	}
	return pw
}

func rdSetting(db *gorm.DB, key string) string {
	var v string
	db.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v
}

// rdDial 用给定口令向本机的 shadowsocks 入站握手,目标是 echo 服务。
// AEAD 客户端在 DialConn 时就把 盐 + 加密的目标地址 发出去了;服务端拿这一段试遍用户表,
// 口令不对就关连接 —— 所以"连不上"要靠之后的读来判断,拨号本身不会报错。
func rdDial(port int, password, target string) (net.Conn, error) {
	method, err := shadowaead.New("aes-256-gcm", nil, password)
	if err != nil {
		return nil, err
	}
	raw, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		return nil, err
	}
	conn, err := method.DialConn(raw, M.ParseSocksaddr(target))
	if err != nil {
		raw.Close()
		return nil, err
	}
	return conn, nil
}

func rdEcho(conn net.Conn, msg string) error {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetDeadline(time.Time{})
	if _, err := conn.Write([]byte(msg)); err != nil {
		return err
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != msg {
		return fmt.Errorf("回显不对: %q", buf)
	}
	return nil
}

// rdMustEcho 拨号直到隧道通(允许几次尝试),返回还开着的连接。
func rdMustEcho(t *testing.T, dial func() (net.Conn, error), why string) net.Conn {
	t.Helper()
	var last error
	for i := 0; i < 6; i++ {
		conn, err := dial()
		if err == nil {
			if err = rdEcho(conn, fmt.Sprintf("ping-%d", i)); err == nil {
				return conn
			}
			conn.Close()
		}
		last = err
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s: %v", why, last)
	return nil
}

// rdMustFail 老连接必须在几秒内读到错误(被踢线 → 服务端关掉 → 客户端读到 EOF / 连接重置)。
func rdMustFail(t *testing.T, conn net.Conn, why string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 16)
	_, _ = conn.Write([]byte("x"))
	if _, err := conn.Read(buf); err == nil {
		t.Fatalf("%s:连接还活着", why)
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("%s:10 秒内没断", why)
	}
}

// rdMustNotConnect 用旧口令拨号,每次都必须失败:握手被拒时服务端直接关掉,读到 EOF / 连接重置;
// 既不通也不断(超时)同样算失败 —— 那说明旧口令还在表里、只是路由没走通。
func rdMustNotConnect(t *testing.T, dial func() (net.Conn, error), why string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		conn, err := dial()
		if err != nil {
			continue
		}
		err = rdEcho(conn, "should-fail")
		conn.Close()
		if err == nil {
			t.Fatalf("%s(第 %d 次却通了)", why, i+1)
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("%s:10 秒既不通也不断", why)
		}
	}
}

func rdEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	return ln.Addr().String()
}

func rdFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
