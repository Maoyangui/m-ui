package ops

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// 统计磁盘占用时最多看这么多个文件(/var/log 里堆了几十万个小文件的机器也不能把一次统计拖成几十秒)。
const diskWalkLimit = 200000

// logDirs / cacheDirs 磁盘占用里单独列出来的大头:系统日志、软件包下载缓存(清理磁盘清的就是它们)。
var (
	logDirs   = []string{"/var/log"}
	cacheDirs = []string{"/var/cache/apt/archives", "/var/cache/dnf", "/var/cache/yum"}
)

// DiskStatOf 统计 dataDir(m-ui 数据目录)所在那块盘的占用,外加 swap 总量、系统日志与软件包缓存各占多少。
// 读不到的项留 0。要遍历目录,调用方自己缓存(见 runner.DiskStat)。
func DiskStatOf(dataDir string) model.DiskStat {
	st := model.DiskStat{Path: dataDir, At: time.Now().Unix()}
	if u, err := disk.Usage(dataDir); err == nil {
		st.Total, st.Used, st.Free = int64(u.Total), int64(u.Used), int64(u.Free)
	}
	if sw, err := mem.SwapMemory(); err == nil {
		st.Swap = int64(sw.Total)
	}
	st.OpsAgent = OpsAgentActive()
	budget := diskWalkLimit
	for _, d := range logDirs {
		st.Logs += dirSize(d, &budget)
	}
	for _, d := range cacheDirs {
		st.Cache += dirSize(d, &budget)
	}
	return st
}

// dirSize 目录下普通文件的大小合计(不跟符号链接、不跨进别的挂载点也无所谓:这几个目录本来就在根盘上)。
// budget 是还能看几个文件,用完就停(结果偏小,但不会卡住)。
func dirSize(root string, budget *int) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if *budget <= 0 {
			return fs.SkipAll
		}
		*budget--
		if d.Type()&(fs.ModeSymlink|fs.ModeDevice|fs.ModeNamedPipe|fs.ModeSocket) != 0 || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// fileSize 文件大小(没有为 0)。
func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
		return fi.Size()
	}
	return 0
}
