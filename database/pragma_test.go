package database

import (
	"path/filepath"
	"testing"
)

// WAL 下 synchronous 要是 NORMAL(=1):提交不再逐笔 fsync,盘慢时面板不会跟着卡;检查点循环用 PASSIVE 不阻塞。
func TestPragmasNormalSyncAndPassiveCheckpoint(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer Close(db)
	var sync int
	db.Raw("PRAGMA synchronous").Scan(&sync)
	if sync != 1 {
		t.Fatalf("synchronous 应为 NORMAL(1),得到 %d", sync)
	}
	var mode string
	db.Raw("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Fatalf("journal_mode 应为 wal,得到 %s", mode)
	}
	if err := CheckpointPassive(db); err != nil {
		t.Fatalf("PASSIVE 检查点失败: %v", err)
	}
	if err := Checkpoint(db); err != nil {
		t.Fatalf("TRUNCATE 检查点失败: %v", err)
	}
}
