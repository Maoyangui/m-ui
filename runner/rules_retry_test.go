package runner

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/rules"
)

// 规则状态已落库、下发限速时读库失败:要记下待重试。以前错误被吞掉,下一轮判定认为一致不再下发,
// 这条限速在整个惩罚期都不生效(审计 MB19)。
func TestRulesNowRetriesWhenLimitsFail(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	r := &Runner{db: db, core: core.NewCore(), dbPath: filepath.Join(dir, "x.db"),
		rules: &rules.Engine{DB: db, Location: func() *time.Location { return time.UTC }}}
	defer r.core.Stop()
	if err := r.reloadAllLocked([]byte(`{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"direct"}]}`)); err != nil {
		t.Fatal(err)
	}
	db.Create(&model.User{Name: "u", Enabled: true, Credentials: []byte(`{}`)})
	db.Create(&model.Rule{Name: "全天", Enabled: true, Kind: rules.KindSchedule, AllUsers: true, Start: "00:00", End: "00:00", DownMbps: 10})
	if err := db.Exec("DROP TABLE resellers").Error; err != nil { // 规则判定用不到它,读限速策略要读
		t.Fatal(err)
	}
	r.RulesNow()
	var n int64
	db.Model(&model.LimitState{}).Count(&n)
	if n != 1 {
		t.Fatalf("前提:规则状态已落库,实际 %d 条", n)
	}
	if !r.limitsPending.Load() {
		t.Fatal("下发失败应记下待重试")
	}
}
