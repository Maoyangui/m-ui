package sub

import (
	"crypto/tls"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// localPort 本机回环上一个空闲的 TCP 端口。
func localPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// 换机还原后订阅证书的文件不在(或私钥配不上):以前 Start 直接报错,runner.Run 跟着返回,整个进程退出、
// systemd 每 2 秒拉一次,面板也再打不开。现在先用临时自签证书顶着,照样走 HTTPS(订阅地址是 https://,降成 http 客户端全断)。
func TestStartKeepsHTTPSWhenCertMissing(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	port := localPort(t)
	gone := filepath.Join(t.TempDir(), "live", "a.example.com")
	for k, v := range map[string]string{
		"subListen": "127.0.0.1", "subPort": strconv.Itoa(port), "webDomain": "a.example.com",
		"subCertFile": filepath.Join(gone, "fullchain.pem"), "subKeyFile": filepath.Join(gone, "privkey.pem"),
	} {
		db.Create(&model.Setting{Key: k, Value: v})
	}
	s := NewServer(db)
	s.TLS = &certutil.Watch{}
	if err := s.Start(); err != nil {
		t.Fatalf("订阅证书读不出来,订阅服务就起不来(整个进程跟着退出): %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			s.Stop()
		}
	})
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{InsecureSkipVerify: true, ServerName: "a.example.com"})
	if err != nil {
		t.Fatalf("订阅端口应该仍是 HTTPS: %v", err)
	}
	leaf := conn.ConnectionState().PeerCertificates[0]
	conn.Close()
	if leaf.Issuer.String() != leaf.Subject.String() || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "a.example.com" {
		t.Fatalf("顶上的应是写着面板域名的临时自签证书: %s %v", leaf.Subject, leaf.DNSNames)
	}
	if list := s.TLS.Temps(); len(list) != 1 || list[0].Service != "sub" || list[0].CertFile != filepath.Join(gone, "fullchain.pem") {
		t.Fatalf("订阅在用临时证书,要登记给页面提示: %+v", list)
	}
	s.Stop()
	stopped = true
	if list := s.TLS.Temps(); len(list) != 0 {
		t.Fatalf("停了就不再列: %+v", list)
	}
}
