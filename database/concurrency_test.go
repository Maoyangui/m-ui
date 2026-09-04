package database

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// 面板里定时任务、同步、订阅日志会同时写库。SQLite 只允许一个写事务,
// 多连接下"事务里先读后写"会撞成升级死锁并直接抛 database is locked。
// 这里模拟这种并发,要求一条都不能失败。
func TestConcurrentWritesDoNotLock(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer Close(db)
	db.Create(&model.User{Name: "u", Enabled: true})

	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				err := db.Transaction(func(tx *gorm.DB) error { // 先读后写:最容易触发升级死锁
					var us []model.User
					if err := tx.Where("enabled = ?", true).Find(&us).Error; err != nil {
						return err
					}
					return tx.Model(&model.User{}).Where("name = ?", "u").
						Update("up", gorm.Expr("up + 1")).Error
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发写不该失败: %v", err)
	}
	var u model.User
	db.First(&u, 1)
	if u.Up != 200 {
		t.Fatalf("应累加 200 次,实际 %d", u.Up)
	}
}
