package ops

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeBin 在临时目录里放几个假命令(只把参数记进 calls 文件),返回放命令的目录和记录文件。
func fakeBin(t *testing.T, scripts map[string]string) (string, string) {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	for name, body := range scripts {
		sh := "#!/bin/sh\necho \"" + name + " $*\" >> " + calls + "\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(sh), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin, calls
}

func runScript(t *testing.T, script, bin string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "LANG=C.UTF-8")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("脚本出错: %v\n%s", err, out)
	}
	return string(out)
}

func mkFile(t *testing.T, path string, size int64, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		f.WriteString("first line\n")
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		f.Seek(size-int64(len("last line\n")), 0)
		f.WriteString("last line\n")
	}
	f.Close()
	if age > 0 {
		at := time.Now().Add(-age)
		os.Chtimes(path, at, at)
	}
}

// 清理磁盘在临时目录里真跑一遍:超过 100MB 的文本日志原地截到最后 5MB(文件还是那个、尾巴留着)、小的不动;
// 一天前轮转下来的旧日志删掉、刚轮转的和 journal 目录不碰;装好每小时检查 syslog 大小的定时器;
// 勾了才停用谷歌云 Ops Agent。
func TestDiskCleanScript(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("要 bash、find -print0、stat -c")
	}
	logs, sbin, units := t.TempDir(), t.TempDir(), t.TempDir()
	rot := filepath.Join(t.TempDir(), "rsyslog")
	os.WriteFile(rot, []byte("/var/log/syslog {}\n"), 0o644)
	old := cleanDirs
	cleanDirs.Log, cleanDirs.Sbin, cleanDirs.Units, cleanDirs.Rotate = logs, sbin, units, []string{filepath.Join(t.TempDir(), "missing"), rot}
	t.Cleanup(func() { cleanDirs = old })

	mkFile(t, filepath.Join(logs, "syslog"), 120<<20, 0)
	mkFile(t, filepath.Join(logs, "messages"), 10<<20, 0)
	mkFile(t, filepath.Join(logs, "syslog.2.gz"), 1<<20, 48*time.Hour)
	mkFile(t, filepath.Join(logs, "apt", "history.log.1"), 1<<20, 72*time.Hour)
	mkFile(t, filepath.Join(logs, "syslog.1"), 1<<20, time.Hour) // 刚轮转的:留着
	mkFile(t, filepath.Join(logs, "journal", "x", "system@1.journal.1"), 1<<20, 96*time.Hour)
	before, err := os.Stat(filepath.Join(logs, "syslog"))
	if err != nil {
		t.Fatal(err)
	}
	bin, calls := fakeBin(t, map[string]string{"apt-get": "", "journalctl": "", "systemctl": "", "logrotate": ""})

	for _, agent := range []bool{false, true} {
		s, err := Script("disk-clean", Params{JournalMB: 300, OpsAgent: agent})
		if err != nil {
			t.Fatal(err)
		}
		out := runScript(t, s, bin)
		got, _ := os.ReadFile(calls)
		c := string(got)
		if !agent {
			fi, _ := os.Stat(filepath.Join(logs, "syslog"))
			if fi.Size() != 5<<20 || !os.SameFile(fi, before) {
				t.Fatalf("syslog 原地截到 5MB(还是那个文件,rsyslog 不用重启): %d", fi.Size())
			}
			tail, _ := os.ReadFile(filepath.Join(logs, "syslog"))
			if !strings.HasSuffix(string(tail), "last line\n") {
				t.Fatal("留的是最后 5MB")
			}
			if fi, _ := os.Stat(filepath.Join(logs, "messages")); fi.Size() != 10<<20 {
				t.Fatal("没到 100MB 的不动")
			}
			for _, gone := range []string{"syslog.2.gz", "apt/history.log.1"} {
				if _, err := os.Stat(filepath.Join(logs, gone)); !os.IsNotExist(err) {
					t.Fatalf("一天前轮转下来的 %s 要删", gone)
				}
			}
			for _, kept := range []string{"syslog.1", "journal/x/system@1.journal.1"} {
				if _, err := os.Stat(filepath.Join(logs, kept)); err != nil {
					t.Fatalf("%s 要留着: %v", kept, err)
				}
			}
			if !strings.Contains(c, "apt-get clean") || !strings.Contains(c, "journalctl --vacuum-size=300M") || !strings.Contains(c, "systemctl enable --now m-ui-syslog-cap.timer") {
				t.Fatalf("清缓存、回收 journald、装定时器:\n%s", c)
			}
			if strings.Contains(c, "google-cloud-ops-agent") {
				t.Fatal("没勾不能停 Ops Agent")
			}
			for _, must := range []string{"截断 " + filepath.Join(logs, "syslog"), "删掉轮转下来的旧日志 2 个", "m-ui 的数据目录和备份没有动"} {
				if !strings.Contains(out, must) {
					t.Fatalf("输出里要有 %q:\n%s", must, out)
				}
			}
			unit, _ := os.ReadFile(filepath.Join(units, "m-ui-syslog-cap.service"))
			if !strings.Contains(string(unit), "ExecStart="+filepath.Join(sbin, "m-ui-syslog-cap")) {
				t.Fatalf("服务单元指向写好的脚本:\n%s", unit)
			}
			// 写好的小脚本:syslog 超过 200MB 就按系统的规则轮转,没超过不动
			capScript := filepath.Join(sbin, "m-ui-syslog-cap")
			if b, err := exec.Command("sh", "-n", capScript).CombinedOutput(); err != nil {
				t.Fatalf("定时器脚本语法: %v %s", err, b)
			}
			os.Truncate(calls, 0)
			runScript(t, "sh "+capScript, bin)
			if got, _ := os.ReadFile(calls); strings.Contains(string(got), "logrotate") {
				t.Fatal("没超过 200MB 不轮转")
			}
			mkFile(t, filepath.Join(logs, "syslog"), 210<<20, 0)
			runScript(t, "sh "+capScript, bin)
			if got, _ := os.ReadFile(calls); !strings.Contains(string(got), "logrotate -f "+rot) {
				t.Fatalf("超过 200MB 按系统的规则立刻轮转: %s", got)
			}
			os.Truncate(calls, 0)
			continue
		}
		for _, u := range opsAgentUnits {
			if !strings.Contains(c, "systemctl disable --now "+u) || !strings.Contains(c, "systemctl mask "+u) {
				t.Fatalf("勾了要停用并屏蔽 %s:\n%s", u, c)
			}
		}
	}
}

// swap 按磁盘余量封顶:给系统留 2G、不超过盘的四分之一;不够 512MB 不建;够就照要的建。
func TestSwapCappedByDisk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("要 bash")
	}
	for _, c := range []struct {
		name               string
		availMB, totalMB   int
		gb                 int
		want, wantFileSize string
	}{
		{"大盘照要的建", 50000, 100000, 2, "已创建 2048MB swap", "2048M"},
		{"谷歌云 8G 小盘要 4G:减到余量", 3000, 8800, 4, "减到 952MB", "952M"},
		{"四分之一封顶", 9000, 10000, 4, "减到 2500MB", "2500M"},
		{"只剩 1G:不建", 1000, 8800, 2, "不够建 swap,跳过", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			old := swapPaths
			swapPaths.File, swapPaths.Fstab = filepath.Join(dir, "swapfile"), filepath.Join(dir, "fstab")
			t.Cleanup(func() { swapPaths = old })
			os.WriteFile(swapPaths.Fstab, nil, 0o644)
			df := "echo 'Filesystem 1048576-blocks Used Available Capacity Mounted'; echo \"/dev/root " + strconv.Itoa(c.totalMB) + " 1 " + strconv.Itoa(c.availMB) + " 50% /\""
			bin, calls := fakeBin(t, map[string]string{"df": df, "swapon": "", "mkswap": "", "fallocate": ""})
			s, err := Script("swap", Params{SwapGB: c.gb})
			if err != nil {
				t.Fatal(err)
			}
			out := runScript(t, s, bin)
			if !strings.Contains(out, c.want) {
				t.Fatalf("输出要有 %q:\n%s", c.want, out)
			}
			got, _ := os.ReadFile(calls)
			fstab, _ := os.ReadFile(swapPaths.Fstab)
			if c.wantFileSize == "" {
				if strings.Contains(string(got), "fallocate") || len(fstab) != 0 {
					t.Fatalf("不够就不建:\n%s", got)
				}
				return
			}
			if !strings.Contains(string(got), "fallocate -l "+c.wantFileSize+" "+swapPaths.File) || !strings.Contains(string(fstab), swapPaths.File+" none swap") {
				t.Fatalf("按封顶后的大小建、写进 fstab:\n%s\n%s", got, fstab)
			}
		})
	}
}
