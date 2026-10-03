package runner

import (
	"fmt"
	"net"
	"testing"
)

// freePort 找一个 TCP、UDP 都能绑的空闲端口给测试线路用:shadowsocks 这类入站两样都要监听。
// 先向 UDP 要端口再试 TCP:Windows 上 Hyper-V / WSL 会动态保留成段的 UDP 端口,而 TCP 的临时端口是挨着分的,
// 反过来从 TCP 要的话几十次都落在同一段保留里。
func freePort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		pc, err := net.ListenPacket("udp", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		port := pc.LocalAddr().(*net.UDPAddr).Port
		pc.Close()
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			ln.Close()
			return port
		}
	}
	t.Fatal("找不到 TCP、UDP 都空闲的端口")
	return 0
}
