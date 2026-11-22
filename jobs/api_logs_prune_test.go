package jobs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 外部 API 的调用日志按每把密钥自己选的天数清(没选过 = 7 天),每把最多留 APILogMaxRows 条,密钥没了的整份清。
func TestPruneAPIKeyLogs(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	now := time.Now()
	day := int64(86400)
	a := model.ApiKey{Name: "a", Token: "token-a-0123456789", Perms: "users.read", Enabled: true}             // 没选过:7 天
	b := model.ApiKey{Name: "b", Token: "token-b-0123456789", Perms: "users.read", Enabled: true, LogDays: 1} // 1 天
	db.Create(&a)
	db.Create(&b)
	add := func(key uint, ago int64) {
		db.Create(&model.ApiKeyLog{KeyId: key, At: now.Unix() - ago, Action: "ping", Status: 200})
	}
	add(a.Id, 6*day)
	add(a.Id, 8*day) // a 超过 7 天
	add(b.Id, day/2)
	add(b.Id, 2*day) // b 超过 1 天
	add(9999, 60)    // 密钥已经没了
	pruneAPIKeyLogs(db, now)
	count := func(key uint) (n int64) {
		db.Model(&model.ApiKeyLog{}).Where("key_id = ?", key).Count(&n)
		return
	}
	if count(a.Id) != 1 || count(b.Id) != 1 || count(9999) != 0 {
		t.Fatalf("按天清理不对: a=%d b=%d 孤儿=%d", count(a.Id), count(b.Id), count(9999))
	}
	// 条数上限:多出来的删最旧的
	rows := make([]model.ApiKeyLog, 0, model.APILogMaxRows+30)
	for i := 0; i < model.APILogMaxRows+30; i++ {
		rows = append(rows, model.ApiKeyLog{KeyId: a.Id, At: now.Unix() - 60, Action: "ping", Status: 200})
	}
	if err := db.CreateInBatches(&rows, 500).Error; err != nil {
		t.Fatal(err)
	}
	var newest uint
	db.Model(&model.ApiKeyLog{}).Where("key_id = ?", a.Id).Select("MAX(id)").Scan(&newest)
	pruneAPIKeyLogs(db, now)
	if n := count(a.Id); n != model.APILogMaxRows {
		t.Fatalf("每把最多留 %d 条,得 %d", model.APILogMaxRows, n)
	}
	var keepNewest uint
	db.Model(&model.ApiKeyLog{}).Where("key_id = ?", a.Id).Select("MAX(id)").Scan(&keepNewest)
	if keepNewest != newest {
		t.Fatal("超出上限时删的应是最旧的")
	}
}
