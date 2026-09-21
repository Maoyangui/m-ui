package core

import (
	"encoding/json"
	"net"
	"os"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 共享凭据与多入站:同一个人的本名会话与 "名字#share" 会话各自精确匹配;一个人在两个入站各有一条会话时,
// 踢线要把两条都关掉。真实入站,M_UI_E2E=1 时才跑(见 usersess_e2e_test.go)。
func TestUserSessionsE2EShareAndMultiInbound(t *testing.T) {
	if os.Getenv("M_UI_E2E") != "1" {
		t.Skip("需要真实端口与 QUIC:M_UI_E2E=1 时才跑")
	}
	certPath, keyPath := e2eSelfSigned(t)
	echoAddr := e2eEchoServer(t)
	share := "alice" + model.ShareSuffix
	tlsIn := map[string]interface{}{"enabled": true, "server_name": "localhost", "certificate_path": certPath, "key_path": keyPath}
	tlsOut := map[string]interface{}{"enabled": true, "insecure": true, "server_name": "localhost"}
	h3 := func(m map[string]interface{}) map[string]interface{} {
		out := map[string]interface{}{}
		for k, v := range m {
			out[k] = v
		}
		out["alpn"] = []string{"h3"}
		return out
	}
	hy2Port, anyPort := e2eFreeUDP(t), e2eFreeTCP(t)
	users := []map[string]interface{}{{"name": "alice", "password": "pa"}, {"name": share, "password": "ps"}}
	serverCfg, _ := json.Marshal(map[string]interface{}{
		"log": map[string]interface{}{"level": "error"},
		"inbounds": []map[string]interface{}{
			{"type": "hysteria2", "tag": "hy2", "listen": "127.0.0.1", "listen_port": hy2Port, "users": users, "tls": h3(tlsIn)},
			{"type": "anytls", "tag": "anytls", "listen": "127.0.0.1", "listen_port": anyPort, "users": users, "tls": tlsIn},
		},
		"outbounds": []map[string]interface{}{{"type": "direct", "tag": "direct"}},
	})
	c := NewCore()
	if err := c.Start(serverCfg); err != nil {
		t.Fatalf("数据面起不来: %v", err)
	}
	defer c.Stop()

	hy2 := func(pw string) map[string]interface{} {
		return map[string]interface{}{"type": "hysteria2", "tag": "out", "server": "127.0.0.1", "server_port": hy2Port, "password": pw, "tls": h3(tlsOut)}
	}
	anytls := func(pw string) map[string]interface{} {
		return map[string]interface{}{"type": "anytls", "tag": "out", "server": "127.0.0.1", "server_port": anyPort, "password": pw, "tls": tlsOut}
	}
	dialer := func(proxy string) func() (net.Conn, error) {
		return func() (net.Conn, error) { return e2eSocksDial(proxy, echoAddr) }
	}
	aliceHy2 := dialer(e2eClientBox(t, hy2("pa")))
	shareHy2 := dialer(e2eClientBox(t, hy2("ps")))
	aliceAny := dialer(e2eClientBox(t, anytls("pa")))

	c1 := e2eMustEcho(t, aliceHy2, "本人经 hy2 应通")
	defer c1.Close()
	c2 := e2eMustEcho(t, shareHy2, "借用者经 hy2 应通")
	defer c2.Close()
	c3 := e2eMustEcho(t, aliceAny, "本人经 anytls 应通")
	defer c3.Close()

	// 收回共享:只关 "alice#share" 那条,本人的两条会话不动
	if n := c.GetInstance().CloseUserSessions([]string{share}); n != 1 {
		t.Fatalf("收回共享应只关借用者 1 条会话,实际 %d", n)
	}
	e2eMustFail(t, c2, "借用者的会话应被关")
	if err := e2eEcho(c1, "alice-hy2-still"); err != nil {
		t.Fatalf("本人经 hy2 的会话不该受影响: %v", err)
	}
	if err := e2eEcho(c3, "alice-anytls-still"); err != nil {
		t.Fatalf("本人经 anytls 的会话不该受影响: %v", err)
	}

	// 踢线(本人 + 共享):两个入站上本人的会话都关;借用者刚才已经没了
	if n := c.GetInstance().CloseUserSessions([]string{"alice", share}); n != 2 {
		t.Fatalf("踢线应关掉本人在两个入站上的 2 条会话,实际 %d", n)
	}
	e2eMustFail(t, c1, "踢线后本人经 hy2 的会话应被关")
	e2eMustFail(t, c3, "踢线后本人经 anytls 的会话应被关")
	// 凭据没变:三方都能重连
	c4 := e2eMustEcho(t, aliceHy2, "踢线后本人经 hy2 应能重连")
	defer c4.Close()
	c5 := e2eMustEcho(t, shareHy2, "收回共享只是关会话,凭据还在,借用者应能重连")
	defer c5.Close()
	c6 := e2eMustEcho(t, aliceAny, "踢线后本人经 anytls 应能重连")
	defer c6.Close()
}
