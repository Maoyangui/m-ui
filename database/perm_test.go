package database

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 主库与 -wal / -shm 只给本账号读写:新建的是 0600,老库(以前按 0644 建的)打开时收紧。
func TestDatabaseFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不按 Unix 权限位控制读取,由 CI 在 Linux 上核验")
	}
	dir := t.TempDir()
	old := filepath.Join(dir, "old.db")
	if err := os.WriteFile(old, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "new.db"), old} {
		db, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		db.Create(&model.Setting{Key: "k", Value: "v"}) // 有写入才有 -wal
		for _, f := range []string{p, p + "-wal", p + "-shm"} {
			st, err := os.Stat(f)
			if err != nil {
				continue
			}
			if perm := st.Mode().Perm(); perm&0o077 != 0 {
				t.Errorf("%s 的权限是 %o,不该让其它账户读到", filepath.Base(f), perm)
			}
		}
		Close(db)
	}
}
