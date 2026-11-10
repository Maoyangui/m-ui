package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 副机上报"到了但没过握手 / 认证"的来源:新字段带 omitempty,老主机不认、新主机收老副机的报告就是空;
// 主机按宽限期汇总、标上服务器名,和正常的最近连接分开。
func TestReportConnFailsCompatAndFresh(t *testing.T) {
	b, _ := json.Marshal(Report{})
	if strings.Contains(string(b), "connFails") || strings.Contains(string(b), `"failed"`) {
		t.Fatalf("没有的时候不该出现新字段: %s", b)
	}
	b, _ = json.Marshal(RecentConn{IP: "1.1.1.1"})
	if strings.Contains(string(b), "failed") || strings.Contains(string(b), "detail") {
		t.Fatalf("正常记录不带失败字段: %s", b)
	}
	var rep Report
	if err := json.Unmarshal([]byte(`{"conns":[{"ip":"1.1.1.1","line":"tj"}],"connFails":[{"ip":"9.9.9.9","line":"tj","failed":"tls","detail":"x"}]}`), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Conns) != 1 || len(rep.ConnFails) != 1 || rep.ConnFails[0].Failed != "tls" || rep.ConnFails[0].Detail != "x" {
		t.Fatalf("解析: %+v", rep)
	}

	now := time.Now().Unix()
	h := &Hub{
		status: map[uint]*NodeStatus{
			1: {Name: "失联", connFails: []RecentConn{{IP: "1.1.1.1", Failed: "auth"}}},
			2: {Name: "在线", conns: []RecentConn{{IP: "2.2.2.2"}}, connFails: []RecentConn{{IP: "9.9.9.9", Failed: "tls"}}},
		},
		remoteAt: map[uint]int64{1: now - remoteReportGrace - 1, 2: now},
	}
	fails := h.RemoteConnFails()
	if len(fails) != 1 || fails[0].IP != "9.9.9.9" || fails[0].Server != "在线" || fails[0].Failed != "tls" {
		t.Fatalf("只汇总宽限期内的、标上服务器名: %+v", fails)
	}
	if conns := h.RemoteConns(); len(conns) != 1 || conns[0].IP != "2.2.2.2" || conns[0].Failed != "" {
		t.Fatalf("正常的最近连接不混进失败记录: %+v", conns)
	}
}
