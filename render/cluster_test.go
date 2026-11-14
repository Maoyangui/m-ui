package render

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"gorm.io/gorm"
)

// clusterRules 取出配置里「只拦 TCP、带端口」的 reject 规则(集群屏蔽的形状),按地址 → 端口集合返回。
func clusterRules(t *testing.T, raw []byte) map[string][]int {
	t.Helper()
	out := map[string][]int{}
	for _, r := range rulesOf(t, raw) {
		if r["action"] != "reject" || r["port"] == nil {
			continue
		}
		nw, _ := r["network"].([]interface{})
		if len(nw) != 1 || nw[0] != "tcp" {
			t.Fatalf("集群屏蔽只该拦 TCP: %v", r)
		}
		var ports []int
		for _, p := range r["port"].([]interface{}) {
			ports = append(ports, int(p.(float64)))
		}
		for _, c := range r["ip_cidr"].([]interface{}) {
			out[c.(string)] = append(out[c.(string)], ports...)
		}
	}
	for k := range out {
		sort.Ints(out[k])
	}
	return out
}

func clusterDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	db.Create(&model.Line{Name: "ss", Protocol: "shadowsocks", Port: 30011, Enabled: true, Options: []byte(`{"method":"aes-256-gcm","password":"x"}`)})
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true, PublicIP: "203.0.113.1"})
	return db
}

func stubLookup(t *testing.T, m map[string][]string) {
	t.Helper()
	old := lookupIP
	lookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		var ips []net.IP
		for _, s := range m[host] {
			ips = append(ips, net.ParseIP(s))
		}
		return ips, nil
	}
	clusterDNS.Lock()
	clusterDNS.m = map[string]clusterDNSEntry{}
	clusterDNS.Unlock()
	t.Cleanup(func() {
		lookupIP = old
		clusterDNS.Lock()
		clusterDNS.m = map[string]clusterDNSEntry{}
		clusterDNS.Unlock()
	})
}

// 「允许访问内网」关着时,用户经这台服务器的代理去连集群里别的服务器的管理端口(面板、副机接口、订阅、代理面板)
// 要被拦:来源正是这台服务器的 IP,副机「只放行主机」、云防火墙「只放行集群几台」都会被绕过。
// 只拦管理端口、只拦 TCP;域名解析出来的(可能是 CDN)不拦 80 / 443;内网地址交给 ip_is_private。
func TestClusterManagementPortsBlocked(t *testing.T) {
	db := clusterDB(t)
	stubLookup(t, map[string][]string{"n2.example.test": {"192.0.2.50", "198.51.100.8"}})
	ports, _ := json.Marshal([]model.ServicePort{{Label: "面板", Port: 2053}, {Label: "订阅", Port: 12056}})
	db.Create(&model.Node{Name: "副机1", ApiUrl: "https://198.51.100.7:8443", PublicIP: "198.51.100.7", PublicIP6: "2001:db8::7", ServicePorts: ports, Enabled: true})
	// 域名 API 地址,没上报服务端口(老副机):按安装默认 2053 / 2056;自己报的地址连 443 一起拦,解析出来的别家地址不拦 443
	db.Create(&model.Node{Name: "副机2", ApiUrl: "https://n2.example.test", PublicIP: "198.51.100.8", Enabled: true})
	// 内网里的副机:ip_is_private 已经整段拦了,不再单列
	db.Create(&model.Node{Name: "副机3", ApiUrl: "http://10.0.0.5:8080", PublicIP: "10.0.0.5", Enabled: true})
	// 停用的副机照样在跑面板和副机接口,也要拦
	db.Create(&model.Node{Name: "副机4", ApiUrl: "http://198.51.100.9:7000", Enabled: false})
	db.Model(&model.Node{}).Where("name = ?", "副机4").Update("enabled", false)

	raw, err := BuildConfig(db, NodeCert{})
	if err != nil {
		t.Fatal(err)
	}
	got := clusterRules(t, raw)
	want := map[string][]int{
		"198.51.100.7/32": {2053, 8443, 12056},
		"2001:db8::7/128": {2053, 8443, 12056},
		"198.51.100.8/32": {443, 2053, 2056},
		"192.0.2.50/32":   {2053, 2056},
		"198.51.100.9/32": {2053, 2056, 7000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("集群屏蔽规则不对:\n得到 %v\n应为 %v\n%s", got, want, raw)
	}
	for c := range got {
		if c == "203.0.113.1/32" {
			t.Fatal("本机地址由 ownPublicCIDRs 全端口拦,不该进集群规则")
		}
	}
	if err := core.ValidateConfig(raw); err != nil {
		t.Fatalf("带集群屏蔽的配置应通过 sing-box 干跑: %v", err)
	}

	// 线路端口不会被拦:规则只列管理端口,线路端口 30011 不在其中
	for _, ps := range got {
		for _, p := range ps {
			if p == 30011 {
				t.Fatal("线路端口不该进集群屏蔽")
			}
		}
	}

	// 打开「允许访问内网」:和本机地址一样不加
	db.Create(&model.Setting{Key: "allowPrivate", Value: "true"})
	raw, _ = BuildConfig(db, NodeCert{})
	if got := clusterRules(t, raw); len(got) != 0 {
		t.Fatalf("allowPrivate=true 时不该有集群屏蔽: %v", got)
	}
}

// 解析失败时沿用上一次的结果,一次 DNS 抖动不能把规则撤掉;同一个域名在缓存期内不重复查。
func TestClusterDNSKeepsLastOnFailure(t *testing.T) {
	stubLookup(t, nil)
	calls := 0
	ok := true
	lookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		calls++
		if !ok {
			return nil, &net.DNSError{Err: "timeout", Name: host, IsTimeout: true}
		}
		return []net.IP{net.ParseIP("192.0.2.60")}, nil
	}
	if ips := resolveClusterHost("n.example.test"); len(ips) != 1 {
		t.Fatalf("第一次应解析到 1 个地址: %v", ips)
	}
	resolveClusterHost("n.example.test")
	if calls != 1 {
		t.Fatalf("缓存期内不该重复查: %d 次", calls)
	}
	// 过期后再查失败:沿用旧结果
	clusterDNS.Lock()
	e := clusterDNS.m["n.example.test"]
	e.at = e.at.Add(-2 * clusterDNSTTL)
	clusterDNS.m["n.example.test"] = e
	clusterDNS.Unlock()
	ok = false
	if ips := resolveClusterHost("n.example.test"); len(ips) != 1 || !ips[0].Equal(net.ParseIP("192.0.2.60")) {
		t.Fatalf("解析失败时应沿用上次结果: %v", ips)
	}
}

// ClusterSig:加副机、副机换 IP、改端口、开关屏蔽都要变(runner 据此重新渲染);内容不变时不变。
func TestClusterSigFollowsInputs(t *testing.T) {
	db := clusterDB(t)
	s0 := ClusterSig(db)
	if ClusterSig(db) != s0 {
		t.Fatal("输入没变,摘要不该变")
	}
	db.Create(&model.Node{Name: "副机1", ApiUrl: "https://198.51.100.7:8443", Enabled: true})
	s1 := ClusterSig(db)
	if s1 == s0 {
		t.Fatal("加副机后摘要应变")
	}
	db.Model(&model.Node{}).Where("name = ?", "副机1").Update("public_ip", "198.51.100.70")
	s2 := ClusterSig(db)
	if s2 == s1 {
		t.Fatal("副机换公网 IP 后摘要应变")
	}
	ports, _ := json.Marshal([]model.ServicePort{{Label: "面板", Port: 2999}})
	db.Model(&model.Node{}).Where("name = ?", "副机1").Update("service_ports", ports)
	s3 := ClusterSig(db)
	if s3 == s2 {
		t.Fatal("副机改端口后摘要应变")
	}
	db.Create(&model.Setting{Key: "allowPrivate", Value: "true"})
	if ClusterSig(db) == s3 {
		t.Fatal("打开「允许访问内网」后摘要应变")
	}
	// 本机那一行变了不算(本机地址由 ownPublicCIDRs 管,publicIPLoop 自己会重新渲染)
	s4 := ClusterSig(db)
	db.Model(&model.Node{}).Where("is_local = ?", true).Update("public_ip", "203.0.113.99")
	if ClusterSig(db) != s4 {
		t.Fatal("本机那一行不该进集群摘要")
	}
}
