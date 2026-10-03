package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
)

// 日常备份按保留份数轮转时只数面板自己生成的(m-ui-*.zip),升级前备份(pre-upgrade-*)不算、也不删。
func TestRotateBackupsSkipsPreUpgrade(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.DB())
	dir := r.BackupDir()
	os.MkdirAll(dir, 0o700)
	now := time.Now()
	touch := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("x"), 0o600)
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	touch("pre-upgrade-0.7.0-1.zip", 1*time.Minute) // 最新的两份是升级前备份
	touch("pre-upgrade-0.7.1-1.zip", 2*time.Minute)
	touch("m-ui-a.zip", 3*time.Minute)
	touch("m-ui-b.zip", 4*time.Minute)
	touch("m-ui-c.zip", 5*time.Minute)
	r.SetSetting("backupKeep", "2")
	r.rotateBackups()
	have := map[string]bool{}
	for _, b := range r.ListBackups() {
		have[b.Name] = true
	}
	if !have["pre-upgrade-0.7.0-1.zip"] || !have["pre-upgrade-0.7.1-1.zip"] || !have["m-ui-a.zip"] || !have["m-ui-b.zip"] || have["m-ui-c.zip"] {
		t.Fatalf("应留 2 份日常备份、升级前备份不动: %v", have)
	}
}
