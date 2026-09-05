package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/selfupdate"
)

// 面板里的"有新版本"提示与一键更新。
//
// 检查结果缓存在内存并落一份到设置里,刷新页面不会每次都去打 GitHub;
// 更新是一次事务:升级前备份 → 下载校验 → 旧程序留作 .prev、新程序换上 → 重启 →
// 服务之外的守护(m-ui upgrade-watch)等新版本健康,起不来就自动换回旧程序、必要时还原备份。
// 数据库、证书、设置在成功路径上一概不动;回滚过会写 upgrade-status.json,状态接口带给页面明说。

var (
	canUpdateOnce sync.Once
	canUpdateVal  bool
	updateMu      sync.Mutex
	updateInfo    selfupdate.Info
	updating      bool
)

// checkUpdate 查一次新版本(带缓存);force 为真时忽略缓存。
func (s *Server) checkUpdate(force bool) selfupdate.Info {
	updateMu.Lock()
	cached := updateInfo
	updateMu.Unlock()
	if !force && cached.CheckedAt > 0 && time.Now().Unix()-cached.CheckedAt < 6*3600 {
		return cached
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info, err := selfupdate.Check(ctx, Version)
	if err != nil {
		logger.Warning("检查新版本失败: ", err)
		if cached.Latest != "" { // 查不到就沿用上次的结果,不要把已有提示抹掉
			cached.Err = info.Err
			return cached
		}
	}
	updateMu.Lock()
	updateInfo = info
	updateMu.Unlock()
	s.saveSettings(map[string]string{
		"updateLatest":    info.Latest,
		"updateCheckedAt": strconv.FormatInt(info.CheckedAt, 10),
	})
	return info
}

// StartUpdateWatch 启动时查一次,之后每 6 小时查一次。
func (s *Server) StartUpdateWatch() {
	go func() {
		time.Sleep(20 * time.Second) // 让面板先起来,别和启动抢网络
		for {
			s.checkUpdate(true)
			time.Sleep(6 * time.Hour)
		}
	}()
}

// upgradeStatusPath 是上一次升级的结果文件(回滚过才有意义)。
func (s *Server) upgradeStatusPath() string { return selfupdate.StatusPath(s.run.DataDir()) }

// lastUpgrade 返回需要管理员知道的升级结果:回滚过、或回滚也没成的那种;成功的不打扰。
func (s *Server) lastUpgrade() *selfupdate.Status {
	st := selfupdate.ReadStatus(s.upgradeStatusPath())
	if st == nil || st.OK {
		return nil
	}
	return st
}

// handleUpdate GET 返回版本信息(?force=1 立即重查);POST 执行更新并重启。
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		info := s.checkUpdate(r.URL.Query().Get("force") == "1")
		updateMu.Lock()
		busy := updating
		updateMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"current": info.Current, "latest": info.Latest, "hasUpdate": info.HasUpdate,
			"checkedAt": info.CheckedAt, "error": info.Err, "updating": busy,
			"canUpdate": canSelfUpdate(), "lastUpgrade": s.lastUpgrade(),
		})
	case http.MethodPost:
		updateMu.Lock()
		if updating {
			updateMu.Unlock()
			badRequest(w, errUpdating)
			return
		}
		info := updateInfo
		updating = true
		updateMu.Unlock()
		defer func() {
			updateMu.Lock()
			updating = false
			updateMu.Unlock()
		}()

		if !canSelfUpdate() {
			badRequest(w, errCannotUpdate)
			return
		}
		if info.Latest == "" {
			info = s.checkUpdate(true)
		}
		if info.Latest == "" {
			badRequest(w, errNoRelease)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		bin, _ := os.Executable()
		logf := func(f string, a ...interface{}) { logger.Info("更新: ", f) }

		// 1. 升级前备份(面板自己的备份格式,带 WAL 检查点):回滚时旧程序对新库不放心就还原它
		backupPath := s.preUpgradeBackup()

		// 2. 下载、校验、试运行,然后旧程序留作 .prev、新程序换上
		newPath, err := selfupdate.Stage(ctx, info.Latest, bin, logf)
		if err != nil {
			badRequest(w, err)
			return
		}
		if err := selfupdate.Swap(bin, newPath, selfupdate.PrevPath(bin)); err != nil {
			badRequest(w, err)
			return
		}

		// 3. 服务之外的守护:等新版本健康;起不来就换回 .prev(必要时还原备份)
		plan := selfupdate.Plan{
			Bin: bin, Prev: selfupdate.PrevPath(bin), Failed: selfupdate.FailedPath(bin),
			From: "v" + Version, To: info.Latest,
			URL:     selfupdate.LocalURL(s.setting("webCertFile") != "", s.settingInt("webPort", 2053), s.basePath()),
			Service: selfupdate.ServiceName(), OldPID: os.Getpid(),
			DBPath: s.run.DBPath(), Backup: backupPath, StatusPath: s.upgradeStatusPath(), Timeout: selfupdate.DefaultTimeout,
		}
		selfupdate.ClearStatus(plan.StatusPath)
		watched := true
		if err := selfupdate.LaunchWatcher(plan); err != nil {
			watched = false
			logger.Warning("无法启动升级守护,这次更新没有自动回滚(旧程序保留在 ", plan.Prev, "): ", err)
		}
		s.audit(r, "panel", "update", info.Latest)
		logger.Info("已换上 ", info.Latest, ",正在重启面板;守护=", watched, " 备份=", backupPath)
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": "1", "version": info.Latest, "watch": watched, "backup": filepath.Base(backupPath)})
		// 先把响应写回去再重启,前端才能进入"等待面板回来"的轮询
		s.run.ScheduleRestart(800 * time.Millisecond)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// preUpgradeBackup 在备份目录写一份 pre-upgrade-<版本>-<时间>.zip,只保留最近两份;失败返回空(升级继续,回滚时只换程序)。
func (s *Server) preUpgradeBackup() string {
	dir := s.run.BackupDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		logger.Warning("升级前备份失败(继续): ", err)
		return ""
	}
	p := filepath.Join(dir, selfupdate.BackupName("v"+Version))
	f, err := os.Create(p)
	if err != nil {
		logger.Warning("升级前备份失败(继续): ", err)
		return ""
	}
	werr := s.run.WriteBackup(f)
	f.Close()
	if werr != nil {
		os.Remove(p)
		logger.Warning("升级前备份失败(继续,回滚时只换回旧程序): ", werr)
		return ""
	}
	selfupdate.Prune(dir, selfupdate.BackupPrefix, selfupdate.KeepBackups)
	logger.Info("升级前备份: ", p)
	return p
}

// handleUpdateAck 管理员看过"已回滚"提示后清掉状态文件。
func (s *Server) handleUpdateAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	selfupdate.ClearStatus(s.upgradeStatusPath())
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// canSelfUpdate 只有 Linux 上以 root 跑、且二进制所在目录可写时才允许一键更新。
// 注意不能去写正在运行的二进制本身(Linux 会返回 ETXTBSY);替换靠 rename,
// 需要的是目录写权限,所以在同目录建个临时文件来判断。
func canSelfUpdate() bool {
	canUpdateOnce.Do(func() { canUpdateVal = probeSelfUpdate() })
	return canUpdateVal
}

func probeSelfUpdate() bool {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return false
	}
	bin, err := os.Executable()
	if err != nil {
		return false
	}
	f, err := os.CreateTemp(filepath.Dir(bin), ".m-ui-upd-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

type updateErr string

func (e updateErr) Error() string { return string(e) }

const (
	errUpdating     = updateErr("更新正在进行中")
	errCannotUpdate = updateErr("当前环境不支持一键更新(需要在 Linux 上以 root 运行),请用安装脚本更新")
	errNoRelease    = updateErr("没有查到可用的发布版本")
)
