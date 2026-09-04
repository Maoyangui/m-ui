package database

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open 打开(必要时创建)m-ui 数据库并迁移表结构。
// 使用纯 Go 的 SQLite 驱动(modernc),无 CGO:交叉编译零依赖,单二进制。
func Open(dbPath string) (*gorm.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return nil, err
	}
	sep := "?"
	if strings.Contains(dbPath, "?") {
		sep = "&"
	}
	// WAL + busy_timeout:常见取舍,读写并发下不互相饿死。
	dsn := dbPath + sep + "_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(model.All()...); err != nil {
		return nil, err
	}
	// 升级新增的列在老行里是 NULL,而 SQL 里 NULL = 0 不成立(订阅按 reseller_id 找人会落空)
	db.Exec("UPDATE users SET reseller_id = 0 WHERE reseller_id IS NULL")
	db.Exec("UPDATE users SET sub_token = '' WHERE sub_token IS NULL")
	db.Exec("UPDATE users SET share_token = '' WHERE share_token IS NULL")
	db.Exec("UPDATE resellers SET used_carried = 0 WHERE used_carried IS NULL")
	db.Exec("UPDATE resellers SET used_base = 0 WHERE used_base IS NULL")
	db.Exec("UPDATE resellers SET claim_before = 0 WHERE claim_before IS NULL")
	db.Exec("UPDATE resellers SET expiry = 0 WHERE expiry IS NULL")
	db.Exec("UPDATE resellers SET speed_up = 0 WHERE speed_up IS NULL")
	db.Exec("UPDATE resellers SET speed_down = 0 WHERE speed_down IS NULL")
	// 套餐从"全局唯一名"改成"按归属唯一",老库里那个唯一索引要去掉
	db.Exec("UPDATE plans SET reseller_id = 0 WHERE reseller_id IS NULL")
	db.Exec("DROP INDEX IF EXISTS idx_plans_name")
	return db, nil
}

// DBHandle 是测试里携带 *gorm.DB 的小包装。
type DBHandle struct{ DB *gorm.DB }

// OpenReadOnly 只读打开一个既有 SQLite 文件(用于导入旧库,绝不写源文件)。
func OpenReadOnly(dbPath string) (*gorm.DB, error) {
	dsn := "file:" + dbPath + "?mode=ro"
	return gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
}

// Checkpoint 把 WAL 中的改动全部并回主库文件并截断 WAL。
// 备份、导入、迁移之后必须调用:否则单独复制 .db 文件会丢失尚在 WAL 中的数据。
func Checkpoint(db *gorm.DB) error {
	return db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error
}

// Close 检查点后关闭底层连接,使数据库文件自洽、可安全复制。
func Close(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	if err := Checkpoint(db); err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
