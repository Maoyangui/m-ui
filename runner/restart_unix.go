//go:build !windows

package runner

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/Maoyangui/m-ui/logger"
)

// restartProcess 用相同参数原地重新执行自身(不依赖 systemd 的 Restart 策略)。
func restartProcess() {
	// 用启动时的路径(systemd 的 ExecStart 是绝对路径)而不是 /proc/self/exe:
	// 一键更新把正在运行的文件改名成了 .prev,/proc/self/exe 会跟着指向旧程序,按它 exec 就又跑回旧版本。
	self := os.Args[0]
	if p, err := exec.LookPath(self); err == nil {
		self = p
	} else if e, err := os.Executable(); err == nil {
		self = e
	}
	if err := syscall.Exec(self, os.Args, os.Environ()); err != nil {
		logger.Error("重新执行失败,改为退出等待守护进程拉起: ", err)
		os.Exit(3)
	}
}
