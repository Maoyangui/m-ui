package ha

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 开着主机容灾时,副机接任后要用的端口不能给线路(用户 11-15):它自己的代理面板端口(副机平时不监听,接任后才起),
// 选了全量接管时还有主机的订阅端口。开容灾、改设置时拦下已有的冲突并点名;只走 UDP 的线路不冲突;
// 中转入口开在副机上的端口也算;主机自己的面板端口不迁过去,不留。
func TestTakeoverPortsReserved(t *testing.T) {
	cl := readyCluster(t, 1)
	m, nodeId := cl.m, cl.ids[0]
	cl.cands[0].set(map[string]string{"resellerPort": "2054"})
	cl.h.RunOnce() // 副机上报它的代理面板端口
	if ps := m.TakeoverPorts(nodeId); len(ps) != 1 || ps[0].Port != 2054 || ps[0].What != TakeoverReseller {
		t.Fatalf("副机接任后要用它自己的代理面板端口: %+v", ps)
	}
	cfg := m.Config()
	nodes, _ := json.Marshal([]uint{nodeId})
	tcp := model.Line{Name: "VL", Protocol: "vless", Port: 2054, Enabled: true, NodeIds: nodes}
	m.d.DB.Create(&tcp)
	udp := model.Line{Name: "HY", Protocol: "hysteria2", Port: 2054, Enabled: true, NodeIds: nodes}
	m.d.DB.Create(&udp)
	cs := m.takeoverConflicts(cfg)
	if len(cs) != 1 || cs[0].Line != "VL" || cs[0].Port != 2054 || cs[0].What != TakeoverReseller {
		t.Fatalf("占着副机代理面板端口的 TCP 线路算冲突,只走 UDP 的不算: %+v", cs)
	}
	in := ConfigInput{Config: cfg}
	if err := m.SaveConfig(in); err == nil || !strings.Contains(err.Error(), "VL@") || !strings.Contains(err.Error(), ":2054") {
		t.Fatalf("开着 / 开启主机容灾时拦下已有的冲突并点名: %v", err)
	}
	off := in
	off.Mode = ModeOff
	if err := m.SaveConfig(off); err != nil {
		t.Fatalf("关掉容灾不受影响: %v", err)
	}
	if len(m.TakeoverPorts(nodeId)) != 0 {
		t.Fatal("没开容灾:不留端口")
	}
	// 换掉线路端口后就能开
	m.d.DB.Model(&model.Line{}).Where("id = ?", tcp.Id).Update("port", 2055)
	if err := m.SaveConfig(in); err != nil {
		t.Fatalf("没冲突了能开: %v", err)
	}
	// 全量接管:主机的订阅端口也留(副机上的线路、中转入口占着都不行)
	m.set(map[string]string{"subDomain": "sub.example.com", "subPort": "2096", KeyDnsToken: "tok"})
	fw, _ := json.Marshal([]model.LineForward{{Via: nodeId, Port: 2096, To: nodeId}})
	relay := model.Line{Name: "RL", Protocol: "trojan", Port: 30443, Enabled: true, Forwards: fw}
	m.d.DB.Create(&relay)
	sub := in
	sub.TakeSub, sub.DnsToken = true, "tok"
	if err := m.SaveConfig(sub); err == nil || !strings.Contains(err.Error(), "RL@") || !strings.Contains(err.Error(), ":2096") {
		t.Fatalf("中转入口开在副机上的端口占着迁来的订阅端口:要拦: %v", err)
	}
	m.d.DB.Delete(&relay)
	if err := m.SaveConfig(sub); err != nil {
		t.Fatal(err)
	}
	if ps := m.TakeoverPorts(nodeId); len(ps) != 2 || ps[1].Port != 2096 || ps[1].What != TakeoverSub {
		t.Fatalf("全量接管:订阅端口也留: %+v", ps)
	}
	m.d.DB.Create(&model.Line{Name: "SUB", Protocol: "vmess", Port: 8443, Enabled: true, NodeIds: nodes})
	if cs := m.SubPortConflicts(8443); len(cs) != 1 || cs[0].Line != "SUB" {
		t.Fatalf("把订阅端口改成副机线路正用着的:要拦: %+v", cs)
	}
	if cs := m.SubPortConflicts(9443); len(cs) != 0 {
		t.Fatalf("没人用的端口随便改: %+v", cs)
	}
	// 就绪检查列出后来才出现的冲突(副机改了代理面板端口)
	cl.cands[0].set(map[string]string{"resellerPort": "2055"})
	cl.h.RunOnce()
	found := false
	for _, ch := range m.View(true).Ready {
		if ch.Code == "takeoverPorts" && ch.Level == "warn" {
			found = true
		}
	}
	if !found {
		t.Fatal("副机改了代理面板端口、撞上线路:就绪检查要列出来")
	}
}
