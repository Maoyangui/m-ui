package selfupdate

// 升级事务:先留旧程序,再换新程序;新版本起不来就自动换回旧程序,仍起不来就连升级前的数据库一起还原。
//
// 三条更新路径共用这一套语义(步骤、超时、文件名、状态文件都一样):
//   面板按钮  web/update_api.go   Stage → Swap → LaunchWatcher(守护跑在 systemd-run 的临时单元里,不随面板重启被杀)
//   命令行菜单 menu.go            Stage → Swap → 重启 → Watch(菜单进程本来就在服务之外,同步等结果)
//   安装脚本  deploy/install.sh   同样的步骤用 bash 写了一遍 —— 它不能依赖任何一个 m-ui 二进制是好的
//
// 健康 = 面板首页在本机回 2xx/3xx(端口、路径、是否 HTTPS 都按设置来)。判断顺序:
//   新版本健康            → 删掉 .prev,记录成功
//   新版本不健康          → 停服务、坏程序改名 .failed、.prev 换回去、起服务,再看
//   仍不健康且有升级前备份 → 用旧程序还原那份备份,再起服务
//     (database.Open 的 AutoMigrate 只增不减,但历史上有过删索引的迁移,旧版本对新库不一定放心,
//      所以升级前备份不是摆设;备份用的是面板自己的 backup.Create,带 WAL 检查点,不是 cp 数据库文件)
// 结果写进 <数据目录>/upgrade-status.json,面板读到"回滚过"就在页面顶部明说,管理员点"知道了"才清掉。

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/brand"
)

// Plan 是一次升级的全部事实:文件在哪、服务叫什么、健康怎么判、失败怎么退。
type Plan struct {
	Bin, Prev, Failed string // 正式程序、旧程序暂存、失败的新程序(留作诊断)
	From, To          string // 版本号,只用于记录
	URL               string // 面板本机地址,如 http://127.0.0.1:2053/app/
	Service           string // systemd 单元名,默认 m-ui
	OldPID            int    // 换程序前的服务进程号;>0 时先等它退出再开始判健康,免得把还在跑的旧进程当成新版本
	DBPath, Backup    string // 有备份时,程序回滚后仍不健康就还原它
	StatusPath        string // 结果文件
	Timeout           time.Duration
}

// Status 是升级的结果,也是 upgrade-status.json 的内容。
type Status struct {
	At         int64  `json:"at"`
	From       string `json:"from"`
	To         string `json:"to"`
	OK         bool   `json:"ok"`         // 新版本健康
	RolledBack bool   `json:"rolledBack"` // 换回了旧程序
	DBRestored bool   `json:"dbRestored"` // 还原了升级前备份
	Healthy    bool   `json:"healthy"`    // 最终面板是否健康
	Backup     string `json:"backup,omitempty"`
	Failed     string `json:"failed,omitempty"`
	Message    string `json:"message"`
}

// ErrRolledBack 表示新版本没起来、已经退回旧版本;调用方据此报告,而不是当成普通错误。
var ErrRolledBack = errors.New("新版本没有正常启动,已回滚")

const (
	DefaultService = "m-ui"
	DefaultTimeout = 90 * time.Second
	BackupPrefix   = "pre-upgrade-" // 升级前备份的文件名前缀,只保留最近 KeepBackups 份
	KeepBackups    = 2
)

func PrevPath(bin string) string   { return bin + ".prev" }
func FailedPath(bin string) string { return bin + ".failed" }

// StatusPath 是数据目录里的结果文件。
func StatusPath(dataDir string) string { return filepath.Join(dataDir, "upgrade-status.json") }

// BackupName 生成升级前备份的文件名:pre-upgrade-v0.4.8-20260905-120000.zip。
func BackupName(from string) string {
	return BackupPrefix + strings.TrimPrefix(from, "v") + "-" + time.Now().Format("20060102-150405") + ".zip"
}

// LocalURL 是面板在本机的地址,健康检查打它。
func LocalURL(tlsOn bool, port int, path string) string {
	scheme := "http"
	if tlsOn {
		scheme = "https"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, port, path)
}

// Stage 下载指定标签、校验 SHA256、解出二进制写到 <bin>.new 并确认它能执行;不动正式程序。
func Stage(ctx context.Context, tag, binPath string, logf func(string, ...interface{})) (string, error) {
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	asset := "m-ui-linux-" + runtime.GOARCH + ".tar.gz"
	base := "https://github.com/" + brand.RepoPath + "/releases/download/" + tag + "/"

	logf("下载 %s", base+asset)
	body, err := download(ctx, base+asset)
	if err != nil {
		return "", err
	}
	want, err := sha256Of(ctx, base+"SHA256SUMS", asset)
	if err != nil {
		return "", fmt.Errorf("取不到这个版本的 SHA256SUMS,无法校验下载内容,已中止: %w", err)
	}
	if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != want {
		return "", fmt.Errorf("校验失败:下载的文件与 Release 的 SHA256 不一致,已中止")
	}
	logf("SHA256 校验通过")

	bin, err := extractBinary(body)
	if err != nil {
		return "", err
	}
	tmp := binPath + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return "", err
	}
	if out, err := exec.Command(tmp, "version").CombinedOutput(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("新二进制无法运行: %v %s", err, out)
	}
	return tmp, nil
}

// Swap 把正式程序挪到 prev、新程序挪到正式位置;第二步失败会把旧程序放回去。
// 用 rename 而不是复制:正在运行的进程持有旧 inode,不受影响。
func Swap(binPath, newPath, prevPath string) error {
	os.Remove(prevPath)
	if err := os.Rename(binPath, prevPath); err != nil {
		if !os.IsNotExist(err) { // 首次安装没有旧程序,直接落新的
			return fmt.Errorf("暂存旧程序失败: %w", err)
		}
	}
	if err := os.Rename(newPath, binPath); err != nil {
		if _, e := os.Stat(prevPath); e == nil {
			os.Rename(prevPath, binPath)
		}
		os.Remove(newPath)
		return fmt.Errorf("替换 %s 失败: %w", binPath, err)
	}
	return nil
}

// Prune 只保留 dir 里前缀为 prefix 的最新 keep 个文件(按修改时间)。
func Prune(dir, prefix string, keep int) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type f struct {
		name string
		mod  time.Time
	}
	var files []f
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if info, err := e.Info(); err == nil {
			files = append(files, f{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for i := keep; i < len(files); i++ {
		os.Remove(filepath.Join(dir, files[i].name))
	}
}

// ServiceName 从 cgroup 里读出当前进程所属的 systemd 单元名(m-ui / m-ui-test…),读不到就是默认值。
func ServiceName() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return DefaultService
	}
	if m := regexp.MustCompile(`/([A-Za-z0-9@._-]+)\.service`).FindStringSubmatch(string(b)); m != nil {
		return m[1]
	}
	return DefaultService
}

// MainPID 读 systemd 记录的服务主进程号。
func MainPID(service string) int {
	out, err := exec.Command("systemctl", "show", "-p", "MainPID", "--value", service).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// LaunchWatcher 用 systemd-run 起一个临时单元跑 `m-ui upgrade-watch`,它在服务的 cgroup 之外,
// 面板重启时不会被一起杀掉。没有 systemd-run 时返回错误,调用方明说"这次没有自动回滚"。
func LaunchWatcher(p Plan) error {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return fmt.Errorf("没有 systemd-run,无法在后台守护这次升级")
	}
	args := []string{"--unit=" + p.Service + "-upgrade-" + strconv.FormatInt(time.Now().Unix(), 10), "--collect", "--quiet", p.Bin, "upgrade-watch"}
	args = append(args, p.Args()...)
	out, err := exec.Command("systemd-run", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run 失败: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Args 把计划编成 upgrade-watch 子命令的参数;main.go 用 ParseArgs 解回来。
func (p Plan) Args() []string {
	return []string{
		"-bin", p.Bin, "-prev", p.Prev, "-failed", p.Failed, "-url", p.URL, "-service", p.Service,
		"-old-pid", strconv.Itoa(p.OldPID), "-db", p.DBPath, "-backup", p.Backup, "-status", p.StatusPath,
		"-from", p.From, "-to", p.To, "-timeout", strconv.Itoa(int(p.Timeout / time.Second)),
	}
}

// ParseArgs 是 Args 的反向:`m-ui upgrade-watch <args>` 用它取回计划。
func ParseArgs(args []string) Plan {
	fs := flag.NewFlagSet("upgrade-watch", flag.ContinueOnError)
	var p Plan
	var timeout int
	fs.StringVar(&p.Bin, "bin", "/usr/local/bin/m-ui", "正式程序路径")
	fs.StringVar(&p.Prev, "prev", "", "旧程序暂存路径(默认 <bin>.prev)")
	fs.StringVar(&p.Failed, "failed", "", "失败的新程序保存路径(默认 <bin>.failed)")
	fs.StringVar(&p.URL, "url", "", "面板本机地址,健康检查打它")
	fs.StringVar(&p.Service, "service", DefaultService, "systemd 单元名")
	fs.IntVar(&p.OldPID, "old-pid", 0, "换程序前的服务进程号")
	fs.StringVar(&p.DBPath, "db", "", "数据库路径(还原备份用)")
	fs.StringVar(&p.Backup, "backup", "", "升级前备份 zip")
	fs.StringVar(&p.StatusPath, "status", "", "结果文件")
	fs.StringVar(&p.From, "from", "", "旧版本")
	fs.StringVar(&p.To, "to", "", "新版本")
	fs.IntVar(&timeout, "timeout", int(DefaultTimeout/time.Second), "等新版本健康的秒数")
	_ = fs.Parse(args)
	p.Timeout = time.Duration(timeout) * time.Second
	if p.Prev == "" {
		p.Prev = PrevPath(p.Bin)
	}
	if p.Failed == "" {
		p.Failed = FailedPath(p.Bin)
	}
	return p
}

// Watch 执行判定与回滚。out 收到人能看的进度;返回的 Status 已经写进 p.StatusPath。
// 新版本健康返回 nil;回滚了返回 ErrRolledBack;回滚也救不回来返回其它错误。
func Watch(p Plan, out io.Writer) (Status, error) {
	return newWatcher(p, out).run()
}

// watcher 把系统交互收成三个可替换的函数,测试里换成假的。
type watcher struct {
	p        Plan
	out      io.Writer
	sysctl   func(args ...string) error
	oldAlive func() bool // 旧进程还在跑(exec 原地替换时 PID 不变,所以看 /proc/<pid>/exe 是否还是旧文件)
	healthy  func() bool
	restore  func(bin, db, backup string) error
	sleep    func(time.Duration)
	now      func() time.Time
}

func newWatcher(p Plan, out io.Writer) *watcher {
	if p.Service == "" {
		p.Service = DefaultService
	}
	if p.Timeout <= 0 {
		p.Timeout = DefaultTimeout
	}
	if p.Prev == "" {
		p.Prev = PrevPath(p.Bin)
	}
	if p.Failed == "" {
		p.Failed = FailedPath(p.Bin)
	}
	if out == nil {
		out = io.Discard
	}
	w := &watcher{p: p, out: out, sleep: time.Sleep, now: time.Now}
	w.sysctl = func(args ...string) error {
		o, err := exec.Command("systemctl", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(string(o)))
		}
		return nil
	}
	w.oldAlive = func() bool { return oldRunning(p.OldPID, p.Prev) }
	w.healthy = func() bool { return httpHealthy(p.URL) }
	w.restore = func(bin, db, backup string) error {
		o, err := exec.Command(bin, "restore", "-db", db, "-from", backup).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v %s", err, strings.TrimSpace(string(o)))
		}
		return nil
	}
	return w
}

func (w *watcher) logf(f string, a ...interface{}) { fmt.Fprintf(w.out, f+"\n", a...) }

func (w *watcher) waitHealthy(d time.Duration) bool {
	deadline := w.now().Add(d)
	for {
		if w.healthy() {
			return true
		}
		if w.now().After(deadline) {
			return false
		}
		w.sleep(time.Second)
	}
}

func (w *watcher) run() (Status, error) {
	p := w.p
	st := Status{At: time.Now().Unix(), From: p.From, To: p.To, Backup: p.Backup}
	finish := func(msg string, err error) (Status, error) {
		st.Message = msg
		w.logf("%s", msg)
		writeStatus(p.StatusPath, st)
		return st, err
	}

	// 1. 旧进程退出之前,它的首页照样是 200;先等旧进程真的换掉再判。
	if p.OldPID > 0 {
		w.logf("等待 %s 重启(旧进程 %d)…", p.Service, p.OldPID)
		deadline := w.now().Add(60 * time.Second)
		for w.oldAlive() && w.now().Before(deadline) {
			w.sleep(time.Second)
		}
		if w.oldAlive() {
			return finish(fmt.Sprintf("60 秒内 %s 没有重启,旧版本仍在运行;新程序已就位,下次重启生效,旧程序保留在 %s", p.Service, p.Prev), errors.New("服务没有重启"))
		}
	}

	// 2. 新版本健康?
	w.logf("等待新版本 %s 启动(最多 %s):%s", p.To, p.Timeout, p.URL)
	if w.waitHealthy(p.Timeout) {
		os.Remove(p.Prev)
		st.OK, st.Healthy = true, true
		return finish(fmt.Sprintf("已更新到 %s,面板正常", p.To), nil)
	}

	// 3. 换回旧程序
	w.logf("新版本 %s 在 %s 内没有起来,回滚到 %s …", p.To, p.Timeout, p.From)
	if _, err := os.Stat(p.Prev); err != nil {
		return finish("没有旧程序可回滚(首次安装?),请查看 journalctl -u "+p.Service, errors.New("没有旧程序"))
	}
	_ = w.sysctl("stop", p.Service)
	os.Remove(p.Failed)
	if err := os.Rename(p.Bin, p.Failed); err != nil {
		w.logf("保留失败程序时出错(继续):%v", err)
	}
	if err := os.Rename(p.Prev, p.Bin); err != nil {
		return finish("换回旧程序失败:"+err.Error()+",请手动处理 "+p.Prev, err)
	}
	st.RolledBack, st.Failed = true, p.Failed
	if err := w.sysctl("start", p.Service); err != nil {
		w.logf("%v", err)
	}
	if w.waitHealthy(60 * time.Second) {
		st.Healthy = true
		return finish(fmt.Sprintf("已回滚到 %s,面板正常;失败的新程序保留在 %s", p.From, p.Failed), ErrRolledBack)
	}

	// 4. 旧程序对新库也不行?还原升级前备份
	if p.Backup != "" && p.DBPath != "" {
		if _, err := os.Stat(p.Backup); err == nil {
			w.logf("旧程序仍未起来,还原升级前备份 %s …", p.Backup)
			_ = w.sysctl("stop", p.Service)
			if err := w.restore(p.Bin, p.DBPath, p.Backup); err != nil {
				w.logf("还原失败:%v", err)
			} else {
				st.DBRestored = true
			}
			_ = w.sysctl("start", p.Service)
			if w.waitHealthy(60 * time.Second) {
				st.Healthy = true
				return finish(fmt.Sprintf("已回滚到 %s 并还原升级前数据库备份,面板正常;失败的新程序保留在 %s", p.From, p.Failed), ErrRolledBack)
			}
		}
	}
	return finish(fmt.Sprintf("已换回 %s 但面板仍未起来,请查看 journalctl -u %s;升级前备份:%s", p.From, p.Service, p.Backup), errors.New("回滚后仍不健康"))
}

// oldRunning:pid 还在,而且它执行的仍是旧程序文件(exec 原地重启后 /proc/<pid>/exe 会指向新程序)。
func oldRunning(pid int, prev string) bool {
	if pid <= 0 {
		return false
	}
	exe, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false
	}
	old, err := os.Stat(prev)
	if err != nil {
		return false
	}
	return os.SameFile(exe, old)
}

// httpHealthy:本机首页 2xx/3xx 即健康(证书自签也算,只看进程有没有把面板端口服务起来)。
func httpHealthy(url string) bool {
	if url == "" {
		return false
	}
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func writeStatus(path string, st Status) {
	if path == "" {
		return
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = os.WriteFile(path, b, 0o600)
	if f, err := os.OpenFile(filepath.Join(filepath.Dir(path), "upgrade.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s %s → %s ok=%v rolledBack=%v dbRestored=%v healthy=%v %s\n", time.Unix(st.At, 0).Format(time.RFC3339), st.From, st.To, st.OK, st.RolledBack, st.DBRestored, st.Healthy, st.Message)
		f.Close()
	}
}

// ReadStatus 读结果文件;没有就返回 nil。
func ReadStatus(path string) *Status {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var st Status
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}

// ClearStatus 删掉结果文件(管理员在面板上点了"知道了")。
func ClearStatus(path string) { os.Remove(path) }
