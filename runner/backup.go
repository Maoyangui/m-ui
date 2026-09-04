package runner

import (
	"errors"
	"github.com/Maoyangui/m-ui/notify"
	"github.com/Maoyangui/m-ui/tz"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/backup"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/logger"
)

// Version 由 main 注入,写进备份 meta。
var Version = "dev"

// BackupDir 返回本地备份目录。
func (r *Runner) BackupDir() string {
	if d := strings.TrimSpace(r.setting("backupDir")); d != "" {
		return d
	}
	return filepath.Join(r.DataDir(), "backups")
}

// CertPaths 返回所有设置里引用的证书/私钥路径(去重)。
func (r *Runner) CertPaths() []string {
	var out []string
	for _, k := range []string{"certFile", "keyFile", "webCertFile", "webKeyFile", "subCertFile", "subKeyFile"} {
		if v := strings.TrimSpace(r.setting(k)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// WriteBackup 把备份 zip 写到 w。
func (r *Runner) WriteBackup(w io.Writer) error {
	return backup.Create(r.db, r.CertPaths(), Version, w)
}

type BackupFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Time int64  `json:"time"`
}

// CreateBackupFile 在备份目录生成一个备份并按 backupKeep 轮转,返回文件名。
func (r *Runner) CreateBackupFile() (BackupFile, error) {
	dir := r.BackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return BackupFile{}, err
	}
	name := "m-ui-" + time.Now().Format("20060102-150405") + ".zip"
	// 备份里有全部用户凭据与证书私钥,按 0600 建文件(目录已是 0700,再加一层)
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return BackupFile{}, err
	}
	if err := r.WriteBackup(f); err != nil {
		f.Close()
		os.Remove(f.Name())
		return BackupFile{}, err
	}
	f.Close()
	st, _ := os.Stat(f.Name())
	r.rotateBackups()
	return BackupFile{Name: name, Size: st.Size(), Time: st.ModTime().Unix()}, nil
}

func (r *Runner) rotateBackups() {
	keep, _ := strconv.Atoi(r.setting("backupKeep"))
	if keep <= 0 {
		keep = 7
	}
	list := r.ListBackups()
	for i := keep; i < len(list); i++ {
		os.Remove(filepath.Join(r.BackupDir(), list[i].Name))
	}
}

// ListBackups 返回备份目录里的文件(新的在前)。
func (r *Runner) ListBackups() []BackupFile {
	entries, _ := os.ReadDir(r.BackupDir())
	out := []BackupFile{} // 非 nil,JSON 为 [] 而不是 null
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".zip") {
			continue
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupFile{Name: e.Name(), Size: st.Size(), Time: st.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

// BackupFilePath 校验文件名并返回绝对路径(防路径穿越)。
func (r *Runner) BackupFilePath(name string) (string, error) {
	if name == "" || name != filepath.Base(name) || !strings.HasSuffix(name, ".zip") {
		return "", errors.New("文件名无效")
	}
	p := filepath.Join(r.BackupDir(), name)
	if _, err := os.Stat(p); err != nil {
		return "", errors.New("备份不存在")
	}
	return p, nil
}

// SetSetting 写入一个设置项(面板保存证书设置用)。
func (r *Runner) SetSetting(key, val string) { r.setSetting(key, val) }

// InspectBackup 检查一个备份文件。
func (r *Runner) InspectBackup(src string) (backup.Summary, error) { return backup.Inspect(src) }

// RestorePending 报告是否有待应用的还原文件。
func (r *Runner) RestorePending() bool { return backup.PendingPath(r.dbPath) != "" }

// StageRestore 把备份文件放到待还原位置,重启后生效。
func (r *Runner) StageRestore(src string) (backup.Summary, error) {
	s, err := backup.Inspect(src)
	if err != nil {
		return s, err
	}
	return s, backup.Stage(r.dbPath, src)
}

// backupLoop 每分钟检查是否到了 backupHour(-1 关闭),每天一次。
func (r *Runner) backupLoop(stop <-chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			hour, err := strconv.Atoi(strings.TrimSpace(r.setting("backupHour")))
			now := time.Now().In(tz.Location(r.setting("timezone"))) // 按面板时区的钟点,不看服务器本地时间
			if err != nil || hour < 0 || now.Hour() != hour {
				continue
			}
			if !r.notifier.Once("backup:"+now.Format("2006-01-02"), 36*time.Hour) {
				continue
			}
			bf, err := r.CreateBackupFile()
			if err != nil {
				logger.Warning("定时备份失败: ", err)
				r.notifier.Event("tgOnCore", "🔴 <b>定时备份失败</b>\n"+notify.Esc(err.Error()))
				continue
			}
			logger.Info("定时备份完成: ", bf.Name)
			if strings.EqualFold(r.setting("backupTelegram"), "true") {
				b, err := os.ReadFile(filepath.Join(r.BackupDir(), bf.Name))
				if err == nil {
					if err := r.notifier.SendDocument(bf.Name, b, "📦 m-ui 定时备份"); err != nil {
						logger.Warning("备份推送 Telegram 失败: ", err)
					}
				}
			}
		case <-stop:
			return
		}
	}
}

// ScheduleRestart 在短暂延迟后重启进程(还原备份、改端口后使用)。
func (r *Runner) ScheduleRestart(delay time.Duration) {
	go func() {
		time.Sleep(delay)
		logger.Info("正在重启 m-ui")
		r.Stop()
		if r.subSrv != nil {
			r.subSrv.Stop()
		}
		database.Checkpoint(r.db)
		database.Close(r.db)
		restartProcess()
	}()
}
