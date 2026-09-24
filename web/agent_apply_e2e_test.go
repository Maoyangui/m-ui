//go:build !race

// 这两条测试起的是真实的内嵌数据面。竞争检测(-race)下会稳定撞上上游库自己的竞争,与 m-ui 无关:
//   - sing-box route.(*NetworkManager).Start 写接口表,同时它自己的接口监听 goroutine 在 updateInterface 里读;
//   - sing common/bufio.(*CachedConn).Close 读 c.buffer,同时拷贝 goroutine 的 Read 在写它(踢线关连接时)。
// 普通 go test 照跑(CI 的 test 步骤),只在 -race 那一步跳过。m-ui 自己在这里暴露出来的日志级别竞争已修(core/log.go)。

package web

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

// 主机 → 副机推送配置的端到端测试:真实的 hub.PushNow 打到真实的 handleAgentApply
// (令牌鉴权 → 快照落库 → 数据面重载 → 确认修订),副机这边是真实的 runner + 内嵌数据面,
// 中间只有一个 httptest 服务器,没有任何桩。钉住四件事:
//
//  1. 合法推送回 200,主机侧 PushNow 无错;副机报告里的修订号等于推过去的那份、ReloadPending 为 false。
//  2. 只改用户的推送走热更新、不重启数据面:推送前经 mixed 入站开一条真实隧道,推送后它还活着
//     (真重启会关掉 connection manager,隧道必断 —— 第 3 条里正好反证了这一点)。
//  3. 内核起不来的配置(新线路的端口被别的进程占着)回 503;主机拿到的错误是 "HTTP 503: …",
//     即 hub 里的 *httpStatusError —— pushRejected 判"该退避"的那一类;报告里 ReloadPending 为 true、
//     修订号照旧前进(落库就写,不等数据面,与 hub.Report 的字段注释一致)。同一修订立刻重推撞上 runner 的
//     冷却期(lastFailedRaw),不再拆正在服务的数据面:回滚后新开的隧道活着。管理员改回线路后推送成功、
//     待重载标记清掉、数据面也没有为此重启。
//  4. 令牌错误 401(独立的 TestAgentApplyAuthAndValidation 还覆盖了 403 / 400 / 503)。
func TestAgentApplyEndToEnd(t *testing.T) {
	// ---- 主机:两条线路、一个用户 ----
	masterDB := openAgentTestDB(t, "master.db")
	masterDB.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	// 隧道要连回本机的回显服务;allowPrivate 在 SyncedSettings 里,会随快照下发到副机
	masterDB.Create(&model.Setting{Key: "allowPrivate", Value: "true"})
	probePort, hotPort := freeTCPPort(t), freeTCPPort(t)
	// probe:mixed 入站不支持原地换用户表(用户变了会拆重建这个入站),所以它的用户始终只有 alice,
	// 专门用来开隧道观察数据面有没有重启;hot:多用户 shadowsocks 入站支持原地换用户,承接"用户列表变化"
	probe := model.Line{Name: "probe", Protocol: "mixed", Port: probePort, Enabled: true, Options: json.RawMessage(`{}`)}
	hot := model.Line{Name: "hot", Protocol: "shadowsocks", Port: hotPort, Enabled: true,
		Options: json.RawMessage(`{"method":"aes-128-gcm","password":"e2e-server-key"}`)}
	masterDB.Create(&probe)
	masterDB.Create(&hot)
	alice := model.User{Name: "alice", Enabled: true, DeviceLimit: 3, Volume: 10 << 30, SpeedDown: 50,
		Credentials: generateCredentials("alice")}
	masterDB.Create(&alice)
	masterDB.Create(&model.UserLine{UserId: alice.Id, LineId: probe.Id})
	masterDB.Create(&model.UserLine{UserId: alice.Id, LineId: hot.Id})
	alicePass := socksPassword(t, alice.Credentials)
	masterSetting := func(k string) string {
		var v string
		masterDB.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
		return v
	}
	expectRevision := func() string {
		t.Helper()
		snap, err := hub.BuildSnapshot(masterDB, masterSetting)
		if err != nil {
			t.Fatal(err)
		}
		return snap.Revision
	}

	// ---- 副机:真实 runner(副机模式、预置令牌)+ 真实 agent 接口 ----
	const token = "e2e-agent-token-0123456789"
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
	if run.CoreRunning() {
		t.Fatal("前提:副机数据面还没起,第一份快照负责把它拉起来")
	}

	// ---- 主机侧的 hub 指向这台副机 ----
	h := hub.New(hub.Deps{DB: masterDB, Setting: masterSetting, IsNode: func() bool { return false }, Version: Version})
	nodeRow := model.Node{Name: "副机", ApiUrl: agentSrv.URL + "/app/", Token: token, Enabled: true, Sort: 2}
	masterDB.Create(&nodeRow)

	// ---- 4. 令牌错误:401,什么都没应用 ----
	badToken := nodeRow
	badToken.Token = "wrong-token"
	if err := h.PushNow(badToken); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("令牌错误应被 401 拒绝,主机侧看到 HTTP 401: %v", err)
	}
	if got := s.setting("hubRevision"); got != "" {
		t.Fatalf("令牌错误的推送不该落库,hubRevision=%q", got)
	}

	// ---- 1. 首次合法推送:200、修订号一致、ReloadPending=false、数据面被拉起 ----
	revA := expectRevision()
	if err := h.PushNow(nodeRow); err != nil {
		t.Fatalf("首次推送应成功: %v", err)
	}
	rep := agentReport(t, agentSrv.URL, token)
	if rep.Revision != revA {
		t.Fatalf("报告里的修订号应等于推送的 %s,实际 %s", revA, rep.Revision)
	}
	if rep.ReloadPending {
		t.Fatal("应用成功后 ReloadPending 必须为 false")
	}
	if !rep.CoreRunning || !run.CoreRunning() {
		t.Fatal("首次快照应把副机数据面拉起来")
	}
	if got := s.setting("hubReloadPending"); got != "" {
		t.Fatalf("应用成功后待重载标记应清空,实际 %q", got)
	}
	if st := run.ReloadStatus(); st.Op != "全量重载" || !st.OK {
		t.Fatalf("数据面没在跑时首次应用应走全量重载且成功: %+v", st)
	}
	if n := run.InboundCount(); n != 2 {
		t.Fatalf("副机应起两条入站,实际 %d", n)
	}
	var users int64
	run.DB().Model(&model.User{}).Count(&users)
	if users != 1 {
		t.Fatalf("副机库里应有 1 个用户,实际 %d", users)
	}

	// 经 probe 入站开一条真实隧道到本机回显服务:它是"数据面有没有重启"的探针
	echoPort := startEchoServer(t)
	if code := tunnelStatus(t, probePort, "alice", "wrong-pass", echoPort); code != http.StatusProxyAuthRequired {
		t.Fatalf("错口令应被入站拒绝(期望 407),实际 %d", code)
	}
	tun1 := openTunnel(t, probePort, "alice", alicePass, echoPort)
	defer tun1.Close()
	if err := tun1.echo("round-1"); err != nil {
		t.Fatalf("隧道应能回显: %v", err)
	}

	// ---- 2. 只改用户(新增 bob、改限额):热更新,不重启,隧道活着 ----
	bob := model.User{Name: "bob", Enabled: true, DeviceLimit: 2, Volume: 5 << 30, Credentials: generateCredentials("bob")}
	masterDB.Create(&bob)
	masterDB.Create(&model.UserLine{UserId: bob.Id, LineId: hot.Id})
	masterDB.Model(&model.User{}).Where("id = ?", alice.Id).Update("device_limit", 5)
	revB := expectRevision()
	if revB == revA {
		t.Fatal("前提:用户变化应改变修订号")
	}
	if err := h.PushNow(nodeRow); err != nil {
		t.Fatalf("只改用户的推送应成功: %v", err)
	}
	rep = agentReport(t, agentSrv.URL, token)
	if rep.Revision != revB || rep.ReloadPending {
		t.Fatalf("热更新后报告应是新修订且无待重载: %+v", rep)
	}
	if st := run.ReloadStatus(); st.Op != "热更新用户" || !st.OK {
		t.Fatalf("只有用户变化必须走热更新路径(不是全量重载): %+v", st)
	}
	if err := tun1.echo("round-2"); err != nil {
		t.Fatalf("只改用户的推送不该重启数据面,推送前开的隧道应仍然可用: %v", err)
	}
	run.DB().Model(&model.User{}).Count(&users)
	if users != 2 {
		t.Fatalf("副机库里应有 2 个用户,实际 %d", users)
	}
	var nodeAlice model.User
	run.DB().Where("name = ?", "alice").First(&nodeAlice)
	if nodeAlice.DeviceLimit != 5 {
		t.Fatalf("限额应随快照落到副机,实际 DeviceLimit=%d", nodeAlice.DeviceLimit)
	}

	// ---- 3. 内核起不来的配置:新线路的端口被本测试占着 ----
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyLine := model.Line{Name: "busy", Protocol: "mixed", Port: busy.Addr().(*net.TCPAddr).Port, Enabled: true, Options: json.RawMessage(`{}`)}
	masterDB.Create(&busyLine)
	revC := expectRevision()
	err = h.PushNow(nodeRow)
	if err == nil {
		t.Fatal("内核起不来的配置不能被确认为已应用")
	}
	// "HTTP 503: …" 是 hub 里 *httpStatusError 的格式化结果:pushRejected 用 errors.As 认它,
	// 同一修订随后进入 pushBackoff 退避(分类与退避本身由 hub/push_fail_test.go 钉住)
	if !strings.HasPrefix(err.Error(), "HTTP 503:") || !strings.Contains(err.Error(), "本机数据面应用失败") {
		t.Fatalf("主机侧应拿到 HTTP 503 + 应用失败原因,实际: %v", err)
	}
	rep = agentReport(t, agentSrv.URL, token)
	if !rep.ReloadPending {
		t.Fatal("数据面没应用成功,报告里 ReloadPending 必须为 true(主机据此不显示已同步)")
	}
	if rep.Revision != revC {
		t.Fatalf("修订号在落库时就写了(与 hub.Report 注释一致),报告应是 %s,实际 %s", revC, rep.Revision)
	}
	if !rep.CoreRunning || !run.CoreRunning() {
		t.Fatal("新配置起不来应回滚到上一份,数据面得在跑")
	}
	if got := s.setting("hubReloadPending"); got != revC {
		t.Fatalf("待重载标记应记下失败的修订 %s,实际 %q", revC, got)
	}
	if st := run.ReloadStatus(); st.OK || !strings.Contains(st.Error, "回滚") {
		t.Fatalf("第一次失败应是真重启 + 回滚: %+v", st)
	}
	// 第一次失败是真的拆过数据面(不可避免的一次断线):旧隧道被数据面主动关掉(不是等到读超时),
	// 这反证了上面"隧道活着 = 没重启"
	if err := tun1.echo("round-3"); err == nil {
		t.Fatal("真重启 + 回滚之后,旧隧道不可能还活着 —— 那样第 2 步的探针就不成立")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("旧隧道应被重启的数据面关掉,而不是悬着直到超时: %v", err)
	}

	// 同一修订立刻重推(主机退避到期 / 手动重推都会这样):runner 的冷却期不许再拆数据面
	tun2 := openTunnel(t, probePort, "alice", alicePass, echoPort)
	defer tun2.Close()
	if err := tun2.echo("round-4"); err != nil {
		t.Fatalf("回滚后的数据面应在服务: %v", err)
	}
	err = h.PushNow(nodeRow)
	if err == nil || !strings.HasPrefix(err.Error(), "HTTP 503:") || !strings.Contains(err.Error(), "冷却期") {
		t.Fatalf("同一份起不来的配置在冷却期内应回 503 并说明冷却: %v", err)
	}
	if err := tun2.echo("round-5"); err != nil {
		t.Fatalf("冷却期内重推不能再拆正在服务的数据面(这就是每 5 秒断线一次的风暴): %v", err)
	}
	rep = agentReport(t, agentSrv.URL, token)
	if !rep.ReloadPending || rep.Revision != revC {
		t.Fatalf("冷却期内重推仍未应用成功,报告应保持待重载: %+v", rep)
	}

	// 管理员把坏线路删掉:渲染结果回到当前生效的那份,应用成功、标记清掉、也不重启
	masterDB.Delete(&busyLine)
	revD := expectRevision()
	if err := h.PushNow(nodeRow); err != nil {
		t.Fatalf("改回线路后推送应成功: %v", err)
	}
	rep = agentReport(t, agentSrv.URL, token)
	if rep.Revision != revD || rep.ReloadPending || !rep.CoreRunning {
		t.Fatalf("修好后报告应是新修订、无待重载、数据面在跑: %+v", rep)
	}
	if got := s.setting("hubReloadPending"); got != "" {
		t.Fatalf("应用成功后待重载标记应清空,实际 %q", got)
	}
	if err := tun2.echo("round-6"); err != nil {
		t.Fatalf("配置回到当前生效的那份,不该重启数据面: %v", err)
	}
	if n := run.InboundCount(); n != 2 {
		t.Fatalf("修好后应仍是两条入站,实际 %d", n)
	}
}

// 不经 hub、直接打接口的几条拒绝路径:不是副机 403、令牌错 / 缺令牌 401、缺修订号 400、数据面未初始化 503。
// ---- 测试助手 ----

// freeTCPPort 找一个当前没人监听的端口给测试线路用。
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// socksPassword 从用户凭据里取出 mixed / socks 入站用的口令。
func socksPassword(t *testing.T, creds json.RawMessage) string {
	t.Helper()
	var m map[string]map[string]interface{}
	if err := json.Unmarshal(creds, &m); err != nil {
		t.Fatal(err)
	}
	p, _ := m["socks"]["password"].(string)
	if p == "" {
		t.Fatal("凭据里没有 socks 口令")
	}
	return p
}

// startEchoServer 起一个本机回显服务,返回端口;隧道穿过数据面连它。
func startEchoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// tunnel 一条经 mixed 入站 CONNECT 到回显服务的连接。
type tunnel struct {
	net.Conn
	r *bufio.Reader
}

// echo 发一行、等它原样回来;数据面被拆过(连接被关)时返回错误。
func (c *tunnel) echo(token string) error {
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(c, "%s\n", token); err != nil {
		return err
	}
	line, err := c.r.ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) != token {
		return fmt.Errorf("回显不匹配: %q", line)
	}
	return nil
}

func connectViaProxy(t *testing.T, proxyPort int, user, pass string, targetPort int) (*tunnel, int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 3*time.Second)
	if err != nil {
		t.Fatalf("连不上 mixed 入站 %d(数据面没在服务?): %v", proxyPort, err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	fmt.Fprintf(c, "CONNECT 127.0.0.1:%d HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nProxy-Authorization: Basic %s\r\n\r\n", targetPort, targetPort, auth)
	r := bufio.NewReader(c)
	// 只读状态行和头:CONNECT 成功的响应没有 body,之后这条连接就是隧道本身,不能去"读完 body"
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		t.Fatalf("读 CONNECT 响应失败: %v", err)
	}
	return &tunnel{Conn: c, r: r}, resp.StatusCode
}

// openTunnel 用正确凭据开一条隧道(CONNECT 必须是 200)。
func openTunnel(t *testing.T, proxyPort int, user, pass string, targetPort int) *tunnel {
	t.Helper()
	tun, code := connectViaProxy(t, proxyPort, user, pass, targetPort)
	if code != http.StatusOK {
		tun.Close()
		t.Fatalf("CONNECT 应 200,实际 %d", code)
	}
	return tun
}

// tunnelStatus 只看 CONNECT 的状态码(用来验证错口令被拒)。
func tunnelStatus(t *testing.T, proxyPort int, user, pass string, targetPort int) int {
	t.Helper()
	tun, code := connectViaProxy(t, proxyPort, user, pass, targetPort)
	tun.Close()
	return code
}

// agentReport 以主机身份拉一次副机报告。
func agentReport(t *testing.T, base, token string) hub.Report {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/app/api/agent/report", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Agent-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("拉报告应 200,实际 %d %s", resp.StatusCode, b)
	}
	var rep hub.Report
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&rep); err != nil {
		t.Fatalf("报告不是 JSON: %v\n%s", err, b)
	}
	return rep
}
