package database

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// 事务里再用连接池查一次(代码里到处都是"事务中顺手读个设置")必须不会卡死。
// v0.4.1 把连接池压到 1 条,这里就会自己等自己,面板整体挂起 —— 加个哨兵防复发。
func TestPoolNotStarvedInsideTransaction(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "m-ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer Close(db)

	done := make(chan error, 1)
	go func() {
		done <- db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("CREATE TABLE IF NOT EXISTS probe(id INTEGER PRIMARY KEY)").Error; err != nil {
				return err
			}
			var n int64 // 故意走 db 而不是 tx:模拟事务体里读设置
			return db.Raw("SELECT 1").Scan(&n).Error
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("事务内使用连接池失败: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("事务内向连接池借连接卡死:连接池被事务自己占满了")
	}
}
