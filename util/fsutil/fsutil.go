// Package fsutil 替换文件的统一做法:新内容先完整写到同目录的临时文件并落盘,再一步改名换上;
// 任何一步失败,原文件原地不动。
//
// 由 2026-11-18 审计的几条问题而来:
//   - E1 import -force 先删目标库、再打开源库(源路径写错就丢库);
//   - A1 还原备份先把原库改名、再写新库,写失败(磁盘满)留下空库,下次启动当成全新安装;
//   - A2 还原时直接删掉原库的 -wal / -shm,进程被强杀过的话,留下的旧库少了 WAL 里已提交的数据;
//   - L4 证书和私钥先后各写一个,第二个写失败留下不配对的一对。
package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SyncDir 改名之后把目录项也落盘(Windows 上打不开目录,忽略)。
func SyncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// writeTemp 在 path 同目录写一个临时文件并落盘,返回它的路径;失败时临时文件已删掉。
func writeTemp(path string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// WriteFileAtomic 写同目录临时文件 → 落盘 → 改名 → 落盘目录。失败时原文件不动、临时文件删掉。
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := writeTemp(path, data, mode)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	SyncDir(filepath.Dir(path))
	return nil
}

// File 要一起换上的一个文件。
type File struct {
	Path string
	Data []byte
	Mode os.FileMode
}

// WriteFilesAtomic 一起换上几个相关的文件(证书与私钥):全部写好、落盘之后才开始改名;
// 改名中途失败,已经换上的写回原来的内容(原来没有的删掉),返回错误。
// 写临时文件这一步最可能失败(磁盘满),这时一个都没换。
func WriteFilesAtomic(files ...File) error {
	tmps := make([]string, len(files))
	cleanup := func() {
		for _, t := range tmps {
			if t != "" {
				os.Remove(t)
			}
		}
	}
	for i, f := range files {
		t, err := writeTemp(f.Path, f.Data, f.Mode)
		if err != nil {
			cleanup()
			return err
		}
		tmps[i] = t
	}
	olds := make([][]byte, len(files))
	had := make([]bool, len(files))
	for i, f := range files {
		if b, err := os.ReadFile(f.Path); err == nil {
			olds[i], had[i] = b, true
		}
	}
	for i, f := range files {
		if err := os.Rename(tmps[i], f.Path); err != nil {
			for j := 0; j < i; j++ {
				if had[j] {
					_ = WriteFileAtomic(files[j].Path, olds[j], files[j].Mode)
				} else {
					_ = os.Remove(files[j].Path)
				}
			}
			cleanup()
			return err
		}
		tmps[i] = ""
	}
	seen := map[string]bool{}
	for _, f := range files {
		if d := filepath.Dir(f.Path); !seen[d] {
			seen[d] = true
			SyncDir(d)
		}
	}
	return nil
}

// DBSuffixes SQLite 主库之外的两个文件:一份库连同它们才是完整的。
var DBSuffixes = []string{"-wal", "-shm"}

// RemoveDB 删掉一份库连同 -wal / -shm(清理没换上的临时库)。
func RemoveDB(p string) {
	for _, suf := range append([]string{""}, DBSuffixes...) {
		os.Remove(p + suf)
	}
}

// SwapDB 用已经写好、落盘、关闭了的 newPath 换掉 dbPath:
// dbPath 连同 -wal / -shm 一起改名成 bakPath(加同样的后缀)留着,再把 newPath 改名成 dbPath。
// 任何一步失败,都把已经改了名的挪回去,返回错误时 dbPath 还是原来那份。
// 返回值 bak:原库留在哪(原来没有库时为空)。
func SwapDB(dbPath, newPath, bakPath string) (bak string, err error) {
	if _, err := os.Stat(newPath); err != nil {
		return "", fmt.Errorf("新库不在: %w", err)
	}
	if bakPath == "" {
		return "", errors.New("没有给出原库的备份名")
	}
	dir := filepath.Dir(dbPath)
	type move struct{ from, to string }
	var done []move
	undo := func() {
		for i := len(done) - 1; i >= 0; i-- {
			_ = os.Rename(done[i].to, done[i].from)
		}
		SyncDir(dir)
	}
	hadDB := false
	for _, suf := range append([]string{""}, DBSuffixes...) {
		if _, err := os.Stat(dbPath + suf); err != nil {
			continue
		}
		if suf == "" {
			hadDB = true
		}
		if _, err := os.Stat(bakPath + suf); err == nil {
			undo()
			return "", fmt.Errorf("备份名 %s 已经有文件,不覆盖", filepath.Base(bakPath+suf))
		}
		if err := os.Rename(dbPath+suf, bakPath+suf); err != nil {
			undo()
			return "", fmt.Errorf("把原库 %s 改名留作备份失败: %w", filepath.Base(dbPath+suf), err)
		}
		done = append(done, move{dbPath + suf, bakPath + suf})
	}
	SyncDir(dir)
	if err := os.Rename(newPath, dbPath); err != nil {
		undo()
		return "", fmt.Errorf("换上新库失败: %w", err)
	}
	// 新库关闭时 SQLite 一般已经删掉了它自己的 -wal / -shm;万一留着空的,清掉,别和新位置的库混在一起
	for _, suf := range DBSuffixes {
		os.Remove(newPath + suf)
	}
	SyncDir(dir)
	if !hadDB {
		return "", nil
	}
	return bakPath, nil
}

// FreeDBName 给一份库找一个没被占用的名字:base 本身(连同 -wal / -shm)都空着就用它,否则依次试 base-2、base-3……
// 同一秒里做两次还原 / 导入时,第二次不会因为备份名撞了而失败,也不会盖掉第一次留下的备份。
func FreeDBName(base string) string {
	free := func(p string) bool {
		for _, suf := range append([]string{""}, DBSuffixes...) {
			if _, err := os.Stat(p + suf); err == nil {
				return false
			}
		}
		return true
	}
	if free(base) {
		return base
	}
	for i := 2; ; i++ {
		if p := fmt.Sprintf("%s-%d", base, i); free(p) {
			return p
		}
	}
}
