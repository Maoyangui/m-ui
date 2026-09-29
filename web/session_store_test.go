package web

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"golang.org/x/crypto/bcrypt"
)

// 会话落库:换一个 Server 实例(等于进程重启)仍然认得登录态;登出后两边都失效。
func TestSessionSurvivesRestart(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	a := &Server{db: db}
	tok := a.newSession("admin")
	if !a.validSession(tok) {
		t.Fatal("刚登录的会话应有效")
	}
	b := &Server{db: db} // "重启"后的新实例:内存里什么都没有
	if !b.validSession(tok) {
		t.Fatal("重启后会话应从库里找回来")
	}
	b.delSession(tok) // 登出:缓存与库一起删
	if b.validSession(tok) {
		t.Fatal("登出后会话应失效")
	}
	c := &Server{db: db} // 再"重启"一次:库里也没有了
	if c.validSession(tok) {
		t.Fatal("登出后的会话不该再从库里找回来")
	}
}

// 库里(以及整库备份里)只有令牌的哈希:拿着库里的值当 Cookie 登不进去;老版本留下的明文令牌升级时清掉。
func TestSessionTableHoldsNoUsableToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}
	tok := s.newSession("admin")
	var rows []model.Session
	db.Find(&rows)
	if len(rows) != 1 || rows[0].Token == tok {
		t.Fatalf("库里不该存令牌本身: %+v", rows)
	}
	if (&Server{db: db}).validSession(rows[0].Token) {
		t.Fatal("库里的值不能直接当令牌用")
	}
	legacy := tok[:len(tok)-1] + "0" // 老版本存的是 96 位明文令牌
	db.Create(&model.Session{Token: legacy, User: "admin", Exp: time.Now().Add(time.Hour).Unix()})
	database.Close(db)
	db, err = database.Open(path) // 升级后第一次打开
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	var n int64
	db.Model(&model.Session{}).Where("token = ?", legacy).Count(&n)
	if n != 0 {
		t.Fatal("老版本的明文令牌应在升级时清掉")
	}
	if !(&Server{db: db}).validSession(tok) {
		t.Fatal("新会话重启后应仍然有效")
	}
}

// A password reset from the CLI opens the same database as the running panel,
// so the web process must reject the old token even though its in-memory cache
// still contains it.
func TestAdminSessionInvalidatedByPasswordChange(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	old, _ := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	db.Create(&model.Admin{Username: "admin", Password: string(old)})
	s := &Server{db: db, sessions: map[string]session{}}
	// 指纹缓存关掉:这条测的是"库里密码一变、旧令牌立刻失效"的语义;线上最多晚 credCacheTTL 几秒生效
	savedTTL := credCacheTTL
	credCacheTTL = 0
	defer func() { credCacheTTL = savedTTL }()
	tok := s.newSession("admin")
	if !s.validSession(tok) {
		t.Fatal("新建管理员会话应有效")
	}
	newHash, _ := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	if err := db.Model(&model.Admin{}).Where("username = ?", "admin").Update("password", string(newHash)).Error; err != nil {
		t.Fatal(err)
	}
	if s.validSession(tok) {
		t.Fatal("管理员密码被重置后旧会话必须失效")
	}
	if _, ok := s.getSession(tok); !ok {
		t.Fatal("凭据失效不应破坏会话记录的过期清理路径")
	}
}

func TestLegacySessionTokenRequiresLogin(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	hash, _ := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.MinCost)
	db.Create(&model.Admin{Username: "admin", Password: string(hash)})
	s := &Server{db: db, sessions: map[string]session{}}
	// Tokens issued before credential binding were 64 hex characters.
	legacy := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	s.putSession(legacy, session{user: "admin", exp: time.Now().Add(time.Hour)})
	if s.validSession(legacy) {
		t.Fatal("旧格式会话必须重新登录")
	}
}
