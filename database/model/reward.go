package model

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RewardCounter 流量奖励:全面板累计的真实流量(上行 + 下行的原始字节,不乘倍率),只有一行(id = 1)。
//
// 只在主机上算:主机本机的在记账时加(jobs.writeLedger),各副机的在主机并账时加(hub.mergeChunk)——
// 和用户用量走同一笔事务,所以不会漏、也不会重。面板内部的用户(线路作上游的专用凭据、占位用户)不算:
// 那份流量在入口那台已经按真正的用户记过一次。服务器上别的流量(SSH、面板自己、别的程序)根本不经过记账,进不来。
//
// 只增不减。主机容灾的副本、备份、换机迁移都带着这张表;接任或还原后比登记处记的小时,按登记处的往上接(RaiseRewardBytes)。
type RewardCounter struct {
	Id        uint  `json:"id" gorm:"primaryKey;autoIncrement:false"`
	Bytes     int64 `json:"bytes"`
	UpdatedAt int64 `json:"updatedAt" gorm:"autoUpdateTime:false"`
}

// AddRewardBytes 累计 n 个真实字节(n <= 0 不动)。在记账的事务里调。
func AddRewardBytes(tx *gorm.DB, n, now int64) error {
	if n <= 0 {
		return nil
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"bytes":      gorm.Expr("reward_counters.bytes + excluded.bytes"),
			"updated_at": now,
		}),
	}).Create(&RewardCounter{Id: 1, Bytes: n, UpdatedAt: now}).Error
}

// RewardBytes 现在累计了多少(还没有记录为 0)。
func RewardBytes(db *gorm.DB) int64 {
	var row RewardCounter
	if err := db.Where("id = ?", 1).Limit(1).Find(&row).Error; err != nil {
		return 0
	}
	return row.Bytes
}

// RaiseRewardBytes 把累计值抬到至少 to(本来就不小于它的不动):接任、还原之后按登记处记着的数往上接,不丢也不倒退。
func RaiseRewardBytes(db *gorm.DB, to, now int64) error {
	if to <= 0 {
		return nil
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"bytes":      gorm.Expr("MAX(reward_counters.bytes, excluded.bytes)"),
			"updated_at": now,
		}),
	}).Create(&RewardCounter{Id: 1, Bytes: to, UpdatedAt: now}).Error
}

// RewardCounted 这个数据面里的名字算不算流量奖励:面板内部的用户不算(见 RewardCounter)。
func RewardCounted(dataPlaneName string) bool { return Owner(dataPlaneName) != "" }
