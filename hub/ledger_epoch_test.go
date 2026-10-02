package hub

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 副机账本没有身份时,删了再加、副机从备份还原、同一台机器加两次,都会把整段历史流量重算一遍(审计 M057)。
// 账本纪元:变了只建基线;第一次报纪元时有游标(升级上来的老副机)照常计、没游标(新加 / 删了再加)只建基线;
// 同一纪元记在另一台副机上不计入。
func TestLedgerEpoch(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})
	usage := func() (int64, int64) {
		var u model.User
		db.Where("name = ?", "alice").First(&u)
		return u.Up, u.Down
	}
	ctr := func(up, down int64) []model.AgentCounter {
		return []model.AgentCounter{{UserName: "alice", Up: up, Down: down}}
	}

	// 老副机(不报纪元)攒下游标
	if _, err := ApplyCounters(db, 2, "台湾", "", ctr(100, 200), 1000, 60, 1); err != nil {
		t.Fatal(err)
	}
	// 升级后第一次报纪元:有游标,照常计增量,不丢流量
	ApplyCounters(db, 2, "台湾", "E1", ctr(150, 260), 1060, 60, 1)
	if up, down := usage(); up != 150 || down != 260 {
		t.Fatalf("升级上来的老副机第一次报纪元应照常计,实际 %d/%d", up, down)
	}
	// 副机从备份还原:计数倒退、纪元换新 —— 只建基线(以前当成回绕从 0 计,备份里的 80/90 又算一遍)
	ApplyCounters(db, 2, "台湾", "E2", ctr(80, 90), 1120, 60, 1)
	if up, down := usage(); up != 150 || down != 260 {
		t.Fatalf("纪元变了只建基线,不该计入,实际 %d/%d", up, down)
	}
	ApplyCounters(db, 2, "台湾", "E2", ctr(85, 100), 1180, 60, 1)
	if up, down := usage(); up != 155 || down != 270 {
		t.Fatalf("基线之后照常计增量,实际 %d/%d", up, down)
	}

	// 新加的副机(没游标)带着整段历史计数来:只建基线
	ApplyCounters(db, 3, "日本", "E3", ctr(5000, 5000), 1240, 60, 1)
	if up, down := usage(); up != 155 || down != 270 {
		t.Fatalf("没游标的副机第一份报告只建基线,实际 %d/%d", up, down)
	}
	ApplyCounters(db, 3, "日本", "E3", ctr(5010, 5020), 1300, 60, 1)
	if up, down := usage(); up != 165 || down != 290 {
		t.Fatalf("基线之后照常计,实际 %d/%d", up, down)
	}

	// 同一台机器又加了一次(副机 4 报的纪元和副机 3 一样):不计入,并报出是哪一台
	_, err = ApplyCounters(db, 4, "日本2", "E3", ctr(5030, 5040), 1360, 60, 1)
	var dup *DuplicateLedgerError
	if !errors.As(err, &dup) || dup.Other != 3 {
		t.Fatalf("同一纪元出现在第二台副机上应报 DuplicateLedgerError(另一台是 #3),得到 %v", err)
	}
	if up, down := usage(); up != 165 || down != 290 {
		t.Fatalf("重复的账本不该计入,实际 %d/%d", up, down)
	}

	// 没有计数的报告也记下纪元:之后第一份有流量的报告不会被误当成新账本丢掉
	ApplyCounters(db, 5, "新加坡", "E5", nil, 1420, 60, 1)
	ApplyCounters(db, 5, "新加坡", "E5", ctr(7, 9), 1480, 60, 1)
	if up, down := usage(); up != 172 || down != 299 {
		t.Fatalf("纪元在空报告里记下后,首份流量应照常计,实际 %d/%d", up, down)
	}
}
