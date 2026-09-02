// Package backup 生成与还原 m-ui 备份:一个 zip 内含数据库一致快照、证书文件与 meta.json。
// 还原分两步:Stage 把备份放到 <db>.restore,进程重启时 ApplyPending 原子替换。
package backup

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database"

	"gorm.io/gorm"
)

const DBName = "m-ui.db"

type CertEntry struct {
	Path string `json:"path"` // 原机绝对路径,还原时写回同一路径
	Zip  string `json:"zip"`  // zip 内文件名
}

type Meta struct {
	Version string      `json:"version"`
	Time    int64       `json:"time"`
	Host    string      `json:"host"`
	Certs   []CertEntry `json:"certs"`
}

// Create 把数据库一致快照 + 证书文件打成 zip 写入 w。
func Create(db *gorm.DB, certPaths []string, version string, w io.Writer) error {
	tmp, err := os.CreateTemp("", "m-ui-snap-*.db")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	os.Remove(tmpPath) // VACUUM INTO 要求目标不存在
	defer os.Remove(tmpPath)
	if err := db.Exec("VACUUM INTO '" + strings.ReplaceAll(filepath.ToSlash(tmpPath), "'", "''") + "'").Error; err != nil {
		return fmt.Errorf("数据库快照: %w", err)
	}
	snap, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}

	host, _ := os.Hostname()
	meta := Meta{Version: version, Time: time.Now().Unix(), Host: host}
	zw := zip.NewWriter(w)
	if err := addFile(zw, DBName, snap); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, p := range certPaths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		b, err := os.ReadFile(p)
		if err != nil {
			continue // 证书缺失不阻止备份
		}
		name := fmt.Sprintf("cert/%d-%s", i, filepath.Base(p))
		if err := addFile(zw, name, b); err != nil {
			return err
		}
		meta.Certs = append(meta.Certs, CertEntry{Path: p, Zip: name})
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := addFile(zw, "meta.json", mb); err != nil {
		return err
	}
	return zw.Close()
}

func addFile(zw *zip.Writer, name string, data []byte) error {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()}
	f, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

// Summary 是对备份文件的检查结果。
type Summary struct {
	IsZip     bool  `json:"isZip"`
	Meta      Meta  `json:"meta"`
	Users     int64 `json:"users"`
	Lines     int64 `json:"lines"`
	Upstreams int64 `json:"upstreams"`
	Size      int64 `json:"size"`
}

// Inspect 校验备份文件(zip 或裸 .db)是 m-ui 数据库,并统计内容。
func Inspect(path string) (Summary, error) {
	var s Summary
	st, err := os.Stat(path)
	if err != nil {
		return s, err
	}
	s.Size = st.Size()
	dbBytes, meta, isZip, err := extract(path)
	if err != nil {
		return s, err
	}
	s.IsZip, s.Meta = isZip, meta
	tmp, err := os.CreateTemp("", "m-ui-inspect-*.db")
	if err != nil {
		return s, err
	}
	tmpPath := tmp.Name()
	tmp.Write(dbBytes)
	tmp.Close()
	defer os.Remove(tmpPath)
	db, err := database.OpenReadOnly(tmpPath)
	if err != nil {
		return s, fmt.Errorf("不是有效的 SQLite 数据库: %w", err)
	}
	defer database.Close(db)
	for _, t := range []string{"users", "lines", "settings"} {
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", t).Scan(&n).Error; err != nil || n == 0 {
			return s, fmt.Errorf("不是 m-ui 数据库:缺少 %s 表(旧面板库请在安装时用 --import 整机迁移,或到「用户」页「从旧面板导入」只导用户)", t)
		}
	}
	db.Raw("SELECT COUNT(*) FROM users").Scan(&s.Users)
	db.Raw("SELECT COUNT(*) FROM lines").Scan(&s.Lines)
	db.Raw("SELECT COUNT(*) FROM upstreams").Scan(&s.Upstreams)
	return s, nil
}

// extract 读出备份中的数据库字节与 meta;裸 .db 文件直接返回。
func extract(path string) (dbBytes []byte, meta Meta, isZip bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, meta, false, err
	}
	if len(raw) >= 16 && string(raw[:15]) == "SQLite format 3" {
		return raw, meta, false, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, meta, false, errors.New("既不是 SQLite 数据库也不是 zip 备份")
	}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return nil, meta, true, err
		}
		b, _ := io.ReadAll(io.LimitReader(rc, 512<<20))
		rc.Close()
		switch f.Name {
		case DBName:
			dbBytes = b
		case "meta.json":
			json.Unmarshal(b, &meta)
		}
	}
	if dbBytes == nil {
		return nil, meta, true, errors.New("zip 中没有 " + DBName)
	}
	return dbBytes, meta, true, nil
}

// Stage 把备份文件复制到 <dbPath>.restore,等待重启时应用。
func Stage(dbPath, srcPath string) error {
	if _, err := Inspect(srcPath); err != nil {
		return err
	}
	b, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	return os.WriteFile(dbPath+".restore", b, 0o600)
}

// PendingPath 返回待应用的还原文件路径(存在时)。
func PendingPath(dbPath string) string {
	p := dbPath + ".restore"
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// ApplyPending 在进程启动、数据库尚未打开时应用待还原文件:
// 当前库改名为 <db>.bak-<时间>,写入备份中的库与证书文件。
func ApplyPending(dbPath string) (bool, error) {
	p := PendingPath(dbPath)
	if p == "" {
		return false, nil
	}
	if err := Restore(dbPath, p); err != nil {
		os.Rename(p, p+".failed")
		return false, err
	}
	os.Remove(p)
	return true, nil
}

// Restore 直接把备份应用到 dbPath(要求该库当前未被打开)。
func Restore(dbPath, srcPath string) error {
	dbBytes, meta, isZip, err := extract(srcPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dbPath); err == nil {
		bak := dbPath + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.Rename(dbPath, bak); err != nil {
			return fmt.Errorf("备份当前库: %w", err)
		}
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		os.Remove(dbPath + suffix)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dbPath, dbBytes, 0o600); err != nil {
		return err
	}
	if !isZip {
		return nil
	}
	raw, _ := os.ReadFile(srcPath)
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	for _, c := range meta.Certs {
		f := files[c.Zip]
		if f == nil || c.Path == "" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		os.MkdirAll(filepath.Dir(c.Path), 0o755)
		mode := os.FileMode(0o644)
		if strings.HasSuffix(c.Path, ".key") {
			mode = 0o600
		}
		os.WriteFile(c.Path, b, mode)
	}
	return nil
}
