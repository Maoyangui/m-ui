package runner

import (
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"testing"
)

// freePort 找一个 TCP、UDP 都能绑的空闲端口给测试线路用:shadowsocks 这类入站两样都要监听。
//
// 端口探到空着就放开了,数据面要过一会儿(建库、渲染、起 sing-box)才去监听,重建入站、回滚时还会放开再重新监听。
// 这些空当里,临时端口段(Linux 的 ip_local_port_range)里的端口随时会被本机任何进程的出站连接拿去当本地端口 ——
// CI 上同时跑着别的包的测试,2026-11-18 就撞过:日志「端口 N 被本机别的程序占着」,ss 查不到谁在监听,拨过去
// connection refused,占着它的是一条出站连接。所以 Linux 上从临时端口段之外挑:内核不会把那里的端口自动分给
// 任何连接(出站连接、绑 0 端口都只在临时端口段里分),只有明着绑这个端口号才会撞上。
// 同一个测试进程里发过的端口不再发第二次:一个测试要好几个端口时不会挑到同一个。
//
// 读不到临时端口段(不是 Linux)时照旧向系统要临时端口:先向 UDP 要再试 TCP —— Windows 上 Hyper-V / WSL 会动态保留
// 成段的 UDP 端口,而 TCP 的临时端口是挨着分的,反过来从 TCP 要的话几十次都落在同一段保留里。
func freePort(t *testing.T) int {
	t.Helper()
	from, to := testPortRange()
	handedOutMu.Lock()
	defer handedOutMu.Unlock()
	for i := 0; i < 200; i++ {
		var port int
		if to > from {
			port = from + rand.IntN(to-from)
			if handedOut[port] || !bindable(port) {
				continue
			}
		} else {
			pc, err := net.ListenPacket("udp", "0.0.0.0:0")
			if err != nil {
				t.Fatal(err)
			}
			port = pc.LocalAddr().(*net.UDPAddr).Port
			pc.Close()
			if handedOut[port] {
				continue
			}
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				continue
			}
			ln.Close()
		}
		handedOut[port] = true
		return port
	}
	t.Fatal("找不到 TCP、UDP 都空闲的端口")
	return 0
}

var (
	handedOutMu sync.Mutex
	handedOut   = map[int]bool{} // 这个测试进程里 freePort 已经发出去的端口
)

// testPortRange 临时端口段之外、给测试挑端口的那一段 [from, to)。优先用临时端口段下面那段(1024 以下要特权),
// 不够再用上面那段;临时端口段读不到、或者两边剩下的都太少,返回 0, 0(freePort 退回向系统要临时端口)。
var testPortRange = sync.OnceValues(func() (from, to int) {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return 0, 0
	}
	var lo, hi int
	if n, _ := fmt.Sscan(string(b), &lo, &hi); n != 2 || lo < 1 || hi < lo || hi > 65535 {
		return 0, 0
	}
	const enough = 1000
	switch {
	case lo-1024 >= enough:
		return 1024, lo
	case 65535-hi >= enough:
		return hi + 1, 65536
	}
	return 0, 0
})

// bindable 这个端口在所有地址上 TCP、UDP 都能监听(线路入站可能监听 0.0.0.0 / ::)。
func bindable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	pc.Close()
	return true
}
