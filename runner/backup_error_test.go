package runner

import (
	"errors"
	"testing"
	"time"
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
