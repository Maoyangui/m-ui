package jobs

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func TestRollupStatsKeepsTotals(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	hour := int64(1_700_000_000) - 1_700_000_000%3600 // 某个整点
	// 一小时内 60 个分钟桶 + 已经存在的整点桶
	db.Create(&model.Stats{DateTime: hour, Resource: "user", Tag: "a", Direction: false, Traffic: 5})
	for i := int64(1); i < 60; i++ {
		db.Create(&model.Stats{DateTime: hour + i*60, Resource: "user", Tag: "a", Direction: false, Traffic: 1})
	}
	// 未到合并时间的行不动
	db.Create(&model.Stats{DateTime: hour + 3600*10 + 60, Resource: "user", Tag: "a", Direction: false, Traffic: 7})
	if err := RollupStats(db, hour+3600*5); err != nil {
		t.Fatal(err)
	}
	var rows []model.Stats
	db.Order("date_time asc").Find(&rows)
	if len(rows) != 2 {
		t.Fatalf("应只剩整点桶与未到期的行,实际 %d: %+v", len(rows), rows)
	}
	if rows[0].DateTime != hour || rows[0].Traffic != 64 {
		t.Fatalf("整点桶应累加为 5+59=64: %+v", rows[0])
	}
	if rows[1].Traffic != 7 {
		t.Fatalf("未到期的行应原样保留: %+v", rows[1])
	}
}
