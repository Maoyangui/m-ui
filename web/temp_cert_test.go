package web

import (
	"crypto/tls"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// loopbackPort 本机回环上一个空闲的 TCP 端口。
func loopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// peerLeaf 连上本机这个端口做一次 TLS 握手(不校验),返回对方给的证书。
func peerLeaf(t *testing.T, port int) *tlsLeaf {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("端口 %d 应该是 HTTPS: %v", port, err)
	}
	defer conn.Close()
	c := conn.ConnectionState().PeerCertificates[0]
	return &tlsLeaf{selfSigned: c.Issuer.String() == c.Subject.String(), names: c.DNSNames}
}

type tlsLeaf struct {
	selfSigned bool
	names      []string
}

// 代理面板的证书文件不在(换机还原后最常见):以前 StartReseller 报错,代理面板直接不起;
// 现在和主面板一样先用临时自签证书顶着,照样走 HTTPS。
func TestResellerPanelKeepsHTTPSWhenCertMissing(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(run.DB()) })
	port := loopbackPort(t)
	gone := filepath.Join(t.TempDir(), "live", "a.example.com")
	for k, v := range map[string]string{
		"resellerListen": "127.0.0.1", "resellerPort": strconv.Itoa(port),
		"resellerCertFile": filepath.Join(gone, "fullchain.pem"), "resellerKeyFile": filepath.Join(gone, "privkey.pem"),
	} {
		run.DB().Create(&model.Setting{Key: k, Value: v})
	}
	s := NewServer(run)
	if err := s.StartReseller(); err != nil {
		t.Fatalf("代理面板证书读不出来,代理面板就起不来: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			s.StopReseller()
		}
	})
	if leaf := peerLeaf(t, port); !leaf.selfSigned {
		t.Fatal("顶上的应是临时自签证书")
	}
	if alerts := run.CertAlerts(); len(alerts) != 1 || alerts[0].Fallback != "reseller" || alerts[0].Trouble != "missing" {
		t.Fatalf("页面顶部要提示代理面板在用临时证书: %+v", alerts)
	}
	s.StopReseller()
	stopped = true
	if alerts := run.CertAlerts(); len(alerts) != 0 {
		t.Fatalf("代理面板停了就不再提示: %+v", alerts)
	}
}
