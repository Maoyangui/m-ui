//go:build !race

// 起的是真实的内嵌数据面,-race 下跳过的原因见 agent_apply_e2e_test.go 开头。

package web

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/runner"
)

// 副机管理连接失联期间,管理员先改了上游参数、又停用了一个用户(上游名与线路都没变);恢复后一份快照
// 同时带着两项变化推过来。副机按"上游变了"走热更新,也必须把用户表换掉:被停用的用户不能再用旧凭据
// 建新连接,确认成功之前就得生效(主机见到确认就不会再推同一修订)。
func TestAgentApplyUpstreamAndUserChangeInOneSnapshot(t *testing.T) {
	masterDB := openAgentTestDB(t, "master.db")
	masterDB.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	masterDB.Create(&model.Setting{Key: "allowPrivate", Value: "true"})
	up := model.Upstream{Name: "relay", Type: "socks", Options: json.RawMessage(`{"server":"127.0.0.1","server_port":1}`)}
	masterDB.Create(&up)
	port := freeTCPPort(t)
	line := model.Line{Name: "via-relay", Protocol: "mixed", Port: port, UpstreamId: up.Id, Enabled: true, Options: json.RawMessage(`{}`)}
	masterDB.Create(&line)
	alice := model.User{Name: "alice", Enabled: true, Credentials: generateCredentials("alice")}
	masterDB.Create(&alice)
	masterDB.Create(&model.UserLine{UserId: alice.Id, LineId: line.Id})
	alicePass := socksPassword(t, alice.Credentials)
	masterSetting := func(k string) string {
		var v string
		masterDB.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
		return v
	}

	const token = "e2e-agent-token-upstream-0123"
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
	h := hub.New(hub.Deps{DB: masterDB, Setting: masterSetting, IsNode: func() bool { return false }, Version: Version})
	nodeRow := model.Node{Name: "副机", ApiUrl: agentSrv.URL + "/app/", Token: token, Enabled: true, Sort: 2}
	masterDB.Create(&nodeRow)

	if err := h.PushNow(nodeRow); err != nil {
		t.Fatalf("首次推送应成功: %v", err)
	}
	echo := startEchoServer(t)
	if code := connectCode(t, port, "alice", alicePass, echo); code == http.StatusProxyAuthRequired {
		t.Fatal("前提:启用的 alice 应能通过入站认证")
	}

	// 两项变化攒在一份快照里:上游换了端口(名字不变),alice 被停用
	masterDB.Model(&model.Upstream{}).Where("id = ?", up.Id).Update("options", json.RawMessage(`{"server":"127.0.0.1","server_port":2}`))
	masterDB.Model(&model.User{}).Where("id = ?", alice.Id).Update("enabled", false)
	if err := h.PushNow(nodeRow); err != nil {
		t.Fatalf("推送应成功: %v", err)
	}
	if rep := agentReport(t, agentSrv.URL, token); rep.ReloadPending {
		t.Fatalf("确认成功后不该有待重载: %+v", rep)
	}
	if st := run.ReloadStatus(); !st.OK {
		t.Fatalf("应用应成功: %+v", st)
	}
	if code := connectCode(t, port, "alice", alicePass, echo); code != http.StatusProxyAuthRequired {
		t.Fatalf("已停用的 alice 用旧凭据建新连接应被拒(407),实际 %d", code)
	}
}

// connectCode 发一次 CONNECT,只要状态码;连接被重置之类(mixed 入站换用户表时会拆了重建)稍等重试,
// 最终拿不到响应才判失败。
func connectCode(t *testing.T, proxyPort int, user, pass string, targetPort int) int {
	t.Helper()
	var last error
	for i := 0; i < 10; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), 3*time.Second)
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
			fmt.Fprintf(c, "CONNECT 127.0.0.1:%d HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nProxy-Authorization: Basic %s\r\n\r\n", targetPort, targetPort, auth)
			resp, rerr := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
			c.Close()
			if rerr == nil {
				return resp.StatusCode
			}
			err = rerr
		}
		last = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("CONNECT 一直拿不到响应: %v", last)
	return 0
}
