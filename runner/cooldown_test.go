package runner

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
)

// 副机重启风暴的回归测试:同一份起不来的配置连推两次,第二次**不能**再拆正在服务的数据面。
//
// 0.6.10 在生产上的病:新线路端口被占 → 副机记待重载回 503 → 主机每 5 秒重推 → 副机每次都
// Stop 旧数据面、Start 失败、再起旧配置,这台机上所有用户每 5 秒断一次线。0.4.16 修过的问题换了形式回来。
// 这条测试直接钉住 reloadAllLockedWithForce 的冷却分支:Box 指针不变 = 没有 Stop 过。
func TestFailedConfigCooldownKeepsDataPlane(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	r := &Runner{db: db, core: core.NewCore(), dbPath: filepath.Join(dir, "x.db")}
	defer r.core.Stop()

	goodPort := freePort(t)
	cfg := func(users string, extra string) []byte {
		return []byte(fmt.Sprintf(`{"log":{"disabled":true},"inbounds":[{"type":"mixed","tag":"in","listen":"127.0.0.1","listen_port":%d,"users":[%s]}%s],"outbounds":[{"type":"direct","tag":"direct"}]}`, goodPort, users, extra))
	}
	good := cfg(`{"username":"a","password":"1"}`, "")
	if err := r.reloadAllLocked(good); err != nil {
		t.Fatalf("初始配置应能启动: %v", err)
	}
	box0 := r.core.GetInstance()

	// 一份能过校验、启动必然失败的配置:多一条入站,监听本测试占住的端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	badExtra := fmt.Sprintf(`,{"type":"mixed","tag":"in2","listen":"127.0.0.1","listen_port":%d}`, busy)
	bad := cfg(`{"username":"a","password":"1"}`, badExtra)

	// 第一次:真重启、失败、回滚 —— 这一次断线是不可避免的
	err = r.reloadAllLocked(bad)
	if err == nil || !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("第一次应报回滚: %v", err)
	}
	if !r.core.IsRunning() {
		t.Fatal("回滚后数据面应在运行")
	}
	box1 := r.core.GetInstance()
	if box1 == box0 {
		t.Fatal("前提:第一次失败是真重启 + 回滚,Box 应换了一个")
	}
	if r.lastFailedRaw == nil {
		t.Fatal("失败的配置应被记住")
	}

	// 第二次同一份:冷却期内不许再 Stop —— Box 指针必须原样
	err = r.reloadAllLocked(bad)
	if err == nil || !strings.Contains(err.Error(), "冷却期") {
		t.Fatalf("冷却期内应报冷却而不是再回滚一次: %v", err)
	}
	if r.core.GetInstance() != box1 {
		t.Fatal("冷却期内拆了正在服务的数据面:这就是每 5 秒断线一次的风暴")
	}

	// 只有用户表不同的变体(管理员没改线路、只是停用 / 换了用户):同样不拆数据面,
	// 但用户表要热更新到运行中的旧配置上 —— 停用、换凭据在这台机上不能被冻结
	bad2 := cfg(`{"username":"b","password":"2"}`, badExtra)
	err = r.reloadAllLocked(bad2)
	if err == nil || !strings.Contains(err.Error(), "冷却期") {
		t.Fatalf("只换用户表的变体也该按同一份失败配置处理: %v", err)
	}
	if r.core.GetInstance() != box1 {
		t.Fatal("只换用户表的变体不该拆数据面")
	}
	if code := proxyAuthStatus(t, goodPort, "a", "1"); code == 0 {
		t.Fatal("探测连不上 mixed 入站:数据面没在服务?")
	}
	if code := proxyAuthStatus(t, goodPort, "a", "1"); code != 407 {
		t.Fatalf("旧用户 a 的凭据应已失效(期望 407),实际 %d", code)
	}
	if code := proxyAuthStatus(t, goodPort, "b", "2"); code == 407 {
		t.Fatal("新用户 b 的凭据应已热更新到运行中的旧配置上,却被 407 拒绝")
	}

	// 强制重载绕过冷却:真重启、再回滚
	err = r.reloadAllLockedForce(bad)
	if err == nil || !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("强制重载应真的再试一次并回滚: %v", err)
	}
	if r.core.GetInstance() == box1 {
		t.Fatal("强制重载应绕过冷却期")
	}
	box2 := r.core.GetInstance()

	// 冷却期过了:允许再试一次
	r.lastFailedAt = time.Now().Add(-failedConfigRetryEvery - time.Second)
	err = r.reloadAllLocked(bad)
	if err == nil || !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("冷却期过后应真的再试一次: %v", err)
	}
	if r.core.GetInstance() == box2 {
		t.Fatal("冷却期过后应真重启")
	}

	// 管理员修好了(端口空出来)。注意上一步失败又把冷却期重新计时了:只放开端口、不改配置的话,
	// 这台机最多要等一个冷却期才会再试(改了线路则立刻)。这里把冷却期视作已过,验证"修好就能成功、失败记录清掉"。
	ln.Close()
	r.lastFailedAt = time.Now().Add(-failedConfigRetryEvery - time.Second)
	if err := r.reloadAllLocked(bad); err != nil {
		t.Fatalf("端口空出来后应能成功: %v", err)
	}
	if r.lastFailedRaw != nil {
		t.Fatal("成功启动后应清掉失败记录")
	}
}

// proxyAuthStatus 向 mixed 入站发一个带 Basic 认证的 CONNECT,返回状态码(0 = 连不上 / 没读到状态行)。
func proxyAuthStatus(t *testing.T, port int, user, pass string) int {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return 0
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	fmt.Fprintf(c, "CONNECT 127.0.0.1:9 HTTP/1.1\r\nHost: 127.0.0.1:9\r\nProxy-Authorization: Basic %s\r\n\r\n", auth)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return 0
	}
	var code int
	fmt.Sscanf(line, "HTTP/1.1 %d", &code)
	return code
}
