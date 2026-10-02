package runner

import (
	"fmt"
	"net"
	"testing"
)

// freePort 找一个 TCP、UDP 都能绑的空闲端口给测试线路用:shadowsocks 这类入站两样都要监听。
// Windows 上 Hyper-V / WSL 会动态保留成段的 UDP 端口,只试 TCP 常挑到绑不上 UDP 的,测试就偶发失败。
func freePort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if pc, err := net.ListenPacket("udp", fmt.Sprintf("0.0.0.0:%d", port)); err == nil {
			pc.Close()
			return port
		}
	}
	t.Fatal("找不到 TCP、UDP 都空闲的端口")
	return 0
}
