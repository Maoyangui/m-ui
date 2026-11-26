package sub

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// externalFor 把分配上的「单独成组」开关带进 ExtItem:开关存在分配上,同一个订阅分给不同用户,各人的开关互不影响;
// 没关的 NoGroup 为假。分享链接只有一个节点,不管开关怎样都不成组。
func TestExternalForNoGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ext.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := db.DB(); err == nil {
		t.Cleanup(func() { sqlDB.Close() }) // 先关库再删临时目录(Windows 上开着的文件删不掉)
	}
	if err := db.AutoMigrate(&model.ExtNode{}, &model.UserExt{}); err != nil {
		t.Fatal(err)
	}
	mk := func(name, typ, value, cache string, sort int) model.ExtNode {
		e := model.ExtNode{Name: name, Type: typ, Value: value, Cache: cache, Enabled: true, Sort: sort}
		if err := db.Create(&e).Error; err != nil {
			t.Fatal(err)
		}
		return e
	}
	a := mk("机场A", "sub", "https://a.example.com/sub", "trojan://p@198.51.100.1:443?sni=a.example.com#A-1", 1)
	b := mk("德国", "sub", "https://b.example.com/sub", "trojan://p@198.51.100.2:443?sni=b.example.com#DE-1", 2)
	c := mk("单条", "link", "trojan://p@198.51.100.3:443?sni=c.example.com#C-1", "", 3)
	// 用户 1:机场A 成组,德国不成组,单条是分享链接;用户 2:德国成组
	for _, r := range []model.UserExt{
		{UserId: 1, ExtId: a.Id},
		{UserId: 1, ExtId: b.Id, NoGroup: true},
		{UserId: 1, ExtId: c.Id},
		{UserId: 2, ExtId: b.Id},
	} {
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{db: db}
	got1 := s.externalFor(1)
	if len(got1) != 3 {
		t.Fatalf("用户 1 应有 3 个外部节点,实际 %d", len(got1))
	}
	if got1[0].Name != "机场A" || got1[0].NoGroup {
		t.Fatalf("用户 1 的机场A应成组,实际 %+v", got1[0])
	}
	if got1[1].Name != "德国" || !got1[1].NoGroup {
		t.Fatalf("用户 1 的德国应不成组,实际 %+v", got1[1])
	}
	if len(got1[1].Clash) == 0 {
		t.Fatalf("关了分组的订阅节点不能丢,实际 %+v", got1[1])
	}
	if got1[2].Name != "单条" || !got1[2].NoGroup || len(got1[2].Clash) != 1 {
		t.Fatalf("分享链接不成组、节点照常给,实际 %+v", got1[2])
	}
	got2 := s.externalFor(2)
	if len(got2) != 1 || got2[0].Name != "德国" || got2[0].NoGroup {
		t.Fatalf("用户 2 的德国应成组,实际 %+v", got2)
	}
}

// 外部订阅自己的「分给用户时单独成组」(ExtNode.NoGroup)只是新分配的默认值:订阅输出只看分配上的开关(UserExt.NoGroup),
// 两者不一样时以分配上的为准 —— 两列在库里都叫 no_group,查询里要分得开(审查后修正 F8-01)。
func TestExternalForUsesAssignmentNotSubDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ext2.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := db.DB(); err == nil {
		t.Cleanup(func() { sqlDB.Close() })
	}
	if err := db.AutoMigrate(&model.ExtNode{}, &model.UserExt{}); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, noGroup bool, sort int) model.ExtNode {
		e := model.ExtNode{Name: name, Type: "sub", Value: "https://" + name + ".example.com/sub",
			Cache: "trojan://p@198.51.100.9:443?sni=" + name + ".example.com#" + name + "-1", Enabled: true, Sort: sort, NoGroup: noGroup}
		if err := db.Create(&e).Error; err != nil {
			t.Fatal(err)
		}
		return e
	}
	defOff := mk("def-nogroup", true, 1) // 订阅默认不成组
	defOn := mk("def-group", false, 2)   // 订阅默认成组
	both := mk("both-nogroup", true, 3)
	for _, r := range []model.UserExt{
		{UserId: 7, ExtId: defOff.Id, NoGroup: false}, // 这个用户的分配上是成组
		{UserId: 7, ExtId: defOn.Id, NoGroup: true},   // 这个用户的分配上关了成组
		{UserId: 7, ExtId: both.Id, NoGroup: true},
	} {
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	var n int64
	db.Model(&model.ExtNode{}).Where("no_group = ?", true).Count(&n)
	if n != 2 {
		t.Fatalf("前提:库里应有 2 个默认不成组的订阅,实际 %d", n)
	}
	got := (&Server{db: db}).externalFor(7)
	if len(got) != 3 {
		t.Fatalf("应有 3 个外部订阅,实际 %d", len(got))
	}
	if got[0].Name != "def-nogroup" || got[0].NoGroup {
		t.Fatalf("订阅默认不成组、分配上成组:应成组,实际 %+v", got[0])
	}
	if got[1].Name != "def-group" || !got[1].NoGroup {
		t.Fatalf("订阅默认成组、分配上关了:应不成组,实际 %+v", got[1])
	}
	if got[2].Name != "both-nogroup" || !got[2].NoGroup {
		t.Fatalf("两边都关:应不成组,实际 %+v", got[2])
	}
}
