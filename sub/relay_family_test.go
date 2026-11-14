package sub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 线路作上游按地址族:目标线路在所选服务器上选了 IPv6,而用这条上游的线路部署在没有 IPv6 的服务器上(入口机),
// 连不上,要在上游列表里说出来;反过来入口机只有 IPv6、目标给 IPv4 也一样。入口机还没上报过地址时不下结论。
func TestRelayProblemByFamily(t *testing.T) {
	db := relayDB(t)
	local := model.Node{Name: "本机", IsLocal: true, Enabled: true, Sort: 1}
	exit := model.Node{Name: "落地", Enabled: true, PublicIP: "203.0.113.2", PublicIP6: "2001:db8::2", Domain: "exit.example.com", Sort: 2}
	entry := model.Node{Name: "入口", Enabled: true, PublicIP: "198.51.100.1", Sort: 3}
	for _, n := range []*model.Node{&local, &exit, &entry} {
		db.Create(n)
	}
	db.Create(&model.Setting{Key: "publicIp", Value: "198.51.100.9"}) // 本机只有 IPv4
	ids := func(v ...uint) json.RawMessage { raw, _ := json.Marshal(v); return raw }
	fam := func(f string) json.RawMessage { return json.RawMessage(`{"` + jsonUint(exit.Id) + `":"` + f + `"}`) }
	target := model.Line{Name: "出口", Protocol: "trojan", Port: 5555, Enabled: true, NodeIds: ids(exit.Id), NodeFamily: fam("v6")}
	db.Create(&target)
	up := model.Upstream{Name: "到落地", FromLine: target.Id, FromNode: exit.Id, RelayCreds: relayCreds()}
	db.Create(&up)
	if err := FillRelay(db, &up); err != nil {
		t.Fatal(err)
	}
	db.Model(&model.Upstream{}).Where("id = ?", up.Id).Updates(map[string]interface{}{"type": up.Type, "options": up.Options})
	if !strings.Contains(string(up.Options), "2001:db8::2") {
		t.Fatalf("目标线路在落地选了 IPv6,上游应连它的 IPv6: %s", up.Options)
	}

	if p := RelayProblem(db, up); p != "" {
		t.Fatalf("还没有线路用这条上游:没有入口机,不下结论: %q", p)
	}
	user := model.Line{Name: "入口线路", Protocol: "vless", Port: 6666, Enabled: true, NodeIds: ids(entry.Id), UpstreamId: up.Id}
	db.Create(&user)
	if p := RelayProblem(db, up); !strings.Contains(p, "没有 IPv6") || !strings.Contains(p, "「入口」") || !strings.Contains(p, "「落地」") {
		t.Fatalf("入口机没有 IPv6 应提示: %q", p)
	}
	// 部署到全部服务器:本机(只有 IPv4)也是入口机
	db.Model(&model.Line{}).Where("id = ?", user.Id).Update("node_ids", nil)
	if p := RelayProblem(db, up); !strings.Contains(p, "「本机、入口」") {
		t.Fatalf("本机也该列出: %q", p)
	}
	db.Model(&model.Line{}).Where("id = ?", user.Id).Update("node_ids", ids(entry.Id))
	// 入口机有了 IPv6:没问题
	db.Model(&model.Node{}).Where("id = ?", entry.Id).Update("public_ip6", "2001:db8::1")
	if p := RelayProblem(db, up); p != "" {
		t.Fatalf("入口机有 IPv6 了: %q", p)
	}
	// 入口机还没上报过地址:不下结论
	db.Model(&model.Node{}).Where("id = ?", entry.Id).Updates(map[string]interface{}{"public_ip": "", "public_ip6": ""})
	if p := RelayProblem(db, up); p != "" {
		t.Fatalf("没上报过地址的不下结论: %q", p)
	}
	// 入口机只有 IPv6,目标改回 IPv4:反过来也连不上
	db.Model(&model.Node{}).Where("id = ?", entry.Id).Updates(map[string]interface{}{"public_ip": "2001:db8::1", "public_ip6": "2001:db8::1"})
	db.Model(&model.Line{}).Where("id = ?", target.Id).Update("node_family", fam("v4"))
	if p := RelayProblem(db, up); !strings.Contains(p, "只有 IPv6") {
		t.Fatalf("纯 IPv6 入口机连 IPv4 目标应提示: %q", p)
	}
	// 经上游组用到也算
	db.Model(&model.Line{}).Where("id = ?", user.Id).Update("upstream_id", 0)
	if p := RelayProblem(db, up); p != "" {
		t.Fatalf("没有线路用了: %q", p)
	}
	grp := model.Upstream{Name: "组", Type: "urltest", Options: []byte(`{"outbounds":["到落地"]}`)}
	db.Create(&grp)
	db.Model(&model.Line{}).Where("id = ?", user.Id).Update("upstream_id", grp.Id)
	if p := RelayProblem(db, up); !strings.Contains(p, "只有 IPv6") {
		t.Fatalf("经上游组用到也要算: %q", p)
	}
}

func jsonUint(v uint) string {
	b, _ := json.Marshal(v)
	return string(b)
}
