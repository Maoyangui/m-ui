//go:build windows

package ops

func diskUsage(string) (int64, int64) { return 0, 0 }
func nofileLimit() uint64             { return 0 }
