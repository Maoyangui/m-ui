package web

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 走 UDP 的线路端口落在别的 hysteria2 线路端口跳跃范围里:nft 把整段 UDP 转给那条线路,新线路收不到包,
// 必须拦下并点名(同一台服务器上才算);TCP 线路不受影响;随机默认端口也要避开跳跃范围(审计 MB04)。
func TestUDPPortInsideHopRangeRejected(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	db.Create(&model.Node{Name: "副机A", ApiUrl: "http://a", Enabled: true, Sort: 2})
	db.Create(&model.Node{Name: "副机B", ApiUrl: "http://b", Enabled: true, Sort: 3})
	db.Create(&model.Line{Name: "HY-hop", Protocol: "hysteria2", Port: 30000, Enabled: true, NodeIds: []byte(`[1,2]`),
		Options: []byte(`{"port_hopping":"40000-41000"}`)})

	for _, p := range []string{"tuic", "shadowsocks", "hysteria2"} {
		l := model.Line{Name: "new-" + p, Protocol: p, Port: 40500, Enabled: true, NodeIds: []byte(`[2]`)}
		if err := s.validateLine(&l); err == nil || !strings.Contains(err.Error(), "HY-hop") {
			t.Fatalf("%s 端口落在同机跳跃范围里应被拒并点名: %v", p, err)
		}
	}
	if err := s.validateLine(&model.Line{Name: "other-node", Protocol: "tuic", Port: 40500, Enabled: true, NodeIds: []byte(`[3]`)}); err != nil &&
		strings.Contains(err.Error(), "跳跃") {
		t.Fatalf("不同服务器上不该报跳跃范围冲突: %v", err)
	}
	if err := s.validateLine(&model.Line{Name: "tcp", Protocol: "socks", Port: 40500, Enabled: true, NodeIds: []byte(`[2]`)}); err != nil &&
		strings.Contains(err.Error(), "跳跃") {
		t.Fatalf("TCP 线路不受跳跃范围影响: %v", err)
	}

	// 随机默认端口:跳跃范围几乎盖满 5 位端口时,挑出来的只能在范围外(或者找不到)
	db.Model(&model.Line{}).Where("name = ?", "HY-hop").Update("options", []byte(`{"port_hopping":"10000-65000"}`))
	for i := 0; i < 5; i++ {
		if p, err := s.freePort(); err == nil && p <= 65000 {
			t.Fatalf("随机端口 %d 落在跳跃范围 10000-65000 里", p)
		}
	}
}
