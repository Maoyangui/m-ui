package runner

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// upstreamPort 运行中的配置(appliedRaw)里出站 tag 的 server_port。
func upstreamPort(t *testing.T, raw []byte, tag string) int {
	t.Helper()
	var cfg struct {
		Outbounds []struct {
			Tag  string `json:"tag"`
			Port int    `json:"server_port"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, o := range cfg.Outbounds {
		if o.Tag == tag {
			return o.Port
		}
	}
	return 0
}

func upstreamRunner(t *testing.T) (*Runner, model.Upstream, model.User, int) {
	t.Helper()
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(r.db) })
	up := model.Upstream{Name: "up", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":1}`)}
	r.db.Create(&up)
	port := freePort(t)
	line := model.Line{Name: "s5", Protocol: "socks", Port: port, UpstreamId: up.Id, Enabled: true}
	r.db.Create(&line)
	alice := model.User{Name: "alice", Enabled: true, Credentials: []byte(`{"socks":{"password":"alice-pw"}}`)}
	r.db.Create(&alice)
	r.db.Create(&model.UserLine{UserId: alice.Id, LineId: line.Id})
	if err := r.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	t.Cleanup(r.Stop)
	return r, up, alice, port
}

// 上游热更新(A → B)成功后,运行中的配置记录也要换成 B:之后整份配置起不来要回滚时,
// 回滚到的是最近真正在跑的 B,而不是热更新之前的 A(候选缺陷 1)。
func TestRollbackAfterUpstreamHotUpdateUsesCurrentUpstream(t *testing.T) {
	r, up, _, _ := upstreamRunner(t)
	r.db.Model(&model.Upstream{}).Where("id = ?", up.Id).Update("options", []byte(`{"server":"127.0.0.1","server_port":2}`))
	box := r.core.GetInstance()
	if err := r.ReloadUpstreams(); err != nil {
		t.Fatal(err)
	}
	if r.core.GetInstance() != box {
		t.Fatal("只改上游参数应热更新,不重启数据面")
	}
	if p := upstreamPort(t, r.appliedRaw, "up"); p != 2 {
		t.Fatalf("热更新后运行中的配置记录应是新上游(端口 2),实际 %d", p)
	}

	// 再加一条线路,端口被别的程序占着:整份配置起不来,回滚
	busy, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	r.db.Create(&model.Line{Name: "busy", Protocol: "socks", Port: busy.Addr().(*net.TCPAddr).Port, Enabled: true})
	err = r.ReloadAll()
	if err == nil || !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("端口被占应启动失败并回滚,实际: %v", err)
	}
	if !r.core.IsRunning() {
		t.Fatal("回滚后数据面应在运行")
	}
	if p := upstreamPort(t, r.appliedRaw, "up"); p != 2 {
		t.Fatalf("回滚应回到最近成功运行的上游(端口 2),实际回到了端口 %d", p)
	}
}

// 同一份快照里既改了上游参数、又停用了用户(副机失联期间先后改的,恢复后一次推过来):
// 副机走"只有上游变了"的热更新,用户表也必须跟着生效,停用的用户不能再用旧凭据建新连接(候选缺陷 2)。
func TestUpstreamHotUpdateAlsoAppliesUserChanges(t *testing.T) {
	r, up, alice, port := upstreamRunner(t)
	if !socks5Auth(t, port, "alice", "alice-pw") {
		t.Fatal("启用的用户应能通过认证")
	}
	r.db.Model(&model.Upstream{}).Where("id = ?", up.Id).Update("options", []byte(`{"server":"127.0.0.1","server_port":2}`))
	r.db.Model(&model.User{}).Where("id = ?", alice.Id).Update("enabled", false)
	box := r.core.GetInstance()
	if err := r.ReloadUpstreams(); err != nil {
		t.Fatal(err)
	}
	if socks5Auth(t, port, "alice", "alice-pw") {
		t.Fatal("上游热更新后,同一份配置里被停用的用户仍能用旧凭据通过认证")
	}
	if r.core.GetInstance() != box {
		t.Fatal("只改了上游参数和用户,应热更新,不重启数据面")
	}
	if p := upstreamPort(t, r.appliedRaw, "up"); p != 2 {
		t.Fatalf("上游也应已热更新(端口 2),实际 %d", p)
	}
}

// 只改上游(线路和用户都没变)时,不能原地换用户表的入站(socks / http / mixed)不能被拆了重建 —— 重建会断开
// 它上面的全部连接。热换出站后运行中的配置记录是重新序列化过的紧凑 JSON,渲染结果带缩进,按字节比会把没变的入站当成变了。
func TestUpstreamHotUpdateKeepsUnchangedSocksInbound(t *testing.T) {
	r, up, _, port := upstreamRunner(t)
	var cfg struct {
		Inbounds []struct {
			Tag string `json:"tag"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(r.appliedRaw, &cfg); err != nil || len(cfg.Inbounds) == 0 {
		t.Fatalf("前提:运行中的配置里应有入站: %v", err)
	}
	tag := cfg.Inbounds[0].Tag
	before, ok := r.core.GetInstance().Inbound().Get(tag)
	if !ok {
		t.Fatalf("前提:入站 %s 应在运行", tag)
	}
	for i, p := range []int{2, 3} { // 连换两次:第二次是在已经热换过一次的运行记录上比
		r.db.Model(&model.Upstream{}).Where("id = ?", up.Id).Update("options", []byte(`{"server":"127.0.0.1","server_port":`+itoa(p)+`}`))
		if err := r.ReloadUpstreams(); err != nil {
			t.Fatal(err)
		}
		after, ok := r.core.GetInstance().Inbound().Get(tag)
		if !ok || after != before {
			t.Fatalf("第 %d 次只改上游,socks 入站被拆了重建(它上面的连接会全部断开)", i+1)
		}
	}
	if !socks5Auth(t, port, "alice", "alice-pw") {
		t.Fatal("入站应照常可用")
	}
}

// 除了上游和用户还有别的变化(比如线路端口改了):热换出站兜不住,要整体重载,改动才真正生效。
func TestUpstreamReloadFallsBackToFullWhenLinesAlsoChanged(t *testing.T) {
	r, up, _, port := upstreamRunner(t)
	newPort := freePort(t)
	r.db.Model(&model.Upstream{}).Where("id = ?", up.Id).Update("options", []byte(`{"server":"127.0.0.1","server_port":2}`))
	r.db.Model(&model.Line{}).Where("name = ?", "s5").Update("port", newPort)
	if err := r.ReloadUpstreams(); err != nil {
		t.Fatal(err)
	}
	if !socks5Auth(t, newPort, "alice", "alice-pw") {
		t.Fatal("线路端口也改了:整体重载后新端口应能用")
	}
	if c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port))); err == nil {
		c.Close()
		t.Fatal("旧端口应已不再监听")
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
