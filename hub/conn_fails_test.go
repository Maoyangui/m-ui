package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
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
	if len(fails) != 1 || fails[0].IP != "9.9.9.9" || fails[0].Server != "在线" || fails[0].NodeId != 2 || fails[0].Failed != "tls" {
		t.Fatalf("只汇总宽限期内的、标上服务器名与 id: %+v", fails)
	}
	if conns := h.RemoteConns(); len(conns) != 1 || conns[0].IP != "2.2.2.2" || conns[0].Failed != "" {
		t.Fatalf("正常的最近连接不混进失败记录: %+v", conns)
	}
}

// 老副机不上报握手 / 认证失败:报告里没有 connFailsOk。主机把它标出来(服务器页、概览据此注明"这台不上报"),
// 新副机没有失败时 connFails 被 omitempty 省掉,但 connFailsOk 在,不会被误标。
func TestNodeStatusMarksOldNodesWithoutConnFails(t *testing.T) {
	var sent sentAlerts
	h := sent.hub()
	n := model.Node{Id: 2, Name: "老副机"}
	var old Report
	if err := json.Unmarshal([]byte(`{"version":"0.7.16","conns":[{"ip":"1.1.1.1","line":"tj"}]}`), &old); err != nil {
		t.Fatal(err)
	}
	h.setStatus(n, true, "", &old)
	if st := h.Statuses()[2]; !st.NoConnFails {
		t.Fatalf("老副机要标出来: %+v", st)
	}
	b, _ := json.Marshal(h.Statuses()[2])
	if !strings.Contains(string(b), `"noConnFails":true`) {
		t.Fatalf("接口里要带上: %s", b)
	}
	var fresh Report
	b, _ = json.Marshal(Report{Version: "0.7.17", ConnFailsOK: true})
	if err := json.Unmarshal(b, &fresh); err != nil || strings.Contains(string(b), "connFails\"") {
		t.Fatalf("新副机没有失败时只带能力标记: %s %v", b, err)
	}
	h.setStatus(n, true, "", &fresh)
	if st := h.Statuses()[2]; st.NoConnFails {
		t.Fatalf("升级后不再标: %+v", st)
	}
	b, _ = json.Marshal(h.Statuses()[2])
	if strings.Contains(string(b), "noConnFails") {
		t.Fatalf("新副机不带这个字段: %s", b)
	}
}
