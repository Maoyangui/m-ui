package sub

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"

	"gorm.io/gorm"
)

func relayDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	return db
}

func relayCreds() json.RawMessage {
	b, _ := json.Marshal(creds.Generate("relay"))
	return b
}

// 线路当上游:出站参数和订阅同一套,凭据用上游自己那份;目标线路的入站里挂着专用用户,只在所选的那台服务器。
func TestRelayOutboundAndInboundUser(t *testing.T) {
	db := relayDB(t)
	local := model.Node{Name: "本机", IsLocal: true, Enabled: true, Addr: "1.1.1.1"}
	remote := model.Node{Name: "落地", Enabled: true, Addr: "2.2.2.2", Domain: "exit.example.com"}
	db.Create(&local)
	db.Create(&remote)
	ids, _ := json.Marshal([]uint{remote.Id})
	line := model.Line{Name: "出口", Protocol: "hysteria2", Port: 5555, Enabled: true, NodeIds: ids, Options: []byte(`{"obfs":{"type":"salamander","password":"o p"}}`)}
	db.Create(&line)
	up := model.Upstream{Name: "到落地", FromLine: line.Id, FromNode: remote.Id, RelayCreds: relayCreds()}
	db.Create(&up)
	if err := FillRelay(db, &up); err != nil {
		t.Fatal(err)
	}
	if up.Type != "hysteria2" {
		t.Fatalf("类型: %s", up.Type)
	}
	var opts map[string]interface{}
	if err := json.Unmarshal(up.Options, &opts); err != nil {
		t.Fatal(err)
	}
	if opts["server"] != "2.2.2.2" || opts["server_port"].(float64) != 5555 {
		t.Fatalf("地址不对: %v", opts)
	}
	var c map[string]map[string]interface{}
	json.Unmarshal(up.RelayCreds, &c)
	if opts["password"] != c["hysteria2"]["password"] {
		t.Fatalf("要用上游的专用凭据: %v", opts)
	}
	obfs, _ := opts["obfs"].(map[string]interface{})
	if obfs == nil || obfs["password"] != "o p" {
		t.Fatalf("混淆参数丢了(链接里以前就漏了这一项): %v", opts)
	}
	tls, _ := opts["tls"].(map[string]interface{})
	if tls == nil || tls["server_name"] != "exit.example.com" {
		t.Fatalf("SNI 要和订阅一样: %v", opts)
	}
	db.Model(&model.Upstream{}).Where("id = ?", up.Id).Updates(map[string]interface{}{"type": up.Type, "options": up.Options})

	// 这台不是目标服务器:线路没部署,配置里没有
	raw, err := render.BuildConfig(db, render.NodeCert{CertPath: "c", KeyPath: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), model.RelayUserName(up.Id)) {
		t.Fatal("别的服务器不该有专用用户")
	}
	// 切到目标服务器的视角
	db.Model(&model.Node{}).Where("id = ?", local.Id).Update("is_local", false)
	db.Model(&model.Node{}).Where("id = ?", remote.Id).Update("is_local", true)
	raw, err = render.BuildConfig(db, render.NodeCert{CertPath: "c", KeyPath: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), model.RelayUserName(up.Id)) {
		t.Fatalf("目标服务器的入站要挂专用用户:\n%s", raw)
	}
	// 记账与在线都不算它
	if model.Owner(model.RelayUserName(up.Id)) != "" || model.Owner(render.PlaceholderName) != "" {
		t.Fatal("内部用户不能归到任何人名下")
	}
	if model.Owner("alice#share") != "alice" || model.Owner("alice") != "alice" {
		t.Fatal("真实用户的归属不能变")
	}
}

// 线路的端口、服务器地址变了,上游跟着改;算不出来(线路停用不算、没部署了)保持上一份。
func TestSyncRelayUpstreams(t *testing.T) {
	db := relayDB(t)
	node := model.Node{Name: "落地", IsLocal: true, Enabled: true, Addr: "2.2.2.2"}
	db.Create(&node)
	line := model.Line{Name: "出口", Protocol: "trojan", Port: 5555, Enabled: true}
	db.Create(&line)
	up := model.Upstream{Name: "到落地", FromLine: line.Id, FromNode: node.Id, RelayCreds: relayCreds()}
	db.Create(&up)
	if err := FillRelay(db, &up); err != nil {
		t.Fatal(err)
	}
	db.Model(&model.Upstream{}).Where("id = ?", up.Id).Updates(map[string]interface{}{"type": up.Type, "options": up.Options})
	if changed, err := SyncRelayUpstreams(db); err != nil || changed {
		t.Fatalf("没变不该改: %v %v", changed, err)
	}
	db.Model(&model.Line{}).Where("id = ?", line.Id).Update("port", 6666)
	if changed, err := SyncRelayUpstreams(db); err != nil || !changed {
		t.Fatalf("端口变了要改: %v %v", changed, err)
	}
	var got model.Upstream
	db.First(&got, up.Id)
	if !strings.Contains(string(got.Options), "6666") {
		t.Fatalf("端口没跟上: %s", got.Options)
	}
	db.Model(&model.Node{}).Where("id = ?", node.Id).Update("addr", "3.3.3.3")
	SyncRelayUpstreams(db)
	db.First(&got, up.Id)
	if !strings.Contains(string(got.Options), "3.3.3.3") {
		t.Fatalf("服务器地址没跟上: %s", got.Options)
	}
	// 协议变了类型也跟着变
	db.Model(&model.Line{}).Where("id = ?", line.Id).Update("protocol", "anytls")
	SyncRelayUpstreams(db)
	db.First(&got, up.Id)
	if got.Type != "anytls" {
		t.Fatalf("类型没跟上: %s", got.Type)
	}
	// 服务器停用:算不出来就保持上一份,面板上报问题
	db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", false)
	if changed, _ := SyncRelayUpstreams(db); changed {
		t.Fatal("算不出来不该改")
	}
	if RelayProblem(db, got) == "" {
		t.Fatal("服务器停用了要报问题")
	}
	db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", true)
	db.Model(&model.Line{}).Where("id = ?", line.Id).Update("enabled", false)
	if RelayProblem(db, got) == "" {
		t.Fatal("线路停用了要报问题")
	}
}

// 绕成环要拦:A 的上游是线路 B、B 的上游是线路 A;自己当自己的上游也算。
func TestRelayLoop(t *testing.T) {
	db := relayDB(t)
	node := model.Node{Name: "n", IsLocal: true, Enabled: true, Addr: "2.2.2.2"}
	db.Create(&node)
	a := model.Line{Name: "A", Protocol: "trojan", Port: 1, Enabled: true}
	b := model.Line{Name: "B", Protocol: "trojan", Port: 2, Enabled: true}
	c := model.Line{Name: "C", Protocol: "trojan", Port: 3, Enabled: true}
	db.Create(&a)
	db.Create(&b)
	db.Create(&c)
	toA := model.Upstream{Name: "toA", Type: "trojan", FromLine: a.Id, FromNode: node.Id}
	toB := model.Upstream{Name: "toB", Type: "trojan", FromLine: b.Id, FromNode: node.Id}
	toC := model.Upstream{Name: "toC", Type: "trojan", FromLine: c.Id, FromNode: node.Id}
	db.Create(&toA)
	db.Create(&toB)
	db.Create(&toC)

	// A → toB:没有环
	a.UpstreamId = toB.Id
	if p := RelayLoop(db, a); p != "" {
		t.Fatalf("不该有环: %s", p)
	}
	db.Model(&model.Line{}).Where("id = ?", a.Id).Update("upstream_id", toB.Id)
	// B → toA:A→B→A
	b.UpstreamId = toA.Id
	if p := RelayLoop(db, b); p == "" || !strings.Contains(p, "A") {
		t.Fatalf("应该查出环: %q", p)
	}
	// B → toC:没环
	b.UpstreamId = toC.Id
	if p := RelayLoop(db, b); p != "" {
		t.Fatalf("不该有环: %s", p)
	}
	// 自己当自己的上游
	c.UpstreamId = toC.Id
	if p := RelayLoop(db, c); p == "" {
		t.Fatal("自己当自己的上游也是环")
	}
	// 分流规则指过去也算
	c.UpstreamId = 0
	c.RouteRules = []byte(`[{"type":"domain_suffix","values":["a.com"],"to":` + itoa(toC.Id) + `}]`)
	if p := RelayLoop(db, c); p == "" {
		t.Fatal("分流规则绕回自己也是环")
	}
	// 改上游的目标:把 toB 改到 C,没环;A 用 toB,把 toB 改到 A 自己,有环
	if p := RelayLoopOf(db, model.Upstream{Id: toB.Id, FromLine: a.Id}); p == "" {
		t.Fatal("把上游改成指向用它的线路,应该查出环")
	}
	if p := RelayLoopOf(db, model.Upstream{Id: toB.Id, FromLine: c.Id}); p != "" {
		t.Fatalf("不该有环: %s", p)
	}
}

func itoa(n uint) string {
	b, _ := json.Marshal(n)
	return string(b)
}
