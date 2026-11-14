package database

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// 只读事务不拿写锁:别的事务占着写锁时照样能读(以前的事务一律 BEGIN IMMEDIATE,要排队等满 10 秒);
// 它开着的时候写者照样能提交;它自己看到的是开头那一刻的一致数据。
func TestReadTxDoesNotTakeWriteLock(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer Close(db)
	db.Create(&model.User{Name: "u", Enabled: true, Up: 1})

	// 1. 写者占着锁:只读事务不等
	held, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		_ = db.Transaction(func(tx *gorm.DB) error {
			tx.Model(&model.User{}).Where("name = ?", "u").Update("up", 2)
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	t0 := time.Now()
	var up int64
	err = ReadTx(db, func(tx *gorm.DB) error {
		return tx.Model(&model.User{}).Select("up").Where("name = ?", "u").Scan(&up).Error
	})
	d := time.Since(t0)
	close(release)
	<-finished
	if err != nil || d > time.Second {
		t.Fatalf("写锁被占时只读事务应立刻读到(用了 %v): %v", d, err)
	}
	if up != 1 {
		t.Fatalf("没提交的写不该被读到: up=%d", up)
	}

	// 2. 只读事务开着:写者照样提交,只读事务里看到的还是开头那份
	inside, wrote := make(chan struct{}), make(chan struct{})
	readDone := make(chan error, 1)
	var first, second int64
	go func() {
		readDone <- ReadTx(db, func(tx *gorm.DB) error {
			if err := tx.Model(&model.User{}).Select("up").Where("name = ?", "u").Scan(&first).Error; err != nil {
				return err
			}
			close(inside)
			<-wrote
			return tx.Model(&model.User{}).Select("up").Where("name = ?", "u").Scan(&second).Error
		})
	}()
	<-inside
	t0 = time.Now()
	err = db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&model.User{}).Where("name = ?", "u").Update("up", 3).Error
	})
	d = time.Since(t0)
	close(wrote)
	if err != nil || d > time.Second {
		t.Fatalf("只读事务开着时写者应照常提交(用了 %v): %v", d, err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if first != 2 || second != 2 {
		t.Fatalf("只读事务应看到同一份一致的数据: 先 %d 后 %d", first, second)
	}
}
