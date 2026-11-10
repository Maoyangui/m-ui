package ha

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// seedMaster 一个有各种数据的主机库。
func seedMaster(t *testing.T, s *Service) {
	t.Helper()
	db := s.d.DB
	now := time.Now().Unix()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Create(&model.Node{Name: "HK", IsLocal: true, Enabled: true, Sort: 1, PublicIP: "1.2.3.4"}).Error)
	must(db.Create(&model.Node{Name: "TW", ApiUrl: "https://tw.example.com:2053/app/", Token: "tw-token", Enabled: true, Sort: 2}).Error)
	must(db.Create(&model.Admin{Username: "boss", Password: "$2a$10$hash"}).Error)
	must(db.Create(&model.Reseller{Name: "agent", Password: "$2a$pw", TotpSecret: "TOTP", ApiToken: "rs-api"}).Error)
	off := model.User{Name: "off", Credentials: json.RawMessage(`{"ss":"x"}`), Up: 7, Down: 8}
	must(db.Create(&off).Error)
	must(db.Model(&model.User{}).Where("id = ?", off.Id).Update("enabled", false).Error)
	must(db.Create(&model.User{Name: "alice", Enabled: true, Credentials: json.RawMessage(`{"ss":"y"}`), Up: 100, Down: 200}).Error)
	must(db.Create(&model.Session{Token: strings.Repeat("a", 64), User: "boss", Exp: now + 3600}).Error)
	must(db.Create(&model.Stats{DateTime: now, Resource: "user", Tag: "alice", Traffic: 5}).Error)
	must(db.Create(&model.SubLog{Ts: now, User: "alice"}).Error)
	must(db.Create(&model.AgentCounter{UserName: "alice", Up: 1, Down: 1}).Error)
	must(db.Create(&model.TrafficCursor{NodeId: 2, UserName: "alice", Up: 50, Down: 60}).Error)
	must(db.Create(&model.Change{DateTime: now - 30*86400, Actor: "old", Key: "k", Action: "a"}).Error)
	must(db.Create(&model.Change{DateTime: now, Actor: "new", Key: "k", Action: "a"}).Error)
	for k, v := range map[string]string{"webPort": "2053", "webDomain": "hk.example.com", "subDomain": "sub.example.com", "apiToken": "api-secret",
		"totpSecret": "TOTPMAIN", "tgToken": "tg", "ledgerEpoch:2": "abc", KeySignKey: "should-not-leak", "hubPushSequence": "5"} {
		must(s.set(map[string]string{k: v}))
	}
}

func openReplicaDB(t *testing.T, zipPath string) (*gorm.DB, ReplicaMeta) {
	t.Helper()
	dir := t.TempDir()
	meta, err := unzipReplica(zipPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.OpenReadOnly(filepath.Join(dir, "replica.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeDB(db) })
	return db, meta
}

// 副本按列原样拷:json:"-" 的令牌与密码、停用的用户都保留;会话、时序、订阅日志、副机账本不带;审计日志只带最近 7 天;
// 本机设置不进 settings(主机身份另记在 meta),容灾的私密键一个都不带;内容不变时指纹不变,变了就变。
func TestBuildReplicaContents(t *testing.T) {
	s := testService(t, false, map[string]string{KeyEpoch: "1", KeyMode: ModeConfirm})
	seedMaster(t, s)
	zp, meta, err := s.buildReplica(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, m2 := openReplicaDB(t, zp)
	if m2.Sum != meta.Sum || meta.MasterId == 0 || meta.Epoch != 1 || meta.MaxChangeId != 2 {
		t.Fatalf("meta 不对: %+v", meta)
	}
	var tok, pw, rsPW, rsTotp, rsAPI string
	db.Raw("SELECT token FROM nodes WHERE name = 'TW'").Scan(&tok)
	db.Raw("SELECT password FROM admins WHERE username = 'boss'").Scan(&pw)
	db.Raw("SELECT password FROM resellers").Scan(&rsPW)
	db.Raw("SELECT totp_secret FROM resellers").Scan(&rsTotp)
	db.Raw("SELECT api_token FROM resellers").Scan(&rsAPI)
	if tok != "tw-token" || pw != "$2a$10$hash" || rsPW != "$2a$pw" || rsTotp != "TOTP" || rsAPI != "rs-api" {
		t.Fatalf("json:\"-\" 的字段必须原样带上: %q %q %q %q %q", tok, pw, rsPW, rsTotp, rsAPI)
	}
	var enabled bool
	db.Raw("SELECT enabled FROM users WHERE name = 'off'").Scan(&enabled)
	if enabled {
		t.Fatal("停用的用户在副本里不能变成启用")
	}
	var up int64
	db.Raw("SELECT up FROM users WHERE name = 'alice'").Scan(&up)
	if up != 100 {
		t.Fatalf("用户用量要带上: %d", up)
	}
	for _, tbl := range []string{"sessions", "stats", "sub_logs", "agent_counters"} {
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM " + tbl).Scan(&n).Error; err != nil {
			t.Fatalf("%s 表要建出来(空的): %v", tbl, err)
		}
		if n != 0 {
			t.Fatalf("%s 不该带进副本,有 %d 行", tbl, n)
		}
	}
	var cur int64
	db.Raw("SELECT up FROM traffic_cursors WHERE node_id = 2").Scan(&cur)
	if cur != 50 {
		t.Fatal("并账游标要带上(接管后靠它把副机流量补回来)")
	}
	var changes []model.Change
	db.Raw("SELECT * FROM changes").Scan(&changes)
	if len(changes) != 1 || changes[0].Actor != "new" {
		t.Fatalf("审计日志只带最近 7 天: %+v", changes)
	}
	get := func(k string) (string, bool) {
		var rows []model.Setting
		db.Raw("SELECT * FROM settings WHERE key = ?", k).Scan(&rows)
		if len(rows) == 0 {
			return "", false
		}
		return rows[0].Value, true
	}
	for _, k := range []string{"webPort", "webDomain", "subDomain", KeySignKey, KeyEpoch, "hubPushSequence"} {
		if _, ok := get(k); ok {
			t.Errorf("本机设置 %s 不该在副本的 settings 里", k)
		}
	}
	for k, want := range map[string]string{"apiToken": "api-secret", "totpSecret": "TOTPMAIN", "tgToken": "tg", "ledgerEpoch:2": "abc", KeyMode: ModeConfirm} {
		if v, ok := get(k); !ok || v != want {
			t.Errorf("全局设置 %s 要带上: %q", k, v)
		}
	}
	if meta.Identity.SubHost != "sub.example.com" || meta.Identity.MasterName != "HK" || meta.Identity.Self.Token == "" {
		t.Fatalf("主机身份要进 meta(订阅域名、作为副机时怎么连): %+v", meta.Identity)
	}
	// 指纹:没改动 → 一样;改一个用户 → 不一样
	_, again, err := s.buildReplica(t.TempDir())
	if err != nil || again.Sum != meta.Sum {
		t.Fatalf("内容没变时指纹要一样: %v %s %s", err, again.Sum, meta.Sum)
	}
	s.d.DB.Model(&model.User{}).Where("name = ?", "alice").Update("up", 101)
	_, changed, _ := s.buildReplica(t.TempDir())
	if changed.Sum == meta.Sum {
		t.Fatal("内容变了指纹要变")
	}
}

// 一致切面:生成副本的同时不停地在一笔事务里同时改用户用量和游标,副本里两者永远对得上(读到的是同一时刻的一份),
// 而且写入不被生成副本挡住。
func TestBuildReplicaConsistentSnapshot(t *testing.T) {
	s := testService(t, false, map[string]string{KeyEpoch: "1", KeyMode: ModeConfirm})
	db := s.d.DB
	db.Create(&model.Node{Name: "HK", IsLocal: true, Enabled: true})
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("u%03d", i)
		db.Create(&model.User{Name: name, Enabled: true, Credentials: json.RawMessage(`{}`)})
		db.Create(&model.TrafficCursor{NodeId: 7, UserName: name})
	}
	var stop atomic.Bool
	var writes atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := int64(1); !stop.Load(); v++ {
			err := db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Model(&model.User{}).Where("1 = 1").Update("up", v).Error; err != nil {
					return err
				}
				return tx.Model(&model.TrafficCursor{}).Where("1 = 1").Update("up", v).Error
			})
			if err != nil {
				t.Errorf("写入被挡住了: %v", err)
				return
			}
			writes.Add(1)
		}
	}()
	for i := 0; i < 5; i++ {
		zp, _, err := s.buildReplica(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		rdb, _ := openReplicaDB(t, zp)
		var bad int64
		rdb.Raw("SELECT COUNT(*) FROM users u JOIN traffic_cursors c ON c.user_name = u.name WHERE u.up <> c.up").Scan(&bad)
		var distinct int64
		rdb.Raw("SELECT COUNT(DISTINCT up) FROM users").Scan(&distinct)
		if bad != 0 || distinct != 1 {
			t.Fatalf("副本不是同一时刻的一份:%d 个用户和游标对不上,用量有 %d 种值", bad, distinct)
		}
	}
	stop.Store(true)
	wg.Wait()
	if writes.Load() < 5 {
		t.Fatalf("生成副本期间写入几乎没进行: %d 笔", writes.Load())
	}
}

// 副本带上订阅用的证书(主机身份):证书库里的那一行一起记下;没有订阅证书时不带。
func TestBuildReplicaSubCert(t *testing.T) {
	s := testService(t, false, map[string]string{KeyEpoch: "1", KeyMode: ModeConfirm})
	s.d.DB.Create(&model.Node{Name: "HK", IsLocal: true, Enabled: true})
	_, meta, err := s.buildReplica(t.TempDir())
	if err != nil || meta.Identity.SubCert != nil {
		t.Fatalf("没有订阅证书时不带: %v %+v", err, meta.Identity.SubCert)
	}
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "sub.crt"), filepath.Join(dir, "sub.key")
	os.WriteFile(crt, []byte("CERT"), 0o600)
	os.WriteFile(key, []byte("KEY"), 0o600)
	s.set(map[string]string{"subCertFile": crt, "subKeyFile": key, "subDomain": "sub.example.com", "acmeCfToken": "cf"})
	s.d.DB.Create(&model.Cert{Name: "sub.example.com", Source: "acme", Method: "cloudflare", AutoRenew: true, CertFile: crt, KeyFile: key})
	zp, meta, err := s.buildReplica(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := meta.Identity.SubCert
	if c == nil || c.Name != "sub.example.com" || c.Source != "acme" || c.Method != "cloudflare" || !c.AutoRenew || meta.Identity.AcmeCfToken != "cf" {
		t.Fatalf("订阅证书与它的续期方式要带上: %+v %q", c, meta.Identity.AcmeCfToken)
	}
	out := t.TempDir()
	if _, err := unzipReplica(zp, out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(out, filepath.FromSlash(c.CertZip)))
	k, _ := os.ReadFile(filepath.Join(out, filepath.FromSlash(c.KeyZip)))
	if !bytes.Equal(b, []byte("CERT")) || !bytes.Equal(k, []byte("KEY")) {
		t.Fatal("证书内容不对")
	}
	// 证书续期换了内容:指纹要变(要重发给备用)
	os.WriteFile(crt, []byte("CERT2"), 0o600)
	_, again, _ := s.buildReplica(t.TempDir())
	if again.Sum == meta.Sum {
		t.Fatal("订阅证书换了,副本指纹要变")
	}
}
