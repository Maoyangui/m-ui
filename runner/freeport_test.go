package runner

import (
	"net"
	"testing"
)

// freePort 找一个当前没人监听的 TCP 端口给测试线路用。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
