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
	"sort"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/logger"

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
	// CertsUnreadable 库里引用、备份时却读不出来的证书文件(没打进包)。老版本不认这个字段,照常还原。
	CertsUnreadable []string `json:"certsUnreadable,omitempty"`
}

// Create 把数据库一致快照 + 证书文件打成 zip 写入 w(快照临时文件放系统临时目录)。
func Create(db *gorm.DB, certPaths []string, version string, w io.Writer) error {
	return CreateIn(db, certPaths, version, "", w)
}

// CreateIn 同 Create,快照临时文件放在 tmpDir(数据目录):/tmp 可能是很小的内存盘,库大一点就写不下。
func CreateIn(db *gorm.DB, certPaths []string, version, tmpDir string, w io.Writer) error {
	_, err := CreateInMeta(db, certPaths, version, tmpDir, w)
	return err
}

// CreateInMeta 同 CreateIn,并返回写进包里的 meta(m-ui backup 据此提示哪些证书没读到)。
func CreateInMeta(db *gorm.DB, certPaths []string, version, tmpDir string, w io.Writer) (Meta, error) {
	var meta Meta
	tmp, err := os.CreateTemp(tmpDir, "m-ui-snap-*.db")
	if err != nil {
		return meta, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	os.Remove(tmpPath) // VACUUM INTO 要求目标不存在
	defer os.Remove(tmpPath)
	if err := db.Exec("VACUUM INTO '" + strings.ReplaceAll(filepath.ToSlash(tmpPath), "'", "''") + "'").Error; err != nil {
		return meta, fmt.Errorf("数据库快照: %w", err)
	}
	snap, err := os.ReadFile(tmpPath)
	if err != nil {
		return meta, err
	}

	host, _ := os.Hostname()
	meta = Meta{Version: version, Time: time.Now().Unix(), Host: host}
	zw := zip.NewWriter(w)
	if err := addFile(zw, DBName, snap); err != nil {
		return meta, err
	}
	seen := map[string]bool{}
	for i, p := range certPaths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		b, err := os.ReadFile(p) // 符号链接读的是它指向的内容(certbot 的 live/ 全是链接)
		if err != nil {
			meta.CertsUnreadable = append(meta.CertsUnreadable, p) // 证书缺失不阻止备份,但记下来:还原时好说清楚
			continue
		}
		name := fmt.Sprintf("cert/%d-%s", i, filepath.Base(p))
		if err := addFile(zw, name, b); err != nil {
			return meta, err
		}
		meta.Certs = append(meta.Certs, CertEntry{Path: p, Zip: name})
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := addFile(zw, "meta.json", mb); err != nil {
		return meta, err
	}
	return meta, zw.Close()
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
// 当前库改名为 <db>.bak-<时间>,写入备份中的库与证书文件。证书各自的去向写进日志(见 Report)。
func ApplyPending(dbPath string) (bool, error) {
	p := PendingPath(dbPath)
	if p == "" {
		return false, nil
	}
	rep, err := Restore(dbPath, p)
	if rep.DB {
		for _, l := range rep.Lines() {
			if l.Warn {
				logger.Warning("还原备份:", strings.TrimSpace(l.Text))
			} else {
				logger.Info("还原备份:", strings.TrimSpace(l.Text))
			}
		}
	}
	if err != nil {
		os.Rename(p, p+".failed")
		return false, err
	}
	os.Remove(p)
	return true, nil
}

// pruneBaks 只保留最近 keep 份 <db>.bak-* 旧库。
func pruneBaks(dbPath string, keep int) {
	matches, _ := filepath.Glob(dbPath + ".bak-*")
	sort.Sort(sort.Reverse(sort.StringSlice(matches))) // 文件名带时间,倒序就是新的在前
	for i := keep; i < len(matches); i++ {
		os.Remove(matches[i])
	}
}

// readZipFile 读出 zip 里的一个条目。
func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	b, readErr := io.ReadAll(rc)
	if closeErr := rc.Close(); readErr == nil {
		readErr = closeErr
	}
	return b, readErr
}

// underDir 判断 p 是否落在 root 目录内(拒绝绝对路径越界与 ..)。
func underDir(root, p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	return abs == root || strings.HasPrefix(abs, root+string(os.PathSeparator))
}

// Restore 直接把备份应用到 dbPath(要求该库当前未被打开),返回这次还原做了什么。
// 证书的规则见 certs.go:数据目录外的不往外写,本机能用的接着用,不能用的放进 <数据目录>/cert/restored/ 并改指。
// 出错时报告里照样有已经做了的部分:DB 为真说明数据库已经换成备份里的了。
func Restore(dbPath, srcPath string) (Report, error) {
	var rep Report
	dbBytes, meta, isZip, err := extract(srcPath)
	if err != nil {
		return rep, err
	}
	bak := ""
	if _, err := os.Stat(dbPath); err == nil {
		bak = dbPath + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.Rename(dbPath, bak); err != nil {
			return rep, fmt.Errorf("备份当前库: %w", err)
		}
		pruneBaks(dbPath, 2) // 每次还原都留一份旧库,只保留最近两份,别越积越多
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		os.Remove(dbPath + suffix)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return rep, err
	}
	if err := os.WriteFile(dbPath, dbBytes, 0o600); err != nil {
		return rep, err
	}
	rep.DB, rep.Bak = true, bak
	// 走到这里数据库**已经**换成备份里的了。之后任何一个证书写失败,都不能再说"还原失败、继续用当前库":
	// 0.6.10 在第一个出错的证书上直接 return,ApplyPending 把包改名 .failed,日志却说"继续用当前库" ——
	// 管理员信了、再传一次再还原,每次还原都生成一份 .bak 而 pruneBaks 只留两份,试到第三次原库就没了。
	// 这里把能写的证书都写完,最后把实情报出来:库已还原、原库在哪、哪些证书没写成。
	db, dbErr := database.OpenNoMigrate(dbPath)
	if dbErr == nil {
		defer database.Close(db)
	}
	root, _ := filepath.Abs(filepath.Dir(dbPath))
	if isZip {
		if entries, err := zipCerts(srcPath, root, meta, &rep); err != nil {
			rep.Failed = append(rep.Failed, CertNote{Path: srcPath, Reason: err.Error()})
		} else {
			restoreCerts(db, dbErr, root, entries, &rep)
		}
	}
	if dbErr == nil {
		rep.Missing = missingCerts(db, meta.CertsUnreadable)
	}
	// 放进数据目录的证书副本、还缺的、没写成的记在数据目录里,面板顶部提示一次(面板上传的还原以前只进启动日志)
	if err := saveNotice(root, rep.notice(time.Now())); err != nil {
		logger.Warning("记下还原结果失败(面板顶部不会提示): ", err)
	}
	if len(rep.Failed) > 0 {
		where := "(之前没有数据库)"
		if bak != "" {
			where = "原库在 " + bak
		}
		var list []string
		for _, f := range rep.Failed {
			list = append(list, f.Path+": "+f.Reason)
		}
		return rep, fmt.Errorf("数据库已从备份还原%s,但 %d 个证书没写成: %s", where, len(rep.Failed), strings.Join(list, "; "))
	}
	return rep, nil
}

// zipCerts 读出备份 zip 里的证书文件。证书路径来自 zip 里的 meta.json,是不可信输入:否则一个伪造的备份
// 就能以 root 身份往任意位置写文件(systemd 单元、authorized_keys…)。所以它只用来判断在不在数据目录 root 里;
// 数据目录外的只决定放进 cert/restored/ 时叫什么,绝不往原路径写。
func zipCerts(srcPath, root string, meta Meta, rep *Report) ([]certEntry, error) {
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("重新读取备份 zip: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("读取备份 zip: %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	var out []certEntry
	seen := map[string]bool{}
	for _, c := range meta.Certs {
		f := files[c.Zip]
		if f == nil || c.Path == "" {
			name := c.Path
			if name == "" {
				name = c.Zip
			}
			rep.Failed = append(rep.Failed, CertNote{Path: name, Reason: "备份里缺这一条"})
			continue
		}
		if seen[c.Path] {
			continue
		}
		seen[c.Path] = true
		b, err := readZipFile(f)
		if err != nil {
			rep.Failed = append(rep.Failed, CertNote{Path: c.Path, Reason: err.Error()})
			continue
		}
		e := certEntry{path: c.Path, data: b, inRoot: underDir(root, c.Path)}
		if e.inRoot {
			if st, err := os.Lstat(c.Path); err == nil && st.Mode()&os.ModeSymlink != 0 {
				e.link = true
			}
		}
		out = append(out, e)
	}
	return out, nil
}
