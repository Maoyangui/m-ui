package stats

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func TestSeriesAggregatesInSQL(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	now := time.Now().Unix()
	// 最近两小时:每分钟一行下行 100、上行 10;另一个用户不该混进来
	for i := int64(1); i <= 120; i++ {
		ts := now - i*60
		db.Create(&model.Stats{DateTime: ts, Resource: "user", Tag: "alice", Direction: false, Traffic: 100})
		db.Create(&model.Stats{DateTime: ts, Resource: "user", Tag: "alice", Direction: true, Traffic: 10})
		db.Create(&model.Stats{DateTime: ts, Resource: "user", Tag: "bob", Direction: false, Traffic: 999})
	}
	res := Series(db, "user", "alice", 6, 1800, 60, time.UTC)
	if res.TotalDown != 12000 || res.TotalUp != 1200 {
		t.Fatalf("总量不对: %+v", res)
	}
	var sum int64
	for _, p := range res.Points {
		sum += p.Down
	}
	if sum != 12000 {
		t.Fatalf("各桶之和应等于总量: %d", sum)
	}
	all := Series(db, "user", "", 6, 1800, 60, time.UTC)
	if all.TotalDown != 12000+120*999 {
		t.Fatalf("tag 为空应合计全部用户: %+v", all)
	}
}

// 对齐到桶边界后,正好落在起点上的样本属于第 0 根柱:48 小时前的数据已并成整点,7 天视图首柱以前会少一整小时(审计 MB17)。
func TestSeriesKeepsSampleAtAlignedStart(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	probe := Series(db, "user", "alice", 24*7, 6*3600, 60, time.UTC)
	db.Create(&model.Stats{DateTime: probe.Start, Resource: "user", Tag: "alice", Direction: true, Traffic: 1000})
	db.Create(&model.Stats{DateTime: probe.Start + 3600, Resource: "user", Tag: "alice", Direction: true, Traffic: 1})
	res := Series(db, "user", "alice", 24*7, 6*3600, 60, time.UTC)
	if res.Start != probe.Start || res.TotalUp != 1001 || res.Points[0].Up != 1001 {
		t.Fatalf("起点那一桶的样本应计入首柱: start=%d total=%d first=%d", res.Start, res.TotalUp, res.Points[0].Up)
	}
}
