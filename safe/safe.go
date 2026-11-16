// Package safe 是起后台协程的地方。
//
// 面板和数据面(sing-box)跑在同一个进程里:任何一个协程里没接住的 panic 都会把整个进程带走 —— 全部用户断线,
// 靠 systemd 两秒后拉起来。HTTP 处理函数有 net/http 自己兜着,常驻循环有 runner 的 keepRunning 兜着;
// 其余「顺手起一个协程去做」的(并发探测几台服务器、保存后重载数据面、发通知……)一律经这里起:
// panic 只记日志(带调用栈),结束的只是这一个协程。
//
// 兜住不等于没事:日志里出现「后台任务 … 异常」就是一个要修的 bug。兜住只是不让一个 bug 变成全站断线。
package safe

import (
	"runtime/debug"

	"github.com/Maoyangui/m-ui/logger"
)

// Do 在当前协程里跑 fn,兜住 panic。返回 fn 是不是正常跑完的(panic 了为 false)。
// 定时循环用它包住每一轮:一轮出错不影响下一轮。
func Do(name string, fn func()) (ok bool) {
	defer func() {
		if v := recover(); v != nil {
			ok = false
			logger.Warning("后台任务 ", name, " 异常(已兜住,进程不受影响): ", v, " | ", string(debug.Stack()))
		}
	}()
	fn()
	return true
}

// Go 起一个后台协程跑 fn。
func Go(name string, fn func()) { go Do(name, fn) }

// Go1 / Go2 / Go3 同 Go,给带参数的写法用:参数在调用这一刻就取值(和 go f(a) 一样),不是等协程跑起来才读。
func Go1[A any](name string, a A, fn func(A)) { go Do(name, func() { fn(a) }) }

func Go2[A, B any](name string, a A, b B, fn func(A, B)) { go Do(name, func() { fn(a, b) }) }

func Go3[A, B, C any](name string, a A, b B, c C, fn func(A, B, C)) {
	go Do(name, func() { fn(a, b, c) })
}
