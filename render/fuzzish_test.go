package render

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 渲染跑在保存、热更新与数据面启动路径上:遇到半成品线路(旧库导入、手工改库)
// 只能返回错误,不能 panic——panic 会带走整个进程。
func TestBuildConfigNeverPanics(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)

	db.Create(&model.User{Name: "u", Enabled: true, Credentials: []byte(`{"vless":{}}`)})
	bad := []model.Line{
		{Name: "l1", Protocol: "vless", Port: 1, Enabled: true, Tls: []byte(`{"mode":"reality"}`)},
		{Name: "l2", Protocol: "hysteria2", Port: 2, Enabled: true, Options: []byte(`bad`)},
		{Name: "l3", Protocol: "shadowsocks", Port: 3, Enabled: true, Options: []byte(`{"method":123}`)},
		{Name: "l4", Protocol: "tuic", Port: 4, Enabled: true, NodeIds: []byte(`bad`)},
		{Name: "l5", Protocol: "trojan", Port: 5, Enabled: true, Transport: []byte(`{"type":"grpc"}`)},
		{Name: "l6", Protocol: "unknown", Port: 6, Enabled: true},
		{Name: "l7", Protocol: "vmess", Port: 7, Enabled: true, Addrs: []byte(`[{"server_port":"x"}]`)},
	}
	for i := range bad {
		db.Create(&bad[i])
		db.Create(&model.UserLine{UserId: 1, LineId: bad[i].Id})
	}
	db.Create(&model.Upstream{Name: "up", Type: "socks", Options: []byte(`bad json`)})

	func() {
		defer func() {
			if v := recover(); v != nil {
				t.Fatalf("BuildConfig panic: %v", v)
			}
		}()
		_, _ = BuildConfig(db, NodeCert{})
	}()
	for _, l := range bad {
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("InboundJSON(%s) panic: %v", l.Protocol, v)
				}
			}()
			_, _ = InboundJSON(l, NodeCert{})
		}()
	}
}
