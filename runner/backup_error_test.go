package runner

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
)

// 定时备份失败记下原因和时间(概况「需要处理」显示),之后成功一次就清掉。
func TestNoteBackupError(t *testing.T) {
	r := &Runner{}
	if msg, at := r.BackupError(); msg != "" || at != 0 {
		t.Fatalf("没失败过应为空: %q %d", msg, at)
	}
	r.noteBackup(errors.New("no space left on device"))
	msg, at := r.BackupError()
	if msg != "no space left on device" || time.Now().Unix()-at > 5 {
		t.Fatalf("失败应记下原因和时间: %q %d", msg, at)
	}
	r.noteBackup(nil)
	if msg, at := r.BackupError(); msg != "" || at != 0 {
		t.Fatalf("成功后应清空: %q %d", msg, at)
	}
}

// 任何一次备份成功(手动「立即备份」、Telegram /backup)都清掉上次定时备份失败的原因:问题已经解决,概况不再常驻报错。
func TestBackupSuccessClearsError(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.DB())
	r.noteBackup(errors.New("no space left on device"))
	if _, err := r.CreateBackupFile(); err != nil {
		t.Fatal(err)
	}
	if msg, at := r.BackupError(); msg != "" || at != 0 {
		t.Fatalf("备份成功后应清空: %q %d", msg, at)
	}
}
