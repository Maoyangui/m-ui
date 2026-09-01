package database

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/fangjunsheng555/m-ui/database/model"

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
	// WAL + busy_timeout:与 s-ui 相同的取舍,读写并发下不互相饿死。
	dsn := dbPath + sep + "_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(model.All()...); err != nil {
		return nil, err
	}
	return db, nil
}

// OpenReadOnly 只读打开一个既有 SQLite 文件(用于导入旧库,绝不写源文件)。
func OpenReadOnly(dbPath string) (*gorm.DB, error) {
	dsn := "file:" + dbPath + "?mode=ro"
	return gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
}
