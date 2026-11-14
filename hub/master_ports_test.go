package hub

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 主机在快照里带上自己的面板 / 订阅 / 代理面板端口:副机的私网屏蔽据此拦用户经副机代理去碰主机的管理端口。
// 主机改了这些端口要整份重推(修订号变);副机上报的端口变了不重推(只给主机建线路避让用)。
func TestSnapshotCarriesMasterServicePorts(t *testing.T) {
	master, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(master)
	node, err := database.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(node)
	vals := map[string]string{"webPort": "8443", "subPort": "8444"}
	setting := func(k string) string { return vals[k] }
	master.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1, PublicIP: "203.0.113.1"})
	master.Create(&model.Node{Name: "副机", ApiUrl: "http://198.51.100.7:7000", Token: "t", Enabled: true, Sort: 2})

	portsOf := func(raw json.RawMessage) []model.ServicePort {
		var ps []model.ServicePort
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &ps); err != nil {
				t.Fatal(err)
			}
		}
		return ps
	}
	snap, err := BuildSnapshot(master, setting)
	if err != nil {
		t.Fatal(err)
	}
	var got []model.ServicePort
	for _, n := range snap.Nodes {
		if n.IsLocal {
			got = portsOf(n.ServicePorts)
		}
	}
	want := []model.ServicePort{{Label: "面板", Port: 8443}, {Label: "订阅", Port: 8444}, {Label: "代理面板", Port: 2054}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("主机那一行的端口 = %v,应为 %v", got, want)
	}
	// 主机库里那一行本身不存(没人给主机上报),只在快照里填
	var row model.Node
	master.Where("is_local = ?", true).First(&row)
	if len(row.ServicePorts) != 0 {
		t.Fatalf("主机库里不该写入端口: %s", row.ServicePorts)
	}

	// 副机应用后,主机那一行(在副机上 is_local=false)带着端口,render 的集群屏蔽据此拦
	snap.SelfNodeId = 2
	if _, _, err := ApplySnapshot(node, snap); err != nil {
		t.Fatal(err)
	}
	var onNode model.Node
	node.Where("name = ?", "主机").First(&onNode)
	if onNode.IsLocal || !reflect.DeepEqual(portsOf(onNode.ServicePorts), want) {
		t.Fatalf("副机上主机那一行: isLocal=%v 端口=%s", onNode.IsLocal, onNode.ServicePorts)
	}

	rev := snap.Revision
	// 副机上报的端口变了:不重推
	p, _ := json.Marshal([]model.ServicePort{{Label: "面板", Port: 9999}})
	master.Model(&model.Node{}).Where("name = ?", "副机").Update("service_ports", p)
	if s2, _ := BuildSnapshot(master, setting); s2.Revision != rev {
		t.Fatal("副机上报的端口变了不该换修订号")
	}
	// 主机改面板端口、关掉代理面板:要重推
	vals["webPort"] = "9443"
	s3, _ := BuildSnapshot(master, setting)
	if s3.Revision == rev {
		t.Fatal("主机改了面板端口,修订号应变")
	}
	vals["resellerEnabled"] = "false"
	s4, _ := BuildSnapshot(master, setting)
	if s4.Revision == s3.Revision {
		t.Fatal("主机关掉代理面板,修订号应变")
	}
	for _, n := range s4.Nodes {
		if n.IsLocal {
			for _, sp := range portsOf(n.ServicePorts) {
				if sp.Label == "代理面板" {
					t.Fatal("代理面板关掉后不该再列它的端口")
				}
			}
		}
	}
}
