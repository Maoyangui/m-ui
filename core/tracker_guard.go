package core

import (
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/Maoyangui/m-ui/logger"
)

// 记账、限速、设备数、在线统计,是挂在每一条代理连接上的附加逻辑,跑在 sing-box 处理连接的协程里。
// 那里没有人兜 panic:这些附加逻辑里出一个意外(空指针、查不到的键),倒下的是整个进程 —— 面板和全部用户的连接。
// 所以四个入口(ConnTracker / StatsTracker 的 RoutedConnection、RoutedPacketConnection)各自兜住:
// 出了意外,这一条连接原样放行(只是这一条不记账、不限速),记一条日志。兜住不等于没事,日志里出现它就是要修的 bug。

var trackerPanicAt atomic.Int64

// trackerRecovered 记一条日志。一分钟最多一条:真出了问题时每条新连接都会走到这里,不能让日志把磁盘刷爆。
func trackerRecovered(where string, v interface{}) {
	now := time.Now().Unix()
	last := trackerPanicAt.Load()
	if now-last < 60 || !trackerPanicAt.CompareAndSwap(last, now) {
		return
	}
	logger.Warning("数据面附加逻辑异常(", where, ";已兜住,连接照常放行): ", v, " | ", string(debug.Stack()))
}
