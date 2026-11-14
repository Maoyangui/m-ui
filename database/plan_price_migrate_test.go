package database_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// oldPlan 加参考价之前的套餐表(表名照旧 plans):用来造老库,也用来模拟回滚后的旧二进制。
type oldPlan struct {
	Id          uint   `gorm:"primaryKey;autoIncrement"`
	ResellerId  uint   `gorm:"uniqueIndex:idx_plans_owner_name,priority:1;index"`
	Name        string `gorm:"uniqueIndex:idx_plans_owner_name,priority:2"`
	VolumeGB    float64
	Days        int
	DeviceLimit int
	SpeedUp     int
	SpeedDown   int
	AutoReset   bool
	ResetDays   int
	FirstUse    bool
	LineIds     json.RawMessage
	LineNodes   json.RawMessage
	Desc        string
	Sort        int
}

func (oldPlan) TableName() string { return "plans" }

func revision(t *testing.T, db *gorm.DB) string {
	t.Helper()
	s, err := hub.BuildSnapshot(db, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return s.Revision
}

// 老库(没有 plans.price,也没有 plan_ops)升级:列和表都补上,老套餐的参考价是 0;
// users 表没动,同一份数据升级前后的快照修订号不变(旧副机不受影响);旧二进制照样能往新库里写套餐。
func TestPlanPriceMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	var tables []interface{}
	for _, m := range model.All() {
		switch m.(type) {
		case *model.Plan, *model.PlanOp:
			continue
		}
		tables = append(tables, m)
	}
	if err := old.AutoMigrate(append(tables, &oldPlan{})...); err != nil {
		t.Fatal(err)
	}
	old.Create(&oldPlan{Name: "月付", VolumeGB: 100, Days: 30, AutoReset: true, ResetDays: 30})
	old.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	old.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})
	old.Create(&model.User{Name: "alice", Enabled: true, Volume: 100 << 30, Expiry: 1_900_000_000, PlanId: 1, Up: 5, AutoReset: true, ResetDays: 30, NextReset: 1_890_000_000})
	old.Create(&model.UserLine{UserId: 1, LineId: 1})
	if old.Migrator().HasColumn("plans", "price") || old.Migrator().HasTable("plan_ops") {
		t.Fatal("老库不该有 price 列和 plan_ops 表")
	}
	before := revision(t, old)
	if sqlDB, err := old.DB(); err == nil {
		sqlDB.Close()
	}

	db, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if !db.Migrator().HasColumn(&model.Plan{}, "price") || !db.Migrator().HasTable(&model.PlanOp{}) {
		t.Fatal("升级后要有 price 列和 plan_ops 表")
	}
	var p model.Plan
	if err := db.Where("name = ?", "月付").First(&p).Error; err != nil || p.Price != 0 || p.VolumeGB != 100 {
		t.Fatalf("老套餐原样保留,参考价为 0: %+v %v", p, err)
	}
	if after := revision(t, db); after != before {
		t.Fatalf("升级前后快照修订号变了: %s → %s", before, after)
	}

	// 回滚到旧二进制:它不知道 price,照样能建套餐(列有默认值)、按白名单改套餐
	if err := db.Create(&oldPlan{Name: "季付", VolumeGB: 300, Days: 90}).Error; err != nil {
		t.Fatalf("旧结构写新库: %v", err)
	}
	var q model.Plan
	db.Where("name = ?", "季付").First(&q)
	if q.Price != 0 {
		t.Fatalf("旧二进制建的套餐参考价应为 0: %v", q.Price)
	}
	db.Model(&model.Plan{}).Where("id = ?", p.Id).Update("price", 30)
	if err := db.Model(&oldPlan{}).Where("id = ?", p.Id).Select("name", "volume_gb").Updates(oldPlan{Name: "月付", VolumeGB: 200}).Error; err != nil {
		t.Fatal(err)
	}
	db.First(&p, p.Id)
	if p.Price != 30 || p.VolumeGB != 200 {
		t.Fatalf("旧版按白名单改套餐不能动参考价: %+v", p)
	}

	// plan_ops 的唯一键:没给键(NULL)的可以有多条;同一作用域同一个键被拒;不同作用域同一个键可以
	key := "order-1"
	for i, op := range []model.PlanOp{{UserId: 1, Op: "renew"}, {UserId: 1, Op: "renew"}, {UserId: 1, Op: "stack", RequestId: &key}, {ResellerId: 7, UserId: 1, Op: "stack", RequestId: &key}} {
		if err := db.Create(&op).Error; err != nil {
			t.Fatalf("第 %d 条应能插入: %v", i+1, err)
		}
	}
	if err := db.Create(&model.PlanOp{UserId: 1, Op: "stack", RequestId: &key}).Error; err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("同一作用域重复的键应被唯一索引拒绝: %v", err)
	}
}
