package runner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/reach"
)

// 大陆检测按地址族选端口:订阅里走 IPv6 的线路不拿来测 IPv4 地址(以前会拿 v6 线路的端口去测 v4 地址,给出"通"的假象);
// 走 IPv6 的线路另测 IPv6 入站:订阅里的 IPv6 地址 + 第一条走 IPv6 的 TCP 线路的端口,UDP 线路只列出来。
func TestReachTargetsByFamily(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.db)
	db := r.db
	db.Model(&model.Node{}).Where("is_local = ?", true).Updates(map[string]interface{}{"name": "本机", "sort": 1})
	r.setSetting("publicIp", "203.0.113.1")
	r.setSetting("publicIp6", "2001:db8:1::1")
	r.setSetting("subPort", "2096")
	mk := func(n model.Node) model.Node {
		n.Enabled = true
		db.Create(&n)
		return n
	}
	dual := mk(model.Node{Name: "双栈", ApiUrl: "https://a.example.com:2053/app/", PublicIP: "203.0.113.2", PublicIP6: "2001:db8:2::2", Sort: 2})
	v6only := mk(model.Node{Name: "只有v6线路", ApiUrl: "https://b.example.com/app/", PublicIP: "203.0.113.3", PublicIP6: "2001:db8:3::3", Sort: 3})
	udp6 := mk(model.Node{Name: "v6只有UDP", PublicIP: "203.0.113.4", PublicIP6: "2001:db8:4::4", Sort: 4})
	def6 := mk(model.Node{Name: "默认v6", PublicIP: "203.0.113.5", PublicIP6: "2001:db8:5::5", AddrFamily: "v6", Sort: 5})
	no6 := mk(model.Node{Name: "没有v6", PublicIP: "203.0.113.6", Sort: 6})
	ula := mk(model.Node{Name: "内网v6", Addr: "[fd00::7]", PublicIP: "203.0.113.7", Sort: 7})
	ids := func(v ...uint) json.RawMessage { raw, _ := json.Marshal(v); return raw }
	fam := func(m map[uint]string) json.RawMessage {
		out := map[string]string{}
		for k, v := range m {
			out[jsonID(k)] = v
		}
		raw, _ := json.Marshal(out)
		return raw
	}
	line := func(l model.Line) {
		l.Enabled = true
		db.Create(&l)
	}
	// 双栈:v6 的 trojan 排在前面,以前大陆检测会拿它的端口 8443 去测 IPv4 地址
	line(model.Line{Name: "tj6", Protocol: "trojan", Port: 8443, Sort: 1, NodeIds: ids(dual.Id, v6only.Id, no6.Id), NodeFamily: fam(map[uint]string{dual.Id: "v6", v6only.Id: "v6", no6.Id: "v6"})})
	line(model.Line{Name: "hy6", Protocol: "hysteria2", Port: 8444, Sort: 2, NodeIds: ids(dual.Id, udp6.Id), NodeFamily: fam(map[uint]string{dual.Id: "v6", udp6.Id: "v6"})})
	line(model.Line{Name: "vl4", Protocol: "vless", Port: 9443, Sort: 3, NodeIds: ids(dual.Id, udp6.Id, no6.Id)})
	// 默认 v6 的服务器:单独选了 IPv4 的线路照测大陆,其余默认走 v6
	line(model.Line{Name: "x4", Protocol: "anytls", Port: 7443, Sort: 4, NodeIds: ids(def6.Id), NodeFamily: fam(map[uint]string{def6.Id: "v4"})})
	line(model.Line{Name: "d6", Protocol: "vmess", Port: 7444, Sort: 5, NodeIds: ids(def6.Id, ula.Id)})

	got := map[string]reach.Target{}
	for _, tg := range r.reachTargets(context.Background()) {
		got[tg.Name] = tg
	}
	check := func(name, host string, port int, from, portLine string, v6Only bool) reach.Target {
		t.Helper()
		g, ok := got[name]
		if !ok {
			t.Fatalf("没有 %s: %+v", name, got)
		}
		if g.Err != "" || g.Host != host || g.Port != port || g.PortFrom != from || g.PortLine != portLine || g.TCPv6Only != v6Only {
			t.Fatalf("%s = %+v,应测 %s:%d(%s %s,tcpV6Only=%v)", name, g, host, port, from, portLine, v6Only)
		}
		return g
	}
	wantV6 := func(g reach.Target, host string, port int, portLine string, udp ...string) {
		t.Helper()
		v := g.V6
		if v == nil || v.Err != "" || v.Host != host || v.Port != port || v.PortLine != portLine || len(v.UDPLines) != len(udp) {
			t.Fatalf("%s 的 IPv6 入站目标 = %+v", g.Name, v)
		}
		for i := range udp {
			if v.UDPLines[i] != udp[i] {
				t.Fatalf("%s 的 UDP 线路 = %v", g.Name, v.UDPLines)
			}
		}
	}
	// 双栈:IPv4 测 vl4 的 9443(不是 v6 线路的 8443),IPv6 测 tj6 的 8443,hy6 注明只能抓包
	wantV6(check("双栈", "203.0.113.2", 9443, "line", "vl4", false), "2001:db8:2::2", 8443, "tj6", "hy6")
	// 线路在订阅里都走 v6:大陆那项不测(无可测端口,不再拿面板 API 端口去测、每轮报"境外也不通");IPv6 测 tj6
	if g := got["只有v6线路"]; g.NoPort == "" || g.Port != 0 || g.Err != "" {
		t.Fatalf("线路都走 IPv6 的服务器大陆那项不测: %+v", g)
	} else {
		wantV6(g, "2001:db8:3::3", 8443, "tj6")
	}
	// v6 只有 UDP 线路:IPv6 不发请求,只列线路
	wantV6(check("v6只有UDP", "203.0.113.4", 9443, "line", "vl4", false), "2001:db8:4::4", 0, "", "hy6")
	// 默认 v6、有单独选 IPv4 的线路:大陆照测(以前整台跳过);默认走 v6 的 d6 测 IPv6
	wantV6(check("默认v6", "203.0.113.5", 7443, "line", "x4", false), "2001:db8:5::5", 7444, "d6")
	// 选了 v6 而这台没有 IPv6:订阅里退回 IPv4,这条线路就是 IPv4 的,不测 IPv6
	if g := check("没有v6", "203.0.113.6", 8443, "line", "tj6", false); g.V6 != nil {
		t.Fatalf("没有 IPv6 的服务器不该测 IPv6 入站: %+v", g.V6)
	}
	// 手填了内网 v6:大陆不测(订阅给的就是 v6),IPv6 那项注明测不了、不发请求
	if g := got["内网v6"]; g.NoPort == "" || g.V6 == nil || g.V6.Err == "" || g.V6.Host != "fd00::7" || g.V6.Port != 7444 {
		t.Fatalf("内网 v6 = %+v / %+v", g, g.V6)
	}
	// 本机什么都没部署:照旧测订阅端口,不测 IPv6
	if g := check("本机", "203.0.113.1", 2096, "sub", "", false); g.V6 != nil {
		t.Fatalf("本机没有线路: %+v", g.V6)
	}

	// 订阅给域名:没单独选的线路由客户端解析决定,默认 v6 不算;单独选了 v6 的照样直接给 IPv6,照样测
	r.setSetting("subServerAddr", "domain")
	got = map[string]reach.Target{}
	for _, tg := range r.reachTargets(context.Background()) {
		got[tg.Name] = tg
	}
	if g := check("默认v6", "203.0.113.5", 7443, "line", "x4", false); g.V6 != nil {
		t.Fatalf("域名模式下默认 v6 不生效,d6 不该测 IPv6: %+v", g.V6)
	}
	wantV6(check("双栈", "203.0.113.2", 9443, "line", "vl4", false), "2001:db8:2::2", 8443, "tj6", "hy6")
}

// TCP 线路全走 IPv6、走 IPv4 的只有 hysteria2(审查第 2 条的场景):IPv4 那项借面板 API 端口看 IP 有没有被墙并注明
// (香港也连不上时由 reach 记成"无可测端口",见 reach.TestAPIPortUnreachableIsNoPort)。
func TestReachTargetsTCPv6OnlyWithUDPv4(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.db)
	db := r.db
	n := model.Node{Name: "B", Enabled: true, ApiUrl: "https://b.example.com:2053/app/", PublicIP: "203.0.113.20", PublicIP6: "2001:db8:20::1"}
	db.Create(&n)
	fam, _ := json.Marshal(map[string]string{jsonID(n.Id): "v6"})
	ids, _ := json.Marshal([]uint{n.Id})
	for _, l := range []model.Line{
		{Name: "tj", Protocol: "trojan", Port: 8443, Sort: 1, NodeIds: ids, NodeFamily: fam},
		{Name: "vl", Protocol: "vless", Port: 9443, Sort: 2, NodeIds: ids, NodeFamily: fam},
		{Name: "hy4", Protocol: "hysteria2", Port: 8444, Sort: 3, NodeIds: ids},
	} {
		l.Enabled = true
		db.Create(&l)
	}
	for _, g := range r.reachTargets(context.Background()) {
		if g.NodeId != n.Id {
			continue
		}
		if g.NoPort != "" || g.Host != "203.0.113.20" || g.Port != 2053 || g.PortFrom != "api" || !g.TCPv6Only {
			t.Fatalf("走 IPv4 的只有 UDP 线路:借 API 端口测 IPv4 并注明 = %+v", g)
		}
		if g.V6 == nil || g.V6.Port != 8443 || g.V6.PortLine != "tj" {
			t.Fatalf("IPv6 入站测 tj: %+v", g.V6)
		}
		return
	}
	t.Fatal("没有 B")
}

// 自定义地址只有写成这台自己的 IPv6(手填的连接地址、探测到的公网 IPv6)才按 IPv6 算(审查第 9 条):端口不拿去测这台的 IPv4,
// 那个地址参与 IPv6 入站检测(用 server_port,没写用线路端口)。别的 IPv6 —— 国内中转、CDN 的入口,写不写 node_id 都一样 ——
// 和 IPv4、域名一样按 IPv4 一侧算:端口照样测这台的 IPv4,不当成这台的 IPv6 入口去测,也不让这台变成"无可测端口"
// (以前中转的 IPv6 让只有 IPv4 的落地机整项不测、盖掉"被墙";带 node_id 或只有一台服务器时还拿中转的地址测 IPv6 入站)。
// 这台自己的 IPv6 线路优先;键名不分大小写(和订阅的读法一致),地址写法不同(大写、方括号)也认。
func TestReachTargetsCustomAddrs(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.db)
	db := r.db
	db.Model(&model.Node{}).Where("is_local = ?", true).Update("enabled", false) // 只看下面这几台
	mk := func(name, addr, ip4, ip6 string) model.Node {
		n := model.Node{Name: name, Enabled: true, ApiUrl: "https://" + name + ".example.com:2053/app/", Addr: addr, PublicIP: ip4, PublicIP6: ip6}
		db.Create(&n)
		return n
	}
	a := mk("a", "", "203.0.113.31", "2001:db8:31::1")
	b := mk("b", "", "203.0.113.32", "2001:db8:32::1")
	c := mk("c", "", "203.0.113.33", "")
	d := mk("d", "", "203.0.113.34", "2001:db8:34::1")
	e := mk("e", "", "203.0.113.35", "")
	f := mk("f", "[2001:db8:36::9]", "203.0.113.36", "2001:db8:36::1")
	ids := func(v ...uint) json.RawMessage { raw, _ := json.Marshal(v); return raw }
	line := func(name, proto string, port, sort int, nodeIds json.RawMessage, addrs string, nodeFam json.RawMessage) {
		l := model.Line{Name: name, Protocol: proto, Port: port, Sort: sort, NodeIds: nodeIds, NodeFamily: nodeFam, Enabled: true}
		if addrs != "" {
			l.Addrs = json.RawMessage(addrs)
		}
		db.Create(&l)
	}
	// a:排第一的是写成 a 自己 IPv6 的自定义地址(以前会拿 8443 测 a 的 IPv4,给出"通"的假象);后面有一条 IPv4 的
	line("c6", "trojan", 8443, 1, ids(a.Id, b.Id), `[{"server":"2001:db8:31::1","node_id":`+jsonID(a.Id)+`}]`, nil)
	line("v4", "vless", 9443, 2, ids(a.Id), "", nil)
	// b:c6 在 b 上只有写给 a 的地址,b 的订阅里没有它的入口,和以前一样借它的端口测 b 的 IPv4;
	// 没写 node_id、写成 b 自己 IPv6 的(键名、地址大写,带方括号也认),用 server_port 测 IPv6 入站
	line("c6b", "anytls", 7443, 3, ids(b.Id), `[{"Server":"[2001:DB8:32::1]","Server_Port":23444}]`, nil)
	// c:只有 IPv4 的落地机,唯一一条线路的自定义地址是国内中转的 IPv6(审查的复现场景):用户连中转、中转连 c 的 IPv4:6443。
	// 照样拿 6443 测 c 的 IPv4(被墙能告警),不记"无可测端口",也不拿中转的地址测 IPv6 入站(只有这一台用它也一样)
	line("relay6", "vmess", 6443, 4, ids(c.Id), `[{"server":"240e:1:2::1","server_port":30000}]`, nil)
	// d:中转给 d 写了带 node_id 的 IPv6 入口:node_id 不能证明地址属于 d,照样按 IPv4 一侧算,端口测 d 的 IPv4;
	// d 自己的 IPv6 线路测 IPv6 入站;自定义地址写成域名的照旧按 IPv4 算
	line("relayD", "trojan", 6444, 5, ids(d.Id), `[{"server":"240e:1:2::2","server_port":30001,"node_id":`+jsonID(d.Id)+`}]`, nil)
	famD, _ := json.Marshal(map[string]string{jsonID(d.Id): "v6"})
	line("own6", "trojan", 5443, 6, ids(d.Id), "", famD)
	line("c4", "trojan", 4443, 7, ids(d.Id), `[{"server":"hk.example.com"}]`, nil)
	// e:没有 IPv6,只有一条 UDP 线路、自定义地址是别处的 IPv6:不测 IPv6 入站(以前报"IPv6 地址不是公网地址"),
	// 走 IPv4 的只有 UDP 线路,借 API 端口看 IPv4
	line("hyE", "hysteria2", 8444, 8, ids(e.Id), `[{"server":"2001:db8::5"}]`, nil)
	// f:手填的连接地址是 IPv6(::9),自定义地址写成探测到的另一个 IPv6(::1)也是这台自己的;走 IPv6 的只有 UDP:
	// 只列线路,地址用自定义地址里那个
	line("hyF", "hysteria2", 8445, 9, ids(f.Id), `[{"server":"2001:db8:36::1","node_id":`+jsonID(f.Id)+`}]`, nil)

	got := map[string]reach.Target{}
	for _, g := range r.reachTargets(context.Background()) {
		got[g.Name] = g
	}
	if g := got["a"]; g.NoPort != "" || g.Port != 9443 || g.PortLine != "v4" || g.V6 == nil || g.V6.Err != "" || g.V6.Host != "2001:db8:31::1" || g.V6.Port != 8443 || g.V6.PortLine != "c6" {
		t.Fatalf("a = %+v / %+v", g, g.V6)
	}
	if g := got["b"]; g.Port != 8443 || g.PortLine != "c6" || g.V6 == nil || g.V6.Host != "2001:db8:32::1" || g.V6.Port != 23444 || g.V6.PortLine != "c6b" {
		t.Fatalf("b = %+v / %+v", g, g.V6)
	}
	if g := got["c"]; g.NoPort != "" || g.Err != "" || g.Host != "203.0.113.33" || g.Port != 6443 || g.PortFrom != "line" || g.PortLine != "relay6" || g.V6 != nil {
		t.Fatalf("c 的线路只经中转的 IPv6:照测 c 的 IPv4 6443,不测 IPv6 入站 = %+v / %+v", g, g.V6)
	}
	if g := got["d"]; g.NoPort != "" || g.Port != 6444 || g.PortLine != "relayD" || g.V6 == nil || g.V6.Host != "2001:db8:34::1" || g.V6.Port != 5443 || g.V6.PortLine != "own6" {
		t.Fatalf("d = %+v / %+v", g, g.V6)
	}
	if g := got["e"]; g.NoPort != "" || g.Host != "203.0.113.35" || g.Port != 2053 || g.PortFrom != "api" || g.TCPv6Only || g.V6 != nil {
		t.Fatalf("e = %+v / %+v", g, g.V6)
	}
	if g := got["f"]; g.NoPort == "" || g.V6 == nil || g.V6.Err != "" || g.V6.Host != "2001:db8:36::1" || g.V6.Port != 0 || len(g.V6.UDPLines) != 1 || g.V6.UDPLines[0] != "hyF" {
		t.Fatalf("f = %+v / %+v", g, g.V6)
	}

	// 单服务器面板:只有 c 一台时中转的 IPv6 也不当成 c 的(以前"只部署在这一台"就拿去测 IPv6 入站)
	db.Model(&model.Node{}).Where("id <> ?", c.Id).Update("enabled", false)
	for _, g := range r.reachTargets(context.Background()) {
		if g.Name != "c" || g.NoPort != "" || g.Port != 6443 || g.V6 != nil {
			t.Fatalf("只剩 c: %+v / %+v", g, g.V6)
		}
	}
}

// 主机的线路在订阅里都走 IPv6(审查:noPort 不能用到主机上):用户刷新订阅连的是主机的订阅地址(订阅域名 → 面板域名 → 公网 IP),
// 照测订阅端口、注明是订阅端口(TCPv6Only),订阅地址的 IPv4 被墙照样能告警;副机同样的配置不测(面板 API 端口不是用户入口)。
// 订阅地址本身是 IPv6 字面量、或这台根本没有 IPv4 时才记"无可测端口"。
func TestReachTargetsLocalAllV6(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.db)
	db := r.db
	db.Model(&model.Node{}).Where("is_local = ?", true).Update("name", "本机")
	var local model.Node
	db.Where("is_local = ?", true).First(&local)
	r.setSetting("publicIp", "203.0.113.1")
	r.setSetting("publicIp6", "2001:db8:1::1")
	r.setSetting("subPort", "2096")
	sub := model.Node{Name: "副机", Enabled: true, ApiUrl: "https://b.example.com:2053/app/", PublicIP: "203.0.113.2", PublicIP6: "2001:db8:2::1"}
	db.Create(&sub)
	ids, _ := json.Marshal([]uint{local.Id, sub.Id})
	fam, _ := json.Marshal(map[string]string{jsonID(local.Id): "v6", jsonID(sub.Id): "v6"})
	for _, l := range []model.Line{
		{Name: "tj6", Protocol: "trojan", Port: 8443, Sort: 1, NodeIds: ids, NodeFamily: fam},
		{Name: "hy6", Protocol: "hysteria2", Port: 8444, Sort: 2, NodeIds: ids, NodeFamily: fam},
	} {
		l.Enabled = true
		db.Create(&l)
	}
	targets := func() map[string]reach.Target {
		got := map[string]reach.Target{}
		for _, g := range r.reachTargets(context.Background()) {
			got[g.Name] = g
		}
		return got
	}
	wantSub := func(step string, got map[string]reach.Target, tcpV6Only bool) {
		t.Helper()
		g := got["本机"]
		if g.NoPort != "" || g.Err != "" || g.Host != "203.0.113.1" || g.Port != 2096 || g.PortFrom != "sub" || g.TCPv6Only != tcpV6Only {
			t.Fatalf("%s:主机照测订阅端口 = %+v", step, g)
		}
		if s := got["副机"]; s.NoPort == "" || s.Port != 0 {
			t.Fatalf("%s:副机的线路都走 IPv6,不测 = %+v", step, s)
		}
	}
	// 线路单独选了 IPv6
	got := targets()
	wantSub("单独选了 IPv6", got, true)
	if v := got["本机"].V6; v == nil || v.Host != "2001:db8:1::1" || v.Port != 8443 || v.PortLine != "tj6" || len(v.UDPLines) != 1 {
		t.Fatalf("主机的 IPv6 入站照测 = %+v", v)
	}
	// 订阅域名:订阅地址不是 IPv6 字面量,照测(测的是这台的 IPv4,不在主机上解析订阅域名)
	r.setSetting("subDomain", "sub.example.com")
	wantSub("填了订阅域名", targets(), true)
	r.setSetting("subDomain", "")
	// 服务器选了"自动地址用 IPv6"、线路都没单独选(以前这一支记"无可测端口",会把以前测出的"被墙"清掉)
	db.Model(&model.Node{}).Where("id IN ?", []uint{local.Id, sub.Id}).Update("addr_family", "v6")
	db.Model(&model.Line{}).Where("1 = 1").Update("node_family", nil)
	wantSub("自动地址用 IPv6", targets(), true)
	// 只有 UDP 线路走 IPv6:照测订阅端口(不说"TCP 线路都走 IPv6")
	db.Model(&model.Line{}).Where("name = ?", "tj6").Update("enabled", false)
	wantSub("只有 UDP 线路", targets(), false)
	db.Model(&model.Line{}).Where("name = ?", "tj6").Update("enabled", true)

	// 纯 IPv6 的主机:订阅地址就是 IPv6(公网 IP 记的是 IPv6),大陆那项测不了
	r.setSetting("publicIp", "2001:db8:1::1")
	if g := targets()["本机"]; g.NoPort == "" || g.Port != 0 || g.Host != "" || g.Err != "" || g.V6 == nil || g.V6.Port != 8443 {
		t.Fatalf("订阅地址是 IPv6:主机不测大陆那项,IPv6 入站照测 = %+v / %+v", g, g.V6)
	}
	// 填了面板域名、却解析不到 IPv4:这台没有 IPv4,同样不测(不报"服务器还没上报公网 IP")
	r.setSetting("webDomain", "localhost") // 只解析到回环地址,不算
	if g := targets()["本机"]; g.NoPort == "" || g.Err != "" || g.Port != 0 {
		t.Fatalf("没有 IPv4:主机不测大陆那项 = %+v", g)
	}
}

func jsonID(id uint) string {
	b, _ := json.Marshal(id)
	return string(b)
}
