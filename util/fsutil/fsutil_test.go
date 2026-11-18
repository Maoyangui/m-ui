package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	return string(b)
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// noTemps 目录里不能留下临时文件。
func noTemps(t *testing.T, dir string) {
	t.Helper()
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("留下了临时文件 %s", e.Name())
		}
	}
}

func TestWriteFileAtomicReplacesAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.key")
	write(t, p, "old")
	if err := WriteFileAtomic(p, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p); got != "new" {
		t.Fatalf("内容 %q", got)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("权限 %v,应为 0600", st.Mode().Perm())
		}
	}
	noTemps(t, dir)
}

// 第二个文件的临时文件写不出来(目录不存在,相当于磁盘满之类):一个都不换。
func TestWriteFilesAtomicNothingChangesWhenATempFails(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "x.crt"), filepath.Join(dir, "missing-dir", "x.key")
	write(t, crt, "old-cert")
	err := WriteFilesAtomic(File{crt, []byte("new-cert"), 0o644}, File{key, []byte("new-key"), 0o600})
	if err == nil {
		t.Fatal("第二个写不出来应报错")
	}
	if got := read(t, crt); got != "old-cert" {
		t.Fatalf("第一个文件不该被换:%q", got)
	}
	noTemps(t, dir)
}

// 改名到第二个时失败(目标是个非空目录):已经换上的第一个写回原内容。
func TestWriteFilesAtomicRollsBackOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	key, crt := filepath.Join(dir, "x.key"), filepath.Join(dir, "x.crt")
	write(t, key, "old-key")
	if err := os.MkdirAll(filepath.Join(crt, "busy"), 0o755); err != nil { // x.crt 是个非空目录,改名会失败
		t.Fatal(err)
	}
	err := WriteFilesAtomic(File{key, []byte("new-key"), 0o600}, File{crt, []byte("new-cert"), 0o644})
	if err == nil {
		t.Fatal("第二个改名失败应报错")
	}
	if got := read(t, key); got != "old-key" {
		t.Fatalf("第一个文件应写回原内容,现在是 %q", got)
	}
	noTemps(t, dir)
}

func TestWriteFilesAtomicReplacesBoth(t *testing.T) {
	dir := t.TempDir()
	key, crt := filepath.Join(dir, "x.key"), filepath.Join(dir, "x.crt")
	write(t, key, "old-key")
	write(t, crt, "old-cert")
	if err := WriteFilesAtomic(File{key, []byte("new-key"), 0o600}, File{crt, []byte("new-cert"), 0o644}); err != nil {
		t.Fatal(err)
	}
	if read(t, key) != "new-key" || read(t, crt) != "new-cert" {
		t.Fatal("两个都应换上")
	}
	noTemps(t, dir)
}

// 换库:原库连同 -wal / -shm 一起改名留着(A2),新库换上。
func TestSwapDBKeepsOldWithWalAndShm(t *testing.T) {
	dir := t.TempDir()
	db, nw, bak := filepath.Join(dir, "m.db"), filepath.Join(dir, "m.db.new"), filepath.Join(dir, "m.db.bak-1")
	write(t, db, "old-db")
	write(t, db+"-wal", "old-wal")
	write(t, db+"-shm", "old-shm")
	write(t, nw, "new-db")
	got, err := SwapDB(db, nw, bak)
	if err != nil {
		t.Fatal(err)
	}
	if got != bak {
		t.Fatalf("返回的备份名 %q", got)
	}
	if read(t, db) != "new-db" {
		t.Fatal("新库没换上")
	}
	for suf, want := range map[string]string{"": "old-db", "-wal": "old-wal", "-shm": "old-shm"} {
		if read(t, bak+suf) != want {
			t.Fatalf("备份 %s 不对", suf)
		}
	}
	for _, suf := range DBSuffixes {
		if _, err := os.Stat(db + suf); err == nil {
			t.Fatalf("新库旁边不该还有旧的 %s", suf)
		}
	}
	if _, err := os.Stat(nw); err == nil {
		t.Fatal("新库的临时名应该没了")
	}
}

// 原来没有库:直接换上,备份名为空。
func TestSwapDBWithoutOld(t *testing.T) {
	dir := t.TempDir()
	db, nw := filepath.Join(dir, "m.db"), filepath.Join(dir, "m.db.new")
	write(t, nw, "new-db")
	got, err := SwapDB(db, nw, filepath.Join(dir, "m.db.bak-1"))
	if err != nil || got != "" {
		t.Fatalf("err=%v bak=%q", err, got)
	}
	if read(t, db) != "new-db" {
		t.Fatal("新库没换上")
	}
}

// 中途出错(-wal 的备份名已被占):已经挪走的主库挪回来,原库原样不动。
func TestSwapDBUndoesOnFailure(t *testing.T) {
	dir := t.TempDir()
	db, nw, bak := filepath.Join(dir, "m.db"), filepath.Join(dir, "m.db.new"), filepath.Join(dir, "m.db.bak-1")
	write(t, db, "old-db")
	write(t, db+"-wal", "old-wal")
	write(t, nw, "new-db")
	write(t, bak+"-wal", "someone-else")
	if _, err := SwapDB(db, nw, bak); err == nil {
		t.Fatal("备份名被占应报错")
	}
	if read(t, db) != "old-db" || read(t, db+"-wal") != "old-wal" {
		t.Fatal("原库应原样留在原处")
	}
	if _, err := os.Stat(bak); err == nil {
		t.Fatal("主库不该还留在备份名下")
	}
	if read(t, nw) != "new-db" {
		t.Fatal("新库没换上时应还在原处(调用方负责清理)")
	}
}

// 新库不在:直接报错,什么都不动。
func TestSwapDBMissingNew(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.db")
	write(t, db, "old-db")
	if _, err := SwapDB(db, filepath.Join(dir, "nope"), filepath.Join(dir, "bak")); err == nil {
		t.Fatal("新库不在应报错")
	}
	if read(t, db) != "old-db" {
		t.Fatal("原库不该动")
	}
}

func TestFreeDBName(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "m.db.bak-1")
	if FreeDBName(base) != base {
		t.Fatal("空着应直接用")
	}
	write(t, base, "x")
	if got := FreeDBName(base); got != base+"-2" {
		t.Fatalf("占了应换成 -2,得到 %s", got)
	}
	write(t, base+"-2-wal", "x") // 只剩 -wal 也算被占
	if got := FreeDBName(base); got != base+"-3" {
		t.Fatalf("得到 %s", got)
	}
}
