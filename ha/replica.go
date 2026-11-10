package ha

import (
	"archive/zip"
	"compress/flate"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"
)

// 副本 = 主库的表级拷贝(设计 3.2):不走 JSON 序列化(Node.Token、Admin.Password、代理密钥这些 json:"-" 的字段会丢),
// 也不走 gorm 的 Create(default:true 的 bool 会把 false 写成 true)—— 按列原样复制每一行。
//
// 给备用的副本不带:会话(接任后所有人重新登录)、流量时序与订阅日志(大,每 5 分钟发一次不划算)、副机账本(主机上没有);
// 审计日志只带最近 7 天。设为主机时推过去的是整库(只不带会话)。设置只带全局的;主机身份(订阅地址、订阅证书、
// 作为副机时怎么连)单独记在 meta.json 里,见 identity.go。

// replicaSkip 给备用的副本不带内容的表(表结构照建,行不拷)。
var replicaSkip = map[string]bool{"sessions": true, "stats": true, "sub_logs": true, "agent_counters": true}

// bundleSkip 设为主机时整库数据不带的表。
var bundleSkip = map[string]bool{"sessions": true, "agent_counters": true}

// 审计日志只带最近 7 天、最多 2 万条:接任后够看,也够做分叉比对。
const (
	replicaChangeDays = 7
	replicaChangeMax  = 20000
)

// ReplicaMeta 副本(或设为主机时整库数据)zip 里的 meta.json。
type ReplicaMeta struct {
	V           int              `json:"v"`
	Version     string           `json:"version"`
	CreatedAt   int64            `json:"createdAt"`
	MasterId    uint             `json:"masterId"`
	MasterName  string           `json:"masterName"`
	Epoch       uint64           `json:"epoch"`
	MaxChangeId uint64           `json:"maxChangeId"`
	Counts      map[string]int64 `json:"counts"`
	Identity    Identity         `json:"identity"`
	Full        bool             `json:"full,omitempty"` // 整库(设为主机)
	Sum         string           `json:"sum"`            // 内容指纹:同样的数据得出同样的值(不含时间)
}

// openScratch 打开一个临时 SQLite 文件(只本进程用,不要日志、不要同步落盘)。
func openScratch(path string) (*gorm.DB, error) {
	return gorm.Open(sqlite.Open(path+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)&_pragma=busy_timeout(10000)"),
		&gorm.Config{Logger: glogger.Discard})
}

func closeDB(db *gorm.DB) {
	if db == nil {
		return
	}
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// schemaObj sqlite_master 里的一项。
type schemaObj struct {
	Type, Name, TblName, Sql string
}

// copyOpts 一次表级拷贝的选项。
type copyOpts struct {
	skip      map[string]bool              // 只建表、不拷行
	settings  func(key, value string) bool // 返回 false = 这一行设置不拷(由调用方另行处理)
	changeMin int64                        // 审计日志只拷这之后的(0 = 全拷)
	changeMax int                          // 审计日志最多拷几条(0 = 不限)
}

// copyTables 在主库的一个只读事务里把表结构与各表的行原样拷进 out(同一时刻的一份,不挡写入),
// 返回各表行数与内容指纹。
func copyTables(src *gorm.DB, out *gorm.DB, o copyOpts, h hash.Hash) (map[string]int64, uint64, error) {
	counts := map[string]int64{}
	var maxChange uint64
	outSQL, err := out.DB()
	if err != nil {
		return nil, 0, err
	}
	err = database.ReadTx(src, func(tx *gorm.DB) error {
		var objs []schemaObj
		if err := tx.Raw(`SELECT type, name, tbl_name, sql FROM sqlite_master
			WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' AND type IN ('table', 'index')
			ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END, name`).Scan(&objs).Error; err != nil {
			return fmt.Errorf("读表结构: %w", err)
		}
		for _, o := range objs {
			if _, err := outSQL.Exec(o.Sql); err != nil {
				return fmt.Errorf("建 %s: %w", o.Name, err)
			}
		}
		if err := tx.Raw("SELECT COALESCE(MAX(id), 0) FROM changes").Scan(&maxChange).Error; err != nil {
			return err
		}
		for _, ob := range objs {
			if ob.Type != "table" || o.skip[ob.Name] {
				continue
			}
			q := "SELECT * FROM " + quoteIdent(ob.Name) + " ORDER BY rowid"
			var args []interface{}
			if ob.Name == "changes" && (o.changeMin > 0 || o.changeMax > 0) {
				q = "SELECT * FROM changes WHERE date_time >= ? ORDER BY id DESC"
				args = append(args, o.changeMin)
				if o.changeMax > 0 {
					q += fmt.Sprintf(" LIMIT %d", o.changeMax)
				}
			}
			n, err := copyRows(tx, outSQL, ob.Name, q, args, o, h)
			if err != nil {
				return fmt.Errorf("拷贝 %s: %w", ob.Name, err)
			}
			counts[ob.Name] = n
		}
		// 自增序列照原库的:删过的最大 id 不会在接任后被新行重新用掉
		var seqs []struct {
			Name string
			Seq  int64
		}
		if tx.Raw("SELECT name, seq FROM sqlite_sequence").Scan(&seqs).Error == nil {
			for _, sq := range seqs {
				if res, err := outSQL.Exec("UPDATE sqlite_sequence SET seq = ? WHERE name = ? AND seq < ?", sq.Seq, sq.Name, sq.Seq); err == nil {
					if n, _ := res.RowsAffected(); n == 0 {
						_, _ = outSQL.Exec("INSERT INTO sqlite_sequence (name, seq) SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = ?)", sq.Name, sq.Seq, sq.Name)
					}
				}
			}
		}
		return nil
	})
	return counts, maxChange, err
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// copyRows 按列原样拷一张表的行(值的类型照 SQLite 的存储类型,不经 gorm 模型)。
func copyRows(tx *gorm.DB, out *sql.DB, table, q string, args []interface{}, o copyOpts, h hash.Hash) (int64, error) {
	rows, err := tx.Raw(q, args...).Rows()
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	keyAt, valAt := -1, -1
	if table == "settings" {
		for i, c := range cols {
			switch c {
			case "key":
				keyAt = i
			case "value":
				valAt = i
			}
		}
	}
	qc := make([]string, len(cols))
	marks := make([]string, len(cols))
	for i, c := range cols {
		qc[i], marks[i] = quoteIdent(c), "?"
	}
	otx, err := out.Begin()
	if err != nil {
		return 0, err
	}
	defer otx.Rollback()
	stmt, err := otx.Prepare("INSERT INTO " + quoteIdent(table) + " (" + strings.Join(qc, ",") + ") VALUES (" + strings.Join(marks, ",") + ")")
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	vals := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if h != nil {
		h.Write([]byte("\x00table:" + table + "\x00"))
	}
	var n int64
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		if keyAt >= 0 && valAt >= 0 && o.settings != nil && !o.settings(asText(vals[keyAt]), asText(vals[valAt])) {
			continue
		}
		if _, err := stmt.Exec(vals...); err != nil {
			return n, err
		}
		if h != nil {
			hashRow(h, vals)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	return n, otx.Commit()
}

func asText(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

// hashRow 把一行喂进内容指纹:每个值带类型标记与长度,不会把 "1" 和 1、"ab"+"c" 和 "a"+"bc" 算成一样。
func hashRow(h hash.Hash, vals []interface{}) {
	var buf [9]byte
	for _, v := range vals {
		switch x := v.(type) {
		case nil:
			h.Write([]byte{0})
		case int64:
			buf[0] = 1
			binary.BigEndian.PutUint64(buf[1:], uint64(x))
			h.Write(buf[:])
		case float64:
			buf[0] = 2
			binary.BigEndian.PutUint64(buf[1:], math.Float64bits(x))
			h.Write(buf[:])
		case bool:
			buf[0] = 3
			if x {
				buf[1] = 1
			} else {
				buf[1] = 0
			}
			h.Write(buf[:2])
		case string:
			hashBytes(h, 4, []byte(x))
		case []byte:
			hashBytes(h, 5, x)
		case time.Time:
			hashBytes(h, 6, []byte(x.UTC().Format(time.RFC3339Nano)))
		default:
			hashBytes(h, 7, []byte(fmt.Sprint(x)))
		}
	}
	h.Write([]byte{0xff})
}

func hashBytes(h hash.Hash, tag byte, b []byte) {
	var head [9]byte
	head[0] = tag
	binary.BigEndian.PutUint64(head[1:], uint64(len(b)))
	h.Write(head[:])
	h.Write(b)
}

// zipWriter 写 zip 用最快的压缩档:库文件 gzip 一档就能压到四分之一,再往上省不了几个字节、多花好几倍时间。
func zipWriter(w io.Writer) *zip.Writer {
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	return zw
}

func zipAddFile(zw *zip.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Unix(0, 0)})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

func zipAddBytes(zw *zip.Writer, name string, b []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Unix(0, 0)})
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// buildReplica 在 dir 下生成一份给备用的副本(明文 zip),返回 zip 路径与 meta。
func (s *Service) buildReplica(dir string) (string, ReplicaMeta, error) {
	return s.buildArchive(dir, false)
}

// buildArchive 生成副本(full=false)或设为主机时的整库数据(full=true)。
func (s *Service) buildArchive(dir string, full bool) (string, ReplicaMeta, error) {
	meta := ReplicaMeta{V: 1, Version: s.d.Version, CreatedAt: s.now().Unix(), MasterId: s.SelfId(), MasterName: s.selfName(),
		Epoch: s.Epoch(), Full: full}
	dbPath := filepath.Join(dir, "replica.db")
	out, err := openScratch(dbPath)
	if err != nil {
		return "", meta, err
	}
	h := sha256.New()
	opts := copyOpts{
		skip:     replicaSkip,
		settings: func(k, v string) bool { return !IsLocal(k) },
	}
	if full {
		opts.skip = bundleSkip
	} else {
		opts.changeMin = s.now().Add(-replicaChangeDays * 24 * time.Hour).Unix()
		opts.changeMax = replicaChangeMax
	}
	counts, maxChange, err := copyTables(s.d.DB, out, opts, h)
	closeDB(out)
	if err != nil {
		return "", meta, err
	}
	meta.Counts, meta.MaxChangeId = counts, maxChange
	zipPath := filepath.Join(dir, "replica.zip")
	zf, err := os.OpenFile(zipPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", meta, err
	}
	defer zf.Close()
	zw := zipWriter(zf)
	if err := zipAddFile(zw, "replica.db", dbPath); err != nil {
		return "", meta, err
	}
	if meta.Identity, err = s.captureIdentity(zw); err != nil {
		return "", meta, err
	}
	// 身份进内容指纹(证书文件内容也算:续期换了证书要重发)
	idb, _ := json.Marshal(meta.Identity)
	h.Write([]byte("\x00identity\x00"))
	hashBytes(h, 4, idb)
	if c := meta.Identity.SubCert; c != nil {
		for _, p := range []string{s.setting("subCertFile"), s.setting("subKeyFile")} {
			if b, err := os.ReadFile(p); err == nil {
				hashBytes(h, 5, b)
			}
		}
	}
	meta.Sum = hex.EncodeToString(h.Sum(nil))
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := zipAddBytes(zw, "meta.json", mb); err != nil {
		return "", meta, err
	}
	if err := zw.Close(); err != nil {
		return "", meta, err
	}
	if err := zf.Sync(); err != nil {
		return "", meta, err
	}
	_ = os.Remove(dbPath)
	return zipPath, meta, nil
}

// unzipReplica 把副本 zip 解到 dir:replica.db、meta.json、identity/*。返回 meta。
func unzipReplica(zipPath, dir string) (ReplicaMeta, error) {
	var meta ReplicaMeta
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return meta, fmt.Errorf("副本不是 zip: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if name != "replica.db" && name != "meta.json" && !(strings.HasPrefix(name, "identity/") && !strings.Contains(name[9:], "/") && !strings.Contains(name, "..")) {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return meta, err
		}
		rc, err := f.Open()
		if err != nil {
			return meta, err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			rc.Close()
			return meta, err
		}
		_, err = io.Copy(out, io.LimitReader(rc, 64<<30))
		rc.Close()
		out.Close()
		if err != nil {
			return meta, err
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return meta, errors.New("副本里没有 meta.json")
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return meta, fmt.Errorf("副本 meta.json 坏了: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "replica.db")); err != nil {
		return meta, errors.New("副本里没有 replica.db")
	}
	return meta, nil
}
