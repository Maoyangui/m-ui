package web

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net"
	"os"
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

// peerFP 连上本机这个端口,返回对方证书的 SHA-256 指纹(主机钉副机证书用的就是它)。
func peerFP(t *testing.T, port int) string {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("端口 %d 应该是 HTTPS: %v", port, err)
	}
	defer conn.Close()
	sum := sha256.Sum256(conn.ConnectionState().PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}

// 面板、代理面板顶着的临时证书存在数据目录里,重启沿用同一张:副机面板顶着临时证书时,主机钉住的指纹以前每次重启都对不上,
// 照提示「重置指纹」后副机一重启又失联。它放在 cert/temp/ 里,重启时不会被当成证书收进证书库。
func TestTempCertSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "m.db")
	port := loopbackPort(t)
	gone := filepath.Join(t.TempDir(), "live", "a.example.com")
	start := func() (*runner.Runner, *Server) {
		t.Helper()
		run, err := runner.New(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		s := NewServer(run)
		if err := s.StartReseller(); err != nil {
			t.Fatal(err)
		}
		return run, s
	}
	run, err := runner.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"resellerListen": "127.0.0.1", "resellerPort": strconv.Itoa(port),
		"resellerCertFile": filepath.Join(gone, "fullchain.pem"), "resellerKeyFile": filepath.Join(gone, "privkey.pem"),
	} {
		run.DB().Create(&model.Setting{Key: k, Value: v})
	}
	database.Close(run.DB())

	run, s := start()
	first := peerFP(t, port)
	s.StopReseller()
	database.Close(run.DB())

	run, s = start() // 重启
	t.Cleanup(func() {
		s.StopReseller()
		database.Close(run.DB())
	})
	if again := peerFP(t, port); again != first {
		t.Fatal("重启后临时证书换了,主机钉住的指纹会对不上")
	}
	if _, err := os.Stat(filepath.Join(run.DataDir(), "cert", "temp", "reseller.crt")); err != nil {
		t.Fatalf("临时证书应存在数据目录的 cert/temp/ 里: %v", err)
	}
	if certs := run.Certs(); len(certs) != 0 {
		t.Fatalf("临时证书不该被收进证书库: %+v", certs)
	}
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
