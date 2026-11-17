package sub

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

const testPin = "eeaa3a75b2cbb17233eb9437e0cf8a122115f9a63e3f28816608452456fed758"

// 自签证书的线路:vless / trojan 链接带证书指纹(pcs),vmess 的 JSON 带 insecure 和 pcs。
// 新版 Xray 内核删掉了 allowInsecure,v2rayN 导入 VLESS 链接时只认 pcs —— 不带的话默认安装(自签证书)的 TLS 线路在这类客户端上连不上。
func TestSelfSignedLinksCarryPin(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"vless":{"uuid":"11111111-1111-1111-1111-111111111111"},"trojan":{"password":"p"},"vmess":{"uuid":"22222222-2222-2222-2222-222222222222"},"hysteria2":{"password":"p"},"tuic":{"uuid":"33333333-3333-3333-3333-333333333333","password":"p"},"anytls":{"password":"p"}}`)}
	self := map[string]model.CertBrief{"self-signed": {Name: "self-signed", SelfSigned: true, Default: true, Sha256: testPin}}
	entries := []Entry{{Name: "a", Host: "203.0.113.1", SNI: "203.0.113.1", NodeId: 1, Insecure: true, Certs: self}}
	cert := []byte(`{"mode":"cert"}`)
	lines := []model.Line{
		{Id: 1, Name: "vl", Protocol: "vless", Port: 1, Tls: cert, Options: []byte(`{"vision":true}`)},
		{Id: 2, Name: "tj", Protocol: "trojan", Port: 2, Tls: cert},
		{Id: 3, Name: "vm", Protocol: "vmess", Port: 3, Tls: cert},
		{Id: 4, Name: "hy", Protocol: "hysteria2", Port: 4, Tls: cert},
		{Id: 5, Name: "tu", Protocol: "tuic", Port: 5, Tls: cert},
		{Id: 6, Name: "at", Protocol: "anytls", Port: 6, Tls: cert},
		{Id: 7, Name: "re", Protocol: "vless", Port: 7, Tls: []byte(`{"mode":"reality","reality":{"public_key":"k","short_ids":["ab"],"handshake_server":"www.example.com"}}`), Options: []byte(`{"vision":true}`)},
	}
	by := map[string]string{}
	for _, l := range GenerateLinks(user, lines, entries) {
		by[l[:strings.Index(l, "://")]+"/"+l[strings.LastIndex(l, "#")+1:]] = l
		if strings.HasPrefix(l, "vmess://") {
			by["vmess"] = l
		}
	}
	for _, k := range []string{"vless/vl", "trojan/tj"} {
		if !strings.Contains(by[k], "allowInsecure=1") || !strings.Contains(by[k], "pcs="+testPin) {
			t.Errorf("%s 要同时带 allowInsecure 和 pcs: %s", k, by[k])
		}
	}
	// vision 的 flow 仍在最后(只是在它前面多了 pcs)
	if !strings.HasSuffix(strings.Split(by["vless/vl"], "#")[0], "&pcs="+testPin+"&flow=xtls-rprx-vision") {
		t.Errorf("vless 链接参数顺序: %s", by["vless/vl"])
	}
	var vm map[string]interface{}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(by["vmess"], "vmess://"))
	if json.Unmarshal(raw, &vm) != nil || vm["insecure"] != "1" || vm["pcs"] != testPin || vm["tls"] != "tls" {
		t.Errorf("vmess 的 JSON 要带 insecure 和 pcs: %s", raw)
	}
	// 这三种协议的「允许不安全」各家客户端仍然认,链接保持原样(其中 hysteria2 / anytls 与旧面板字节级一致,有黄金测试)
	for _, k := range []string{"hysteria2/hy", "tuic/tu", "anytls/at"} {
		if by[k] == "" || strings.Contains(by[k], "pcs=") || strings.Contains(by[k], "pinSHA256") || !strings.Contains(by[k], "nsecure=1") {
			t.Errorf("%s 不该变: %s", k, by[k])
		}
	}
	if strings.Contains(by["vless/re"], "pcs=") || strings.Contains(by["vless/re"], "allowInsecure") {
		t.Errorf("Reality 线路和证书无关: %s", by["vless/re"])
	}

	// 正式证书:不带;老版本副机(没报指纹):只带 allowInsecure
	real := map[string]model.CertBrief{"a.example.com": {Name: "a.example.com", Domains: []string{"a.example.com"}, Default: true}}
	l := GenerateLinks(user, lines[:1], []Entry{{Name: "a", Host: "203.0.113.1", SNI: "a.example.com", NodeId: 1, Certs: real}})[0]
	if strings.Contains(l, "pcs=") || strings.Contains(l, "allowInsecure") || !strings.Contains(l, "sni=a.example.com") {
		t.Errorf("正式证书的链接: %s", l)
	}
	old := map[string]model.CertBrief{"self-signed": {Name: "self-signed", SelfSigned: true, Default: true}}
	l = GenerateLinks(user, lines[:1], []Entry{{Name: "a", Host: "203.0.113.1", NodeId: 1, Insecure: true, Certs: old}})[0]
	if strings.Contains(l, "pcs=") || !strings.Contains(l, "allowInsecure=1") {
		t.Errorf("没报指纹的副机: %s", l)
	}
	l = GenerateLinks(user, lines[:1], []Entry{{Name: "a", Host: "203.0.113.1", NodeId: 1, Insecure: true}})[0]
	if strings.Contains(l, "pcs=") || !strings.Contains(l, "allowInsecure=1") {
		t.Errorf("没报证书清单的副机: %s", l)
	}
	// 设置里强制了「不允许不安全」:不带 allowInsecure,但自签证书的指纹照样给(指纹是更严的校验,不是放松)
	l = GenerateLinks(user, lines[:1], []Entry{{Name: "a", Host: "203.0.113.1", NodeId: 1, InsecureFixed: true, Certs: self}})[0]
	if strings.Contains(l, "allowInsecure") || !strings.Contains(l, "pcs="+testPin) {
		t.Errorf("强制不允许不安全: %s", l)
	}
}

// 走 WebSocket / gRPC / HTTPUpgrade 而地址是域名的不带指纹:那可能套了 CDN,CDN 出示的是它自己的证书,带了反而对不上。
// 地址是 IP(v4 / v6)的照带。
func TestPinSkippedWhenMaybeCDN(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"vless":{"uuid":"11111111-1111-1111-1111-111111111111"},"trojan":{"password":"p"},"vmess":{"uuid":"22222222-2222-2222-2222-222222222222"}}`)}
	self := map[string]model.CertBrief{"self-signed": {Name: "self-signed", SelfSigned: true, Default: true, Sha256: testPin}}
	cert := []byte(`{"mode":"cert"}`)
	ws := []byte(`{"type":"ws","path":"/w"}`)
	lines := []model.Line{
		{Id: 1, Name: "ws", Protocol: "vless", Port: 1, Tls: cert, Transport: ws},
		{Id: 2, Name: "grpc", Protocol: "trojan", Port: 2, Tls: cert, Transport: []byte(`{"type":"grpc","service_name":"g"}`)},
		{Id: 3, Name: "hu", Protocol: "vless", Port: 3, Tls: cert, Transport: []byte(`{"type":"httpupgrade","path":"/h"}`)},
		{Id: 4, Name: "tcp", Protocol: "trojan", Port: 4, Tls: cert},
		{Id: 5, Name: "vmws", Protocol: "vmess", Port: 5, Tls: cert, Transport: ws},
	}
	vmess := func(links []string) map[string]interface{} {
		for _, l := range links {
			if strings.HasPrefix(l, "vmess://") {
				var m map[string]interface{}
				raw, _ := base64.StdEncoding.DecodeString(l[8:])
				_ = json.Unmarshal(raw, &m)
				return m
			}
		}
		return nil
	}
	// 地址是域名
	links := GenerateLinks(user, lines, []Entry{{Name: "a", Host: "cdn.example.com", SNI: "cdn.example.com", NodeId: 1, Insecure: true, Certs: self}})
	for _, l := range links {
		if strings.HasPrefix(l, "vmess://") {
			continue
		}
		name := l[strings.LastIndex(l, "#")+1:]
		if has := strings.Contains(l, "pcs="); has != (name == "tcp") {
			t.Errorf("域名地址,%s 带指纹=%v: %s", name, has, l)
		}
		if !strings.Contains(l, "allowInsecure=1") {
			t.Errorf("允许不安全照旧带着: %s", l)
		}
	}
	if m := vmess(links); m["pcs"] != nil || m["insecure"] != "1" {
		t.Errorf("域名地址的 vmess-ws 不带指纹、带 insecure: %v", m)
	}
	// 地址是 IP:都带
	for _, host := range []string{"203.0.113.1", "2001:db8::1"} {
		links = GenerateLinks(user, lines, []Entry{{Name: "a", Host: host, NodeId: 1, Insecure: true, Certs: self}})
		for _, l := range links {
			if !strings.HasPrefix(l, "vmess://") && !strings.Contains(l, "pcs="+testPin) {
				t.Errorf("IP 地址(%s)要带指纹: %s", host, l)
			}
		}
		if m := vmess(links); m["pcs"] != testPin {
			t.Errorf("IP 地址(%s)的 vmess 要带指纹: %v", host, m)
		}
	}
}

// 自定义地址指了服务器的:「允许不安全」按那台上这条线路实际出示的证书判断,不借第一台服务器的;不带指纹、不改 SNI。
func TestCustomAddrInsecureFollowsItsNode(t *testing.T) {
	self := model.CertBrief{Name: "self-signed", SelfSigned: true, Default: true, Sha256: testPin}
	real := model.CertBrief{Name: "v6.example.com", Domains: []string{"v6.example.com"}}
	wild := model.CertBrief{Name: "*.example.net", Domains: []string{"*.example.net"}, Default: true}
	entries := []Entry{
		{Name: "a", Host: "203.0.113.1", NodeId: 1, Insecure: true, Certs: map[string]model.CertBrief{"self-signed": self, "v6.example.com": real}},
		{Name: "b", Host: "203.0.113.2", NodeId: 2, Insecure: false, Certs: map[string]model.CertBrief{"*.example.net": wild}},
		{Name: "c", Host: "203.0.113.3", NodeId: 3, Insecure: true}, // 老版本副机,没报证书清单
	}
	type want struct {
		insecure bool
		sni      string
	}
	check := func(name string, line model.Line, ws ...want) {
		t.Helper()
		got := resolveAddrs(line, entries, nil)
		if len(got) != len(ws) {
			t.Fatalf("%s: %d 个地址,应为 %d: %+v", name, len(got), len(ws), got)
		}
		for i, w := range ws {
			if got[i].insecure != w.insecure || got[i].sni != w.sni || got[i].pin != "" {
				t.Errorf("%s 第 %d 个: insecure=%v sni=%q pin=%q,应为 %+v", name, i, got[i].insecure, got[i].sni, got[i].pin, w)
			}
		}
	}
	line := func(tls, addrs string) model.Line {
		return model.Line{Id: 1, Name: "l", Protocol: "trojan", Port: 1, Tls: []byte(tls), Addrs: []byte(addrs)}
	}
	// 线路单独选了正式证书、那台的默认证书是自签:地址(域名)证书盖得住 → 不再带「允许不安全」
	check("选了正式证书", line(`{"mode":"cert","cert":"v6.example.com"}`, `[{"server":"v6.example.com","node_id":1}]`), want{false, ""})
	// 同一条线路,地址是 IP 又没填 SNI:证书盖不住客户端会校验的名字,维持这台默认证书的判断(自签 → 带)
	check("地址是 IP 没填 SNI", line(`{"mode":"cert","cert":"v6.example.com"}`, `[{"server":"198.51.100.9","node_id":1}]`), want{true, ""})
	// 填了 SNI、证书盖得住 → 不带;SNI 原样
	check("填了 SNI", line(`{"mode":"cert","cert":"v6.example.com"}`, `[{"server":"198.51.100.9","sni":"v6.example.com","node_id":1}]`), want{false, "v6.example.com"})
	// 用默认(自签)证书 → 带
	check("默认自签", line(`{"mode":"cert"}`, `[{"server":"v6.example.com","node_id":1}]`), want{true, ""})
	// 指到第二台(泛域名正式证书):以前借第一台的判断会带上「允许不安全」
	check("第二台泛域名", line(`{"mode":"cert"}`, `[{"server":"hk.example.net","node_id":2}]`), want{false, ""})
	// 泛域名只管一级,盖不住的维持那台默认的判断(正式证书 → 不带,和以前单台时一样,该填 SNI 的还得填)
	check("泛域名盖不住", line(`{"mode":"cert"}`, `[{"server":"a.b.example.net","node_id":2}]`), want{false, ""})
	// 老版本副机:按它自己上报的判断
	check("老版本副机", line(`{"mode":"cert"}`, `[{"server":"x.example.org","node_id":3}]`), want{true, ""})
	// 没指服务器的地址:和以前一样跟第一台
	check("没指服务器", line(`{"mode":"cert","cert":"v6.example.com"}`, `[{"server":"v6.example.com"}]`), want{true, ""})
	// 设置里强制了的:不按证书判断
	entries[0].InsecureFixed, entries[0].Insecure = true, false
	check("强制不允许", line(`{"mode":"cert"}`, `[{"server":"v6.example.com","node_id":1}]`), want{false, ""})
	entries[0].Insecure = true
	check("强制允许", line(`{"mode":"cert","cert":"v6.example.com"}`, `[{"server":"v6.example.com","node_id":1}]`), want{true, ""})
	entries[0].InsecureFixed = false

	// 链接里:不带 allowInsecure、不带 pcs
	user := model.User{Name: "u", Credentials: []byte(`{"trojan":{"password":"p"}}`)}
	l := GenerateLinks(user, []model.Line{line(`{"mode":"cert","cert":"v6.example.com"}`, `[{"server":"v6.example.com","node_id":1}]`)}, entries)[0]
	if strings.Contains(l, "allowInsecure") || strings.Contains(l, "pcs=") {
		t.Errorf("链接: %s", l)
	}
	l = GenerateLinks(user, []model.Line{line(`{"mode":"cert"}`, `[{"server":"198.51.100.9","node_id":1}]`)}, entries)[0]
	if !strings.Contains(l, "allowInsecure=1") || strings.Contains(l, "pcs=") {
		t.Errorf("自签 + 自定义地址:带 allowInsecure、不带指纹: %s", l)
	}
}
