package backup

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// makeBackup 造一份只有一个用户 restored 的备份包。
func makeBackup(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(dir, "src.db")
	db, err := database.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	db.Create(&model.User{Name: "restored", Enabled: true})
	var buf bytes.Buffer
	if err := Create(db, nil, "test", &buf); err != nil {
		t.Fatal(err)
	}
	database.Close(db)
	p := filepath.Join(dir, "b.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

func hasUser(t *testing.T, dbPath, name string) bool {
	t.Helper()
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("打开 %s: %v", dbPath, err)
	}
	defer database.Close(db)
	var n int64
	db.Model(&model.User{}).Where("name = ?", name).Count(&n)
	return n == 1
}

// 审计 A2:进程被强杀后(已提交的数据还在 WAL 里、没并回主库)应用还原,
// 留下的旧库要连同 -wal / -shm 一起留着,打开它时数据还在。以前还原直接删掉 -wal / -shm。
func TestRestoreKeepsUncheckpointedWalInBackup(t *testing.T) {
	dir := t.TempDir()
	zipPath := makeBackup(t, dir)

	// 造「被强杀」的现场:库开着、写了一笔,不关库(WAL 不并回),把三个文件原样拷到还原的目标位置
	live := filepath.Join(dir, "live.db")
	db, err := database.Open(live)
	if err != nil {
		t.Fatal(err)
	}
	db.Create(&model.User{Name: "walprobe", Enabled: true})
	dstDir := filepath.Join(dir, "data")
	os.MkdirAll(dstDir, 0o755)
	dst := filepath.Join(dstDir, DBName)
	copyFile(t, live, dst)
	if _, err := os.Stat(live + "-wal"); err != nil {
		t.Skip("这个 SQLite 没有留下 -wal,造不出现场")
	}
	copyFile(t, live+"-wal", dst+"-wal")
	if _, err := os.Stat(live + "-shm"); err == nil {
		copyFile(t, live+"-shm", dst+"-shm")
	}
	database.Close(db)

	rep, err := Restore(dst, zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Bak == "" {
		t.Fatal("应留下原库备份")
	}
	if _, err := os.Stat(rep.Bak + "-wal"); err != nil {
		t.Fatal("原库的 -wal 应跟着备份留下")
	}
	if !hasUser(t, rep.Bak, "walprobe") {
		t.Fatal("备份里少了 WAL 中已提交的数据")
	}
	if !hasUser(t, dst, "restored") {
		t.Fatal("还原后的库应是备份里的内容")
	}
}

// 审计 A1:新库写不进去(这里让临时文件的名字被一个非空目录占着,改名必然失败;现实里是磁盘满)
// —— 当前库原地不动、不产生 .bak,也不算还原了(rep.DB 为假)。以前此时原库已经改名、留下空库。
func TestRestoreWriteFailureKeepsCurrentDB(t *testing.T) {
	dir := t.TempDir()
	zipPath := makeBackup(t, dir)
	dstDir := filepath.Join(dir, "data")
	os.MkdirAll(dstDir, 0o755)
	dst := filepath.Join(dstDir, DBName)
	db, err := database.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	db.Create(&model.User{Name: "current", Enabled: true})
	database.Close(db)
	if err := os.MkdirAll(filepath.Join(dst+".restoring", "busy"), 0o755); err != nil {
		t.Fatal(err)
	}

	rep, err := Restore(dst, zipPath)
	if err == nil {
		t.Fatal("新库写不进去应报错")
	}
	if rep.DB {
		t.Fatal("没换成功不能报告「库已还原」")
	}
	if baks, _ := filepath.Glob(dst + ".bak-*"); len(baks) > 0 {
		t.Fatalf("没换成功不该有备份: %v", baks)
	}
	if !hasUser(t, dst, "current") {
		t.Fatal("当前库应原样留在原处")
	}
}

// pruneBaks 按主库分组:删掉的旧备份连同 -wal / -shm 一起删,留下的完整。
func TestPruneBaksGroupsWalAndShm(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, DBName)
	for _, stamp := range []string{"20260101-000000", "20260102-000000", "20260103-000000"} {
		for _, suf := range []string{"", "-wal", "-shm"} {
			os.WriteFile(db+".bak-"+stamp+suf, []byte("x"), 0o600)
		}
	}
	pruneBaks(db, 2)
	for _, suf := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(db + ".bak-20260101-000000" + suf); err == nil {
			t.Fatalf("最旧的一份(%q)应删掉", suf)
		}
		for _, keep := range []string{"20260102-000000", "20260103-000000"} {
			if _, err := os.Stat(db + ".bak-" + keep + suf); err != nil {
				t.Fatalf("较新的 %s%s 应留着", keep, suf)
			}
		}
	}
}
