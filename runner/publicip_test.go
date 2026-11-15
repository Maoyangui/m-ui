package runner

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 两个地址族各自连续没测到 3 轮才从记录里去掉;测到了就立刻更新。
func TestNextPublicIPs(t *testing.T) {
	var m4, m6 int
	step := func(v4, v6, old, old6, want, want6 string) {
		t.Helper()
		pub, pub6 := nextPublicIPs(v4, v6, old, old6, &m4, &m6)
		if pub != want || pub6 != want6 {
			t.Fatalf("nextPublicIPs(%q,%q,%q,%q) = %q %q,应为 %q %q", v4, v6, old, old6, pub, pub6, want, want6)
		}
	}
	// 双栈首测
	step("203.0.113.1", "2001:db8::1", "", "", "203.0.113.1", "2001:db8::1")
	// IPv4 偶尔测不通:前两轮保持旧 v4,不换成 v6;第三轮才认为没有 IPv4
	step("", "2001:db8::1", "203.0.113.1", "2001:db8::1", "203.0.113.1", "2001:db8::1")
	step("", "2001:db8::1", "203.0.113.1", "2001:db8::1", "203.0.113.1", "2001:db8::1")
	step("", "2001:db8::1", "203.0.113.1", "2001:db8::1", "2001:db8::1", "2001:db8::1")
	// IPv4 回来了,立刻用回 IPv4、计数清零
	step("203.0.113.1", "2001:db8::1", "2001:db8::1", "2001:db8::1", "203.0.113.1", "2001:db8::1")
	if m4 != 0 {
		t.Fatalf("测到 IPv4 后计数应清零,得到 %d", m4)
	}
	// IPv6 同理:连续 3 轮没测到才清掉
	step("203.0.113.1", "", "203.0.113.1", "2001:db8::1", "203.0.113.1", "2001:db8::1")
	step("203.0.113.1", "", "203.0.113.1", "2001:db8::1", "203.0.113.1", "2001:db8::1")
	step("203.0.113.1", "", "203.0.113.1", "2001:db8::1", "203.0.113.1", "")
	// 旧记录是 v6(纯 v6 机器)时,IPv4 测不到不会拿 v6 冒充 IPv4
	m4, m6 = 0, 0
	step("", "2001:db8::9", "2001:db8::9", "2001:db8::9", "2001:db8::9", "2001:db8::9")
	// v6 地址换了,立刻用新的
	step("", "2001:db8::a", "2001:db8::9", "2001:db8::9", "2001:db8::a", "2001:db8::a")
}

// 0.6.15 之前建的库升级后本机记录的 public_ip6 是 NULL:探测到 IPv6 也得写进去(服务器列表靠它显示)。
func TestSyncLocalNodeIPsFromNullColumn(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true, PublicIP: "203.0.113.1"})
	db.Create(&model.Node{Name: "副机", Enabled: true, PublicIP: "203.0.113.2"})
	db.Exec("UPDATE nodes SET public_ip6 = NULL")

	syncLocalNodeIPs(db, "203.0.113.1", "2001:db8::1")
	var local, remote model.Node
	db.Where("is_local = ?", true).First(&local)
	db.Where("is_local = ?", false).First(&remote)
	if local.PublicIP6 != "2001:db8::1" || local.PublicIP != "203.0.113.1" {
		t.Fatalf("本机记录应写入探测到的 IPv6(原来是 NULL):%q %q", local.PublicIP, local.PublicIP6)
	}
	if remote.PublicIP6 != "" || remote.PublicIP != "203.0.113.2" {
		t.Fatalf("副机记录不该被本机探测值改动:%q %q", remote.PublicIP, remote.PublicIP6)
	}
	// IPv6 没了也要清掉
	syncLocalNodeIPs(db, "203.0.113.1", "")
	db.Where("is_local = ?", true).First(&local)
	if local.PublicIP6 != "" {
		t.Fatalf("IPv6 没了应清空,得到 %q", local.PublicIP6)
	}
}

// 自签证书默认带上本机探测到的 IPv4 与 IPv6(双栈机器选了 IPv6 时客户端连的是 v6 地址)。
func TestAutoCertHostsIncludesIPv6(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.db)
	r.setSetting("publicIp", "203.0.113.1")
	r.setSetting("publicIp6", "2001:db8::1")
	r.db.Model(&model.Node{}).Where("is_local = ?", true).Updates(map[string]interface{}{"public_ip": "203.0.113.1", "public_ip6": "2001:db8::1", "domain": "hk.example.com"})
	got := strings.Join(r.autoCertHosts(), ",")
	if got != "203.0.113.1,2001:db8::1,hk.example.com" {
		t.Fatalf("自签证书地址 = %s,应含 IPv4、IPv6 与域名各一次", got)
	}
}

// 有没有 IPv4 变了才重新渲染(纯 IPv6 时直连线路要换公共 DNS 的地址);同一地址族里换 IP 不用。
func TestIPv6OnlyChanged(t *testing.T) {
	for _, c := range []struct {
		old, pub string
		want     bool
	}{
		{"", "2001:db8::1", true}, {"203.0.113.1", "2001:db8::1", true}, {"2001:db8::1", "203.0.113.1", true},
		{"", "203.0.113.1", false}, {"203.0.113.1", "203.0.113.2", false}, {"2001:db8::1", "2001:db8::2", false},
	} {
		if got := ipv6OnlyChanged(c.old, c.pub); got != c.want {
			t.Errorf("ipv6OnlyChanged(%q,%q) = %v,应为 %v", c.old, c.pub, got, c.want)
		}
	}
}
