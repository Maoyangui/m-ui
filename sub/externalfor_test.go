package sub

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// externalFor 把分配上的“不生成策略组”开关带进 ExtItem:开关存在分配上,
// 同一个订阅分给不同用户,各人的开关互不影响;没关的 NoGroup 为假。
func TestExternalForNoGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ext.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.ExtNode{}, &model.UserExt{}); err != nil {
		t.Fatal(err)
	}
	mk := func(name, value string, sort int) model.ExtNode {
		e := model.ExtNode{Name: name, Type: "link", Value: value, Enabled: true, Sort: sort}
		if err := db.Create(&e).Error; err != nil {
			t.Fatal(err)
		}
		return e
	}
	a := mk("机场A", "trojan://p@198.51.100.1:443?sni=a.example.com#A-1", 1)
	b := mk("德国", "trojan://p@198.51.100.2:443?sni=b.example.com#DE-1", 2)
	// 用户 1:机场A 生成组,德国不生成;用户 2:德国生成组
	for _, r := range []model.UserExt{
		{UserId: 1, ExtId: a.Id},
		{UserId: 1, ExtId: b.Id, NoGroup: true},
		{UserId: 2, ExtId: b.Id},
	} {
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{db: db}
	got1 := s.externalFor(1)
	if len(got1) != 2 {
		t.Fatalf("用户 1 应有 2 个订阅,实际 %d", len(got1))
	}
	if got1[0].Name != "机场A" || got1[0].NoGroup {
		t.Fatalf("用户 1 的机场A应生成组,实际 %+v", got1[0])
	}
	if got1[1].Name != "德国" || !got1[1].NoGroup {
		t.Fatalf("用户 1 的德国应不生成组,实际 %+v", got1[1])
	}
	if len(got1[1].Clash) == 0 {
		t.Fatalf("关了分组的订阅节点不能丢,实际 %+v", got1[1])
	}
	got2 := s.externalFor(2)
	if len(got2) != 1 || got2[0].Name != "德国" || got2[0].NoGroup {
		t.Fatalf("用户 2 的德国应生成组,实际 %+v", got2)
	}
}
