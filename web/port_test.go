package web

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 端口只在同一台服务器上才会撞:主机的 443 和只部署在副机 B 的 443 互不相干;
// 部署范围有交集(含"全部服务器")才算冲突。
func TestLinePortConflictIsPerServer(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	db.Create(&model.Node{Name: "副机A", ApiUrl: "http://a", Enabled: true, Sort: 2})
	db.Create(&model.Node{Name: "副机B", ApiUrl: "http://b", Enabled: true, Sort: 3})

	// 主机 + 副机A 上的 443
	a := model.Line{Name: "HK-443", Protocol: "hysteria2", Port: 443, Enabled: true, NodeIds: []byte(`[1,2]`)}
	if err := s.validateLine(&a); err != nil {
		t.Fatalf("第一条 443 应通过: %v", err)
	}
	db.Create(&a)

	// 只在副机B 上的 443:没有交集,放行(而且不测本机端口占用)
	b := model.Line{Name: "B-443", Protocol: "hysteria2", Port: 443, Enabled: true, NodeIds: []byte(`[3]`)}
	if err := s.validateLine(&b); err != nil {
		t.Fatalf("不同服务器上的同端口应放行: %v", err)
	}
	db.Create(&b)

	// 全部服务器的 443:和上面两条都有交集
	all := model.Line{Name: "ALL-443", Protocol: "hysteria2", Port: 443, Enabled: true}
	if err := s.validateLine(&all); err == nil || !strings.Contains(err.Error(), "443") {
		t.Fatalf("部署到全部服务器的同端口应被拒并点名: %v", err)
	}
	// 副机A + 副机B 的 443:和 HK-443 在副机A 上撞
	ab := model.Line{Name: "AB-443", Protocol: "hysteria2", Port: 443, Enabled: true, NodeIds: []byte(`[2,3]`)}
	if err := s.validateLine(&ab); err == nil || !strings.Contains(err.Error(), "HK-443") || !strings.Contains(err.Error(), "副机A") {
		t.Fatalf("应指出在副机A 上和 HK-443 冲突: %v", err)
	}
	// 编辑 B-443 自己(端口不变)不能误报
	edit := b
	edit.Name = "B-443-renamed"
	if err := s.validateLine(&edit); err != nil {
		t.Fatalf("编辑自己不该报冲突: %v", err)
	}
	// 老库升级:曾经的全局唯一索引已去掉,同端口两条线路能同时存在
	var n int64
	db.Model(&model.Line{}).Where("port = ?", 443).Count(&n)
	if n != 2 {
		t.Fatalf("库里应有两条 443 线路,实际 %d", n)
	}
}

// 没有服务器记录(全新库、还没起过数据面)时按部署范围直接比。
func TestLinePortConflictWithoutNodes(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Line{Name: "a", Protocol: "hysteria2", Port: 8443, Enabled: true, NodeIds: []byte(`[1]`)})
	if err := s.validateLine(&model.Line{Name: "b", Protocol: "hysteria2", Port: 8443, Enabled: true, NodeIds: []byte(`[2]`)}); err != nil {
		t.Fatalf("范围不相交应放行: %v", err)
	}
	if err := s.validateLine(&model.Line{Name: "c", Protocol: "hysteria2", Port: 8443, Enabled: true}); err == nil {
		t.Fatal("全部服务器应与 a 冲突")
	}
	if err := s.validateLine(&model.Line{Name: "d", Protocol: "hysteria2", Port: 8443, Enabled: true, NodeIds: []byte(`[1,3]`)}); err == nil {
		t.Fatal("范围相交应冲突")
	}
}
