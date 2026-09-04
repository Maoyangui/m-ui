package sub

import (
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 线路参数、用户凭据、外部节点都可能是"半成品"(旧库导入、外部订阅抓回来的脏数据):
// 三种订阅格式在这些输入下可以少几个节点,但不能 panic——订阅服务是对公网开放的。
func TestSubscriptionBuildersNeverPanic(t *testing.T) {
	users := []model.User{
		{Name: "u"},                                               // 完全没有凭据
		{Name: "u", Credentials: []byte(`{}`)},                    // 空对象
		{Name: "u", Credentials: []byte(`{"vless":{}}`)},          // 缺 uuid
		{Name: "u", Credentials: []byte(`not json`)},              // 坏 JSON
		{Name: "u", Credentials: []byte(`{"tuic":{"uuid":123}}`)}, // 类型不对
	}
	lines := []model.Line{
		{Name: "a", Protocol: "hysteria2", Port: 1, Enabled: true},
		{Name: "b", Protocol: "vless", Port: 2, Enabled: true, Tls: []byte(`{"mode":"reality"}`)},      // reality 缺 key
		{Name: "c", Protocol: "shadowsocks", Port: 3, Enabled: true, Options: []byte(`{"method":""}`)}, // 空算法
		{Name: "d", Protocol: "tuic", Port: 4, Enabled: true, Options: []byte(`bad json`)},
		{Name: "e", Protocol: "vmess", Port: 5, Enabled: true, Transport: []byte(`{"type":"ws"}`)},
		{Name: "f", Protocol: "unknown", Port: 6, Enabled: true},
		{Name: "g", Protocol: "trojan", Port: 7, Enabled: true, Addrs: []byte(`[{"server":""}]`)},
	}
	entries := []Entry{{Host: "h.example"}, {Host: "", SNI: "x"}, {Host: "1.2.3.4", Insecure: true}}
	external := []ExtItem{{Name: "x", Links: []string{"ss://", "bad", ""}, Clash: []map[string]interface{}{{}, {"type": "ss"}}}}

	for _, u := range users {
		opt := Options{Entries: entries, External: external, ShowNotice: true, UpdateHours: 12, ProfileTitle: "测试"}
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("BuildLinkSub panic: %v", v)
				}
			}()
			_ = BuildLinkSub(u, lines, opt)
		}()
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("BuildClashSub panic: %v", v)
				}
			}()
			_, _ = BuildClashSub(u, lines, opt)
		}()
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("BuildSingBoxSub panic: %v", v)
				}
			}()
			_, _ = BuildSingBoxSub(u, lines, opt)
		}()
	}
}
