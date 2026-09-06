package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// oldDB 造一个最小的旧面板库:一条 ss 入站、direct 出站、两个用户(一个停用)、一个管理员。
func oldDB(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "old.db")
	db, err := gorm.Open(sqlite.Open(p), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE outbounds (id INTEGER PRIMARY KEY, type TEXT, tag TEXT, options TEXT)`,
		`CREATE TABLE inbounds (id INTEGER PRIMARY KEY, type TEXT, tag TEXT, options TEXT, addrs TEXT)`,
		`CREATE TABLE settings (key TEXT, value TEXT)`,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, password TEXT)`,
		`CREATE TABLE clients (id INTEGER PRIMARY KEY, enable INTEGER, name TEXT, config TEXT, inbounds TEXT, volume INTEGER, expiry INTEGER,
			up INTEGER, down INTEGER, total_up INTEGER, total_down INTEGER, "desc" TEXT, remark TEXT, created_at INTEGER, online_at INTEGER,
			auto_reset INTEGER, reset_days INTEGER, next_reset INTEGER)`,
		`INSERT INTO outbounds VALUES (1,'direct','direct','{}')`,
		`INSERT INTO inbounds VALUES (1,'shadowsocks','ss','{"listen":"::","listen_port":8388,"method":"aes-256-gcm"}','')`,
		`INSERT INTO settings VALUES ('config','{"route":{"rules":[{"action":"route","inbound":["ss"],"outbound":"direct"}]}}')`,
		`INSERT INTO settings VALUES ('webDomain','example.com')`,
		`INSERT INTO users VALUES (1,'admin','$2a$10$abcdefghijklmnopqrstuv')`,
		`INSERT INTO clients VALUES (1,1,'alive','{"shadowsocks":{"name":"alive","password":"p1"}}','[1]',0,0,10,20,0,0,'','',0,0,0,0,0)`,
		`INSERT INTO clients VALUES (2,0,'paused','{"shadowsocks":{"name":"paused","password":"p2"}}','[1]',1024,0,30,40,0,0,'','',0,0,0,0,0)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	sqlDB, _ := db.DB()
	sqlDB.Close()
	return p
}

func TestRunKeepsDisabledUsers(t *testing.T) {
	dir := t.TempDir()
	from := oldDB(t, dir)
	to := filepath.Join(dir, "m-ui.db")
	if err := Run(from, to, "", "", false); err != nil {
		t.Fatalf("整机导入失败: %v", err)
	}
	db, err := database.Open(to)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	var users []model.User
	db.Order("id asc").Find(&users)
	if len(users) != 2 {
		t.Fatalf("应导入 2 个用户,得到 %d", len(users))
	}
	if !users[0].Enabled || users[0].Name != "alive" {
		t.Fatalf("启用的用户不对: %+v", users[0])
	}
	if users[1].Enabled || users[1].Name != "paused" {
		t.Fatalf("旧库里停用的用户导入后必须仍是停用: %+v", users[1])
	}
	var n int64
	db.Model(&model.UserLine{}).Count(&n)
	if n != 2 {
		t.Fatalf("用户线路关系应为 2,得到 %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "m-ui-report.md")); err != nil {
		t.Fatalf("应生成导入报告: %v", err)
	}
}

func TestImportUsersOnlyKeepsDisabled(t *testing.T) {
	dir := t.TempDir()
	from := oldDB(t, dir)
	db, err := database.Open(filepath.Join(dir, "cur.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Line{Name: "l1", Protocol: "shadowsocks", Port: 30001, Enabled: true})
	sum, err := ImportUsersOnly(from, db, true)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Created != 2 || sum.Assigned != 2 {
		t.Fatalf("汇总不对: %+v", sum)
	}
	var u model.User
	db.Where("name = ?", "paused").First(&u)
	if u.Enabled {
		t.Fatal("只搬用户时停用的用户也必须保持停用")
	}
}
