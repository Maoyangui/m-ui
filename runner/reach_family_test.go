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
	// TCP 线路全走 v6:IPv4 改测 API 端口并注明;IPv6 测 tj6
	wantV6(check("只有v6线路", "203.0.113.3", 443, "api", "", true), "2001:db8:3::3", 8443, "tj6")
	// v6 只有 UDP 线路:IPv6 不发请求,只列线路
	wantV6(check("v6只有UDP", "203.0.113.4", 9443, "line", "vl4", false), "2001:db8:4::4", 0, "", "hy6")
	// 默认 v6、有单独选 IPv4 的线路:大陆照测(以前整台跳过);默认走 v6 的 d6 测 IPv6
	wantV6(check("默认v6", "203.0.113.5", 7443, "line", "x4", false), "2001:db8:5::5", 7444, "d6")
	// 选了 v6 而这台没有 IPv6:订阅里退回 IPv4,这条线路就是 IPv4 的,不测 IPv6
	if g := check("没有v6", "203.0.113.6", 8443, "line", "tj6", false); g.V6 != nil {
		t.Fatalf("没有 IPv6 的服务器不该测 IPv6 入站: %+v", g.V6)
	}
	// 手填了内网 v6:大陆不测(订阅给的就是 v6),IPv6 那项注明测不了、不发请求
	if g := got["内网v6"]; g.Err == "" || g.V6 == nil || g.V6.Err == "" || g.V6.Host != "fd00::7" || g.V6.Port != 7444 {
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

func jsonID(id uint) string {
	b, _ := json.Marshal(id)
	return string(b)
}
