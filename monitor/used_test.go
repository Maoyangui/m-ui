package monitor

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/notify"
)

// 巡检只测本机线路真正用到的上游;告警按"哪台服务器在用"判定,并带上服务器名。
func TestUpstreamCheckScopeAndAlerts(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Upstream{Name: "warp", Type: "socks"})  // id 1:本机在用
	db.Create(&model.Upstream{Name: "jp", Type: "tuic"})     // id 2:只有副机在用
	db.Create(&model.Upstream{Name: "unused", Type: "tuic"}) // id 3:谁都没用
	var checked []string
	m := New(Deps{
		DB:      db,
		Setting: func(string) string { return "" },
		Check: func(u model.Upstream) (bool, int, string, string) {
			checked = append(checked, u.Name)
			return false, 0, "urltest", "连不上"
		},
		Notify:        notify.New(func(string) string { return "" }),
		UsedUpstreams: func() map[uint]bool { return map[uint]bool{1: true} },
		SelfName:      func() string { return "主机" },
		SelfNodeId:    func() uint { return 1 },
		Alerting:      func() bool { return true },
		RemoteHealth: func() []NodeHealth {
			return []NodeHealth{{NodeId: 2, NodeName: "高带宽", Id: 2, Name: "jp", OK: false, Fails: 5, Error: "超时"}}
		},
	})

	changed := m.RunUpstreamCheck()
	if len(checked) != 1 || checked[0] != "warp" {
		t.Fatalf("只该测本机用到的 warp,实际测了 %v", checked)
	}
	if len(m.Results()) != 1 || m.Results()[0].Name != "warp" {
		t.Fatalf("本机结果只该有 warp: %+v", m.Results())
	}
	// 阈值默认 2:本机 warp 才失败 1 次不告警;副机上报的 jp 已经连败 5 次,要告警
	if len(changed) != 1 || changed[0] != "jp" {
		t.Fatalf("第一轮应只有副机的 jp 触发告警: %v", changed)
	}
	// 再来一轮:本机 warp 连败 2 次,达到阈值
	changed = m.RunUpstreamCheck()
	if len(changed) != 1 || changed[0] != "warp" {
		t.Fatalf("第二轮应轮到本机 warp: %v", changed)
	}
	// 已告过警的不重复发
	if changed := m.RunUpstreamCheck(); len(changed) != 0 {
		t.Fatalf("同一条故障不该反复告警: %v", changed)
	}
}

// 副机不发告警:通知配置在主机上,副机重复发会变成三条一样的消息。
func TestNodeDoesNotAlert(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Upstream{Name: "warp", Type: "socks"})
	m := New(Deps{
		DB: db, Setting: func(string) string { return "" },
		Check:         func(model.Upstream) (bool, int, string, string) { return false, 0, "tcp", "连不上" },
		Notify:        notify.New(func(string) string { return "" }),
		UsedUpstreams: func() map[uint]bool { return map[uint]bool{1: true} },
		Alerting:      func() bool { return false }, // 本机是副机
	})
	for i := 0; i < 5; i++ {
		if changed := m.RunUpstreamCheck(); len(changed) != 0 {
			t.Fatalf("副机不该产生告警: %v", changed)
		}
	}
	if r := m.Results(); len(r) != 1 || r[0].Fails != 5 {
		t.Fatalf("副机仍要记连续失败次数并上报: %+v", r)
	}
}

// 上游不再被本机使用后,它的旧结果要清掉,挂着的告警状态也要清掉。
func TestStaleResultsCleared(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Upstream{Name: "warp", Type: "socks"})
	used := map[uint]bool{1: true}
	m := New(Deps{
		DB: db, Setting: func(string) string { return "" },
		Check:         func(model.Upstream) (bool, int, string, string) { return false, 0, "tcp", "连不上" },
		Notify:        notify.New(func(string) string { return "" }),
		UsedUpstreams: func() map[uint]bool { return used },
		SelfNodeId:    func() uint { return 1 },
		Alerting:      func() bool { return true },
	})
	m.RunUpstreamCheck()
	m.RunUpstreamCheck() // 达到阈值,已告警
	if len(m.Results()) != 1 {
		t.Fatal("前提:本机有一条结果")
	}
	used = map[uint]bool{} // 线路改了部署,本机不再用它
	m.RunUpstreamCheck()
	if len(m.Results()) != 0 {
		t.Fatalf("不再使用的上游结果应清掉: %+v", m.Results())
	}
	used = map[uint]bool{1: true} // 又用回来:应能重新告警,而不是被旧状态挡住
	m.RunUpstreamCheck()
	if changed := m.RunUpstreamCheck(); len(changed) != 1 {
		t.Fatalf("重新使用后应能再次告警: %v", changed)
	}
}
