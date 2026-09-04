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
// 更新只替换 /usr/local/bin/m-ui 再重启服务,数据库、证书、备份、设置一概不动。

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
			"canUpdate": canSelfUpdate(),
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
		if err := selfupdate.Apply(ctx, info.Latest, bin, func(f string, a ...interface{}) {
			logger.Info("更新: ", f)
		}); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "panel", "update", info.Latest)
		logger.Info("已更新到 ", info.Latest, ",正在重启面板")
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1", "version": info.Latest})
		// 先把响应写回去再重启,前端才能进入"等待面板回来"的轮询
		s.run.ScheduleRestart(800 * time.Millisecond)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
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
