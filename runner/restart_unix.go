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
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	if p, err := exec.LookPath(self); err == nil {
		self = p
	}
	if err := syscall.Exec(self, os.Args, os.Environ()); err != nil {
		logger.Error("重新执行失败,改为退出等待守护进程拉起: ", err)
		os.Exit(3)
	}
}
