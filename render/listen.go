package render

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
)

// listenAddr 入站的监听地址。"::" 是双栈:Go 对通配地址用 AF_INET6 并显式设 IPV6_V6ONLY=0,IPv4 也收,
// 不受 net.ipv6.bindv6only 影响,线路选 IPv4 还是 IPv6 都不用改监听。
// 内核整个关了 IPv6(启动参数 ipv6.disable=1、或编译时没带 IPv6)时 AF_INET6 的套接字建不起来,"::" 会让所有入站
// 一起起不来(不只是 IPv6 线路),只能改听 "0.0.0.0"。判据是 /proc/net/if_inet6 不存在 —— 只是用 sysctl 关掉
// 各网卡的 IPv6(disable_ipv6=1)时这个文件还在,"::" 也照样能听。常规机器上恒为 "::",渲染结果逐字节不变,
// 不会因此让副机觉得配置变了去重载。
func listenAddr() string {
	if noIPv6Stack() {
		return "0.0.0.0"
	}
	return "::"
}

// noIPv6Stack 只在 Linux 上判断;读不了(权限之类)按有 IPv6 处理,不改常规机器的配置。测试可替换。
var noIPv6Stack = func() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat("/proc/net/if_inet6")
	return errors.Is(err, fs.ErrNotExist)
}
