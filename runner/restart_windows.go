//go:build windows

package runner

import (
	"os"
	"os/exec"

	"github.com/fangjunsheng555/m-ui/logger"
)

// restartProcess Windows 无 exec(2):启动一个新进程后退出(开发测试用)。
func restartProcess() {
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	cmd := exec.Command(self, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		logger.Error("启动新进程失败: ", err)
	}
	os.Exit(0)
}
