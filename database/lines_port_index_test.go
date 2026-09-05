package database

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

func TestLinesPortIndexBecomesNonUnique(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.db")
	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟老库:把索引换成唯一索引
	db.Exec("DROP INDEX IF EXISTS idx_lines_port")
	db.Exec("CREATE UNIQUE INDEX idx_lines_port ON lines(port)")
	Close(db)
	for i := 0; i < 2; i++ { // 连开两次:第一次去掉唯一索引并建回普通索引,第二次不能再把它删掉
		db, err = Open(p)
		if err != nil {
			t.Fatal(err)
		}
		var sql string
		db.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_lines_port'").Scan(&sql)
		if sql == "" || contains(sql, "UNIQUE") {
			t.Fatalf("第 %d 次打开后 idx_lines_port 应是普通索引,实际 %q", i+1, sql)
		}
		db.Create(&model.Line{Name: "a" + string(rune('0'+i)), Protocol: "hysteria2", Port: 443})
		Close(db)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (func() bool { for i := 0; i+len(sub) <= len(s); i++ { if s[i:i+len(sub)] == sub { return true } }; return false })() }
