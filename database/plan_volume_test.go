package database

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 老库的 plans.volume_gb 是整数列。升级后要能存小数(0.1 GB),老数据、唯一索引都不能丢。
func TestPlanVolumeMigratesToFloat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"DROP TABLE plans",
		"CREATE TABLE `plans` (`id` integer PRIMARY KEY AUTOINCREMENT,`reseller_id` integer,`name` text,`volume_gb` integer,`days` integer,`device_limit` integer,`speed_up` integer,`speed_down` integer,`auto_reset` numeric,`reset_days` integer,`line_ids` text,`line_nodes` text,`desc` text,`sort` integer)",
		"CREATE UNIQUE INDEX `idx_plans_owner_name` ON `plans`(`reseller_id`,`name`)",
		"CREATE INDEX `idx_plans_reseller_id` ON `plans`(`reseller_id`)",
		"INSERT INTO plans(id,reseller_id,name,volume_gb,days) VALUES (7,0,'月付',100,30)",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	Close(db)

	db, err = Open(path)
	if err != nil {
		t.Fatalf("升级打开老库失败: %v", err)
	}
	defer Close(db)
	var p model.Plan
	if err := db.First(&p, 7).Error; err != nil {
		t.Fatalf("老套餐丢了: %v", err)
	}
	if p.VolumeGB != 100 || p.Days != 30 || p.Name != "月付" {
		t.Fatalf("老数据变了: %+v", p)
	}
	small := model.Plan{Name: "试用", VolumeGB: 0.1, Days: 1}
	if err := db.Create(&small).Error; err != nil {
		t.Fatalf("存小数失败: %v", err)
	}
	var got model.Plan
	db.First(&got, small.Id)
	if got.VolumeGB != 0.1 {
		t.Fatalf("小数没存住: %v", got.VolumeGB)
	}
	// 唯一索引还在:同一归属下名字不能重
	if err := db.Create(&model.Plan{Name: "试用"}).Error; err == nil {
		t.Fatal("唯一索引丢了:重名套餐存进去了")
	}
	var n int64
	db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_plans_reseller_id'").Scan(&n)
	if n != 1 {
		t.Fatal("普通索引丢了")
	}
}
