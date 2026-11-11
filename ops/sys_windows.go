//go:build windows

package ops

// DiskUsage 见 sys_unix.go;Windows 上不统计。
func DiskUsage(string) (int64, int64) { return 0, 0 }
func diskUsage(string) (int64, int64) { return 0, 0 }
func nofileLimit() uint64             { return 0 }
