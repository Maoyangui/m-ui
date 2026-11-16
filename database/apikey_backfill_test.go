package database

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 来源 IP 白名单改成按密钥存:旧版建的密钥(新列是 NULL)在启动时各抄一份它所在面板旧的白名单,
// 新版写过的行不动;反复启动结果不变;回退期间旧版新建的密钥,再升上来时一样补上。
func TestBackfillApiKeyIPs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rsOn := model.Reseller{Name: "on", Enabled: true, ApiIpAllow: true, ApiIpList: "203.0.113.41"}
	rsOff := model.Reseller{Name: "off", Enabled: true}
	for _, r := range []*model.Reseller{&rsOn, &rsOff} {
		if err := db.Create(r).Error; err != nil {
			t.Fatal(err)
		}
	}
	db.Create(&model.Setting{Key: "apiIpAllow", Value: "true"})
	db.Create(&model.Setting{Key: "apiIpList", Value: "198.51.100.7, 192.0.2.0/24"})
	// 旧版建的:主面板一把、开了白名单的代理一把、没开的代理一把
	old := func(name string, rid uint) uint {
		k := model.ApiKey{Name: name, ResellerId: rid, Token: "tok-" + name, Perms: "users.read", Enabled: true}
		if err := db.Create(&k).Error; err != nil {
			t.Fatal(err)
		}
		db.Exec("UPDATE api_keys SET is_default = NULL, ip_allow = NULL, ip_list = NULL WHERE id = ?", k.Id)
		return k.Id
	}
	master, on, off := old("master", 0), old("rs-on", rsOn.Id), old("rs-off", rsOff.Id)
	// 新版建的:自己的名单 / 明确不限
	own := model.ApiKey{Name: "own", Token: "tok-own", Perms: "users.read", Enabled: true, IpAllow: true, IpList: "10.0.0.1"}
	open := model.ApiKey{Name: "open", Token: "tok-open", Perms: "users.read", Enabled: true}
	db.Create(&own)
	db.Create(&open)
	if err := Close(db); err != nil {
		t.Fatal(err)
	}

	check := func(round string) {
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer Close(db)
		for _, c := range []struct {
			id    uint
			allow bool
			list  string
		}{
			{master, true, "198.51.100.7, 192.0.2.0/24"}, {on, true, "203.0.113.41"}, {off, false, ""},
			{own.Id, true, "10.0.0.1"}, {open.Id, false, ""},
		} {
			var k model.ApiKey
			db.First(&k, c.id)
			if k.IpAllow != c.allow || k.IpList != c.list || k.IsDefault {
				t.Fatalf("%s:密钥 %s 应为 allow=%v list=%q,得 %+v", round, k.Name, c.allow, c.list, k)
			}
		}
		var nulls int64
		db.Raw("SELECT COUNT(*) FROM api_keys WHERE ip_list IS NULL OR ip_allow IS NULL OR is_default IS NULL").Scan(&nulls)
		if nulls != 0 {
			t.Fatalf("%s:还有 %d 行留着 NULL", round, nulls)
		}
	}
	check("第一次启动")

	// 之后在新版里把主面板那把改成不限、全局白名单也换了:再启动不能被旧白名单盖回去
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Model(&model.ApiKey{}).Where("id = ?", master).Updates(map[string]interface{}{"ip_allow": false, "ip_list": ""})
	// 回退期间旧版新建了一把(新列是 NULL)
	late := old("late", 0)
	_ = Close(db)
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer Close(db)
	var m, l model.ApiKey
	db.First(&m, master)
	db.First(&l, late)
	if m.IpAllow || m.IpList != "" {
		t.Fatalf("新版里改过的不该被旧白名单盖回去: %+v", m)
	}
	if !l.IpAllow || l.IpList != "198.51.100.7, 192.0.2.0/24" {
		t.Fatalf("回退期间建的密钥再升上来应补上白名单: %+v", l)
	}
}
