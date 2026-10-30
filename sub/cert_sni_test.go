package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

func TestCertSNI(t *testing.T) {
	wild := model.CertBrief{Name: "*.example.com", Domains: []string{"*.example.com", "example.com"}}
	cases := []struct {
		b    model.CertBrief
		cur  string
		want string
	}{
		{wild, "hk.example.com", "hk.example.com"}, // 泛域名覆盖节点域名,不动
		{wild, "example.com", "example.com"},
		{wild, "a.b.example.com", "example.com"}, // 泛域名只管一级,换成证书里的具体域名
		{wild, "other.com", "example.com"},
		{wild, "", "example.com"},
		{model.CertBrief{Domains: []string{"*.only.com"}}, "x.com", "www.only.com"}, // 只有泛域名:在它下面起一个
		{model.CertBrief{Domains: []string{"a.com"}}, "b.com", "a.com"},
		{model.CertBrief{SelfSigned: true, Domains: []string{"a.com"}}, "b.com", "b.com"}, // 自签靠允许不安全,SNI 不动
	}
	for _, c := range cases {
		if got := certSNI(c.b, c.cur); got != c.want {
			t.Errorf("certSNI(%v, %q) = %q,应为 %q", c.b.Domains, c.cur, got, c.want)
		}
	}
}

// 没单独选证书的线路用服务器的默认证书:换了默认证书(比如泛域名)而面板域名没变,SNI 要跟着证书走;
// 设置里强制了允许不安全时 SNI 是管理员自己定的,不动。
func TestDefaultCertInSubscription(t *testing.T) {
	line := model.Line{Id: 1, Name: "l", Protocol: "trojan", Port: 1, Tls: []byte(`{"mode":"cert"}`)}
	wild := map[string]model.CertBrief{"*.example.net": {Name: "*.example.net", Domains: []string{"*.example.net", "example.net"}, Default: true}}
	self := map[string]model.CertBrief{"self-signed": {Name: "self-signed", Domains: []string{"panel.example.com"}, SelfSigned: true, Default: true}}
	base := addr{server: "1.1.1.1", port: 1, sni: "panel.example.com"}
	if got := certAddr(base, line, Entry{Certs: wild}); got.sni != "example.net" || got.insecure {
		t.Fatalf("默认证书是泛域名:%+v", got)
	}
	if got := certAddr(base, line, Entry{Certs: self}); got.sni != "panel.example.com" || !got.insecure {
		t.Fatalf("默认证书是自签:%+v", got)
	}
	fixed := base
	fixed.insecure = true
	if got := certAddr(fixed, line, Entry{Certs: wild, InsecureFixed: true}); got.sni != "panel.example.com" || !got.insecure {
		t.Fatalf("强制允许不安全时 SNI 不该动:%+v", got)
	}
	if got := certAddr(base, line, Entry{}); got != base {
		t.Fatalf("老版本副机没报清单,地址不动:%+v", got)
	}
	reality := model.Line{Id: 2, Name: "r", Protocol: "vless", Port: 1, Tls: []byte(`{"mode":"reality"}`)}
	if got := certAddr(base, reality, Entry{Certs: wild}); got != base {
		t.Fatalf("Reality 线路不用证书,地址不动:%+v", got)
	}
}

// 线路单独选了证书:有这张证书的服务器,订阅里 SNI 跟着证书走;没有的服务器用它自己的默认证书,地址不动。
func TestLineCertInSubscription(t *testing.T) {
	line := model.Line{Id: 1, Name: "l", Protocol: "trojan", Port: 1, Tls: []byte(`{"mode":"cert","cert":"*.example.com"}`)}
	hasIt := map[string]model.CertBrief{"*.example.com": {Name: "*.example.com", Domains: []string{"*.example.com", "example.com"}}}
	entries := []Entry{
		{Name: "a", Host: "1.1.1.1", SNI: "a.other.com", NodeId: 1, Suffix: "-a", Certs: hasIt},
		{Name: "b", Host: "2.2.2.2", SNI: "b.other.com", NodeId: 2, Suffix: "-b"}, // 老版本副机,没报证书清单
		{Name: "c", Host: "3.3.3.3", SNI: "hk.example.com", NodeId: 3, Suffix: "-c", Certs: hasIt},
		{Name: "d", Host: "4.4.4.4", SNI: "d.other.com", NodeId: 4, Suffix: "-d", Certs: map[string]model.CertBrief{"*.example.com": {SelfSigned: true}}},
		{Name: "e", Host: "5.5.5.5", SNI: "e.other.com", NodeId: 5, Suffix: "-e", InsecureFixed: true, Certs: map[string]model.CertBrief{"*.example.com": {SelfSigned: true}}},
	}
	addrs := resolveAddrs(line, entries, nil)
	want := []struct {
		sni      string
		insecure bool
	}{{"example.com", false}, {"b.other.com", false}, {"hk.example.com", false}, {"d.other.com", true}, {"e.other.com", false}}
	for i, w := range want {
		if addrs[i].sni != w.sni || addrs[i].insecure != w.insecure {
			t.Errorf("第 %d 台: %+v,应为 %+v", i, addrs[i], w)
		}
	}
	// 没选证书的线路完全不受影响
	plain := model.Line{Id: 2, Name: "p", Protocol: "trojan", Port: 1, Tls: []byte(`{"mode":"cert"}`)}
	if a := resolveAddrs(plain, entries, nil); a[0].sni != "a.other.com" {
		t.Fatalf("没选证书的线路 SNI 不该变: %+v", a[0])
	}
	// Reality 线路带着 cert 字段也不管
	reality := model.Line{Id: 3, Name: "r", Protocol: "vless", Port: 1, Tls: []byte(`{"mode":"reality","cert":"*.example.com"}`)}
	if a := resolveAddrs(reality, entries, nil); a[0].sni != "a.other.com" {
		t.Fatalf("Reality 不该受证书影响: %+v", a[0])
	}
	// 链接里确实带上了
	user := model.User{Name: "u", Credentials: []byte(`{"trojan":{"password":"p"}}`)}
	links := GenerateLinks(user, []model.Line{line}, entries[:1])
	if len(links) != 1 || !strings.Contains(links[0], "sni=example.com") {
		t.Fatalf("链接里 SNI 不对: %v", links)
	}
}

// 双栈服务器:线路单独选的 IP 版本优先,没选的跟随服务器;这台没有那个地址就还用默认的,不把节点弄没。
func TestLineFamilyInSubscription(t *testing.T) {
	entries := []Entry{
		{Name: "a", Host: "1.1.1.1", IP4: "1.1.1.1", IP6: "2001:db8::a", NodeId: 1, Suffix: "-a"},
		{Name: "b", Host: "2001:db8::b", IP4: "2.2.2.2", IP6: "2001:db8::b", NodeId: 2, Suffix: "-b"},
		{Name: "c", Host: "3.3.3.3", IP4: "3.3.3.3", NodeId: 3, Suffix: "-c"}, // 没有 IPv6
	}
	serverOf := func(family string) map[string]string {
		l := model.Line{Id: 1, Name: "l", Protocol: "trojan", Port: 443}
		if family != "" {
			l.NodeFamily = []byte(family)
		}
		out := map[string]string{}
		for _, a := range resolveAddrs(l, entries, nil) {
			out[a.remark] = a.server
		}
		return out
	}
	got := serverOf("")
	if got["-a"] != "1.1.1.1" || got["-b"] != "2001:db8::b" || got["-c"] != "3.3.3.3" {
		t.Fatalf("没选时跟随服务器: %v", got)
	}
	got = serverOf(`{"1":"v6","2":"v4","3":"v6"}`)
	if got["-a"] != "2001:db8::a" || got["-b"] != "2.2.2.2" {
		t.Fatalf("线路选的优先: %v", got)
	}
	if got["-c"] != "3.3.3.3" {
		t.Fatalf("这台没有 IPv6,不能把节点弄没: %v", got)
	}
	// 自定义对外地址优先于这项
	l := model.Line{Id: 1, Name: "l", Protocol: "trojan", Port: 443, NodeFamily: []byte(`{"1":"v6"}`), Addrs: []byte(`[{"server":"9.9.9.9"}]`)}
	if a := resolveAddrs(l, entries, nil); len(a) != 1 || a[0].server != "9.9.9.9" {
		t.Fatalf("自定义地址: %+v", a)
	}
}

// 节点写成 IPv6 地址时分享链接和 Clash 都要写对;Clash 打开 ipv6 总开关。
func TestIPv6ServerInOutput(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"trojan":{"password":"p"}}`)}
	line := model.Line{Id: 1, Name: "l", Protocol: "trojan", Port: 443, Tls: []byte(`{"mode":"cert"}`), NodeFamily: []byte(`{"1":"v6"}`)}
	entries := []Entry{{Name: "a", Host: "1.1.1.1", IP4: "1.1.1.1", IP6: "2001:db8::a", NodeId: 1}}
	links := GenerateLinksFor(user, []model.Line{line}, entries, nil)
	if len(links) != 1 || !strings.Contains(links[0], "@[2001:db8::a]:443") {
		t.Fatalf("分享链接: %v", links)
	}
	y, err := BuildClashFor(user, []model.Line{line}, entries, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(y, "ipv6: true") || !strings.Contains(y, "2001:db8::a") {
		t.Fatalf("Clash:\n%s", y)
	}
	line.NodeFamily = nil
	y, err = BuildClashFor(user, []model.Line{line}, entries, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(y, "ipv6: true") {
		t.Fatal("没有 IPv6 节点时不用动这个开关")
	}
}
