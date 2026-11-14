//go:build !windows

package ops

import "syscall"

// DiskUsage 某个目录所在文件系统的总量与可用空间(字节);取不到时为 0, 0。概况的「本机负载」卡用。
func DiskUsage(path string) (total, free int64) { return diskUsage(path) }

func diskUsage(path string) (total, free int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	return int64(st.Blocks) * int64(st.Bsize), int64(st.Bavail) * int64(st.Bsize)
}

func nofileLimit() uint64 {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return 0
	}
	return uint64(rl.Cur)
}
