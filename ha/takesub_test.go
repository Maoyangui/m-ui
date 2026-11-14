package ha

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/certutil"
)

// 接管范围:没设过的按配没配 DNS 接口算(老版本开过自动改 DNS 的照旧接管订阅),明确设了以设的为准。
func TestTakeSubDefault(t *testing.T) {
	if s := testService(t, false, map[string]string{KeyDnsToken: "tok", KeyDnsNames: "sub.example.com"}); !s.takeSub() {
		t.Fatal("老设置配了 DNS 接口:照旧接管订阅")
	}
	s := testService(t, false, nil)
	if s.takeSub() {
		t.Fatal("什么都没配:默认只接管主机的工作")
	}
	s.set(map[string]string{KeyTakeSub: "false", KeyDnsToken: "tok", KeyDnsNames: "sub.example.com"})
	if s.takeSub() {
		t.Fatal("明确关了:配了 DNS 接口也不接管订阅")
	}
	s.set(map[string]string{KeyTakeSub: "true", KeyDnsToken: ""})
	if !s.takeSub() {
		t.Fatal("明确开了")
	}
}

// 保存:选了接管订阅,订阅地址得用域名、得有 DNS 接口(格式按服务商查),订阅域名自动加进要跟着主机走的域名;不认识的服务商拒绝。
func TestSaveConfigTakeSub(t *testing.T) {
	base := ConfigInput{Config: Config{Mode: ModeOff, Standbys: 2, ReplicaMinutes: 5, ConfirmMinutes: 3, AutoMinutes: 10, RankStepMinutes: 3, Witness: true}}
	with := func(f func(*ConfigInput)) ConfigInput { in := base; f(&in); return in }
	s := testService(t, false, map[string]string{"publicIp": "203.0.113.7"})
	expect := func(in ConfigInput, want string) {
		t.Helper()
		if err := s.SaveConfig(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("应报含 %q 的错: %v", want, err)
		}
	}
	expect(with(func(c *ConfigInput) { c.TakeSub, c.DnsToken = true, "tok" }), "域名") // 订阅地址是 IP
	s.set(map[string]string{"subDomain": "sub.example.com"})
	expect(with(func(c *ConfigInput) { c.TakeSub = true }), "DNS 接口")
	expect(with(func(c *ConfigInput) { c.TakeSub, c.DnsProvider, c.DnsToken = true, "aliyun", "onlyid" }), "AccessKey")
	expect(with(func(c *ConfigInput) { c.DnsProvider = "godaddy" }), "不支持")
	if err := s.SaveConfig(with(func(c *ConfigInput) {
		c.TakeSub, c.DnsProvider, c.DnsToken, c.DnsNames = true, "aliyun", "AK,SK", "panel.example.com"
	})); err != nil {
		t.Fatal(err)
	}
	c := s.Config()
	if !c.TakeSub || c.DnsProvider != "aliyun" || !c.DnsTokenSet || !strings.Contains(c.DnsNames, "sub.example.com") || !strings.Contains(c.DnsNames, "panel.example.com") {
		t.Fatalf("存下了接管范围、服务商,订阅域名自动加进名单: %+v", c)
	}
	// 换回只接管主机的工作:凭据留着,开关关上
	if err := s.SaveConfig(with(func(c *ConfigInput) { c.DnsProvider = "aliyun" })); err != nil || s.takeSub() || !s.Config().DnsTokenSet {
		t.Fatalf("关掉接管订阅: %v %+v", err, s.Config())
	}
}

// 副本带不带订阅证书:只接管主机的工作时不带(私钥不出主机),接管订阅才带;两种都记下原主机的订阅走 https。
func TestCaptureIdentityByScope(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "sub.crt"), filepath.Join(dir, "sub.key")
	if err := certutil.GenerateSelfSigned([]string{"sub.example.com"}, crt, key, 30); err != nil {
		t.Fatal(err)
	}
	s := testService(t, false, map[string]string{"subDomain": "sub.example.com", "subCertFile": crt, "subKeyFile": key})
	capture := func(full ...bool) (Identity, []string) {
		t.Helper()
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		id, err := s.captureIdentity(zw, len(full) > 0 && full[0])
		if err != nil {
			t.Fatal(err)
		}
		zw.Close()
		zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		return id, names
	}
	id, files := capture()
	if id.SubCert != nil || len(files) != 0 || !id.SubHTTPS || id.SubHost != "sub.example.com" {
		t.Fatalf("只接管主机的工作:不带证书文件,记下走 https: %+v %v", id, files)
	}
	if id, files = capture(true); id.SubCert == nil || len(files) != 2 {
		t.Fatalf("设为主机的整库数据:订阅总是一起迁过去,证书带上: %+v %v", id, files)
	}
	s.set(map[string]string{KeyTakeSub: "true", KeyDnsToken: "tok", KeyDnsNames: "sub.example.com"})
	id, files = capture()
	if id.SubCert == nil || len(files) != 2 {
		t.Fatalf("接管订阅:带上订阅证书: %+v %v", id, files)
	}
}

// 接任:没开接管订阅,新主机的订阅设置照旧是自己的(说清订阅地址还指向原主机);开了才换上原主机的订阅域名、端口、路径。
func TestTakeoverSubscriptionScope(t *testing.T) {
	for _, take := range []bool{false, true} {
		p := newP2(t, 1, ModeConfirm)
		m, c := p.m, p.cands[0]
		m.set(map[string]string{"subDomain": "sub.example.com", "subPort": "2096", "subPath": "/s/"})
		if take {
			m.set(map[string]string{KeyTakeSub: "true", KeyDnsToken: "tok", KeyDnsNames: "sub.example.com"})
		}
		c.set(map[string]string{"subPort": "2087", "subPath": "/own/", "subDomain": ""})
		if err := m.ReplicateNow(); err != nil {
			t.Fatal(err)
		}
		if err := c.Takeover(TakeoverOpts{Force: true, By: "cli"}); err != nil {
			t.Fatal(err)
		}
		ndb := swapIn(t, c)
		get := func(k string) string {
			var v string
			ndb.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
			return v
		}
		var b Became
		if err := json.Unmarshal([]byte(get(KeyBecame)), &b); err != nil {
			t.Fatal(err)
		}
		if take {
			if get("subDomain") != "sub.example.com" || get("subPort") != "2096" || get("subPath") != "/s/" || !b.Identity.Taken || !b.Identity.SubURLKept {
				t.Fatalf("接管订阅:换上原主机的订阅设置: %s %s %s %+v", get("subDomain"), get("subPort"), get("subPath"), b.Identity)
			}
			continue
		}
		if get("subPort") != "2087" || get("subPath") != "/own/" || get("subDomain") != "" || b.Identity.Taken || b.Identity.SubURLKept {
			t.Fatalf("只接管主机的工作:订阅设置照旧: %s %s %q %+v", get("subPort"), get("subPath"), get("subDomain"), b.Identity)
		}
		if len(b.Identity.Warnings) != 1 || b.Identity.Warnings[0].Code != idwSubNotTaken || b.Identity.Warnings[0].Args["host"] != "sub.example.com" {
			t.Fatalf("要说清订阅没接管、订阅地址还指向原主机: %+v", b.Identity.Warnings)
		}
	}
}

// 接管了订阅、订阅走 https:新主机上的订阅证书不能用(快到期、不含订阅域名、没有)就经 DNS 接口重签;能用就不动;没有接口要手动处理。
func TestEnsureSubCertAfterTakeover(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, hosts []string, days int) string {
		crt, key := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
		if err := certutil.GenerateSelfSigned(hosts, crt, key, days); err != nil {
			t.Fatal(err)
		}
		return crt
	}
	good, soon, other := mk("good", []string{"sub.example.com"}, 60), mk("soon", []string{"sub.example.com"}, 5), mk("other", []string{"x.example.com"}, 60)
	s := testService(t, false, map[string]string{KeyDnsToken: "tok"})
	var issued []string
	s.d.IssueSubCert = func(d string) error { issued = append(issued, d); return nil }
	run := func(certFile string) Became {
		t.Helper()
		s.set(map[string]string{"subCertFile": certFile})
		b := Became{Identity: identityResult{Taken: true, SubHTTPS: true, SubHost: "sub.example.com"}}
		s.ensureSubCert(&b)
		return b
	}
	if b := run(good); len(issued) != 0 || len(b.Identity.Warnings) != 0 {
		t.Fatalf("证书能用:不重签 %v %+v", issued, b.Identity.Warnings)
	}
	for _, f := range []string{soon, other, ""} {
		before := len(issued)
		b := run(f)
		if len(issued) != before+1 || issued[len(issued)-1] != "sub.example.com" || b.Identity.Warnings[0].Code != idwSubCertNew {
			t.Fatalf("%q 不能用:经 DNS 接口给订阅域名重签 %v %+v", f, issued, b.Identity.Warnings)
		}
	}
	s.set(map[string]string{KeyDnsToken: ""})
	if b := run(soon); b.Identity.Warnings[0].Code != idwSubCertTodo {
		t.Fatalf("没有 DNS 接口:要手动处理 %+v", b.Identity.Warnings)
	}
	// 没接管订阅、或原主机的订阅本来是 http:不管证书
	n := len(issued)
	b := Became{Identity: identityResult{Taken: false, SubHTTPS: true, SubHost: "sub.example.com"}}
	s.set(map[string]string{KeyDnsToken: "tok"})
	s.ensureSubCert(&b)
	b2 := Became{Identity: identityResult{Taken: true, SubHTTPS: false, SubHost: "sub.example.com"}}
	s.ensureSubCert(&b2)
	if len(issued) != n {
		t.Fatal("没接管订阅、订阅本来是 http 的不签")
	}
}

// 设为主机(管理员计划内换主机)不受接管范围影响:默认只接管主机的工作时也整套迁订阅(域名、端口、路径、证书)。
func TestSetMasterMovesSubscriptionByDefault(t *testing.T) {
	cl := readyCluster(t, 1)
	m, tgt := cl.m, cl.cands[0]
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "sub.crt"), filepath.Join(dir, "sub.key")
	if err := certutil.GenerateSelfSigned([]string{"sub.example.com"}, crt, key, 60); err != nil {
		t.Fatal(err)
	}
	m.set(map[string]string{"subDomain": "sub.example.com", "subPort": "2096", "subPath": "/s/", "subCertFile": crt, "subKeyFile": key})
	tgt.set(map[string]string{"subPort": "2087", "subPath": "/own/", "subDomain": ""})
	if m.takeSub() {
		t.Fatal("前提:默认只接管主机的工作")
	}
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	if st := waitSwitchDone(t, m); st.Stage != StageDone || !m.IsNode() {
		t.Fatalf("设为主机应完成: %+v", st)
	}
	ndb := swapIn(t, tgt)
	get := func(k string) string {
		var v string
		ndb.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
		return v
	}
	if get("subDomain") != "sub.example.com" || get("subPort") != "2096" || get("subPath") != "/s/" {
		t.Fatalf("订阅设置换成原主机的: %s %s %s", get("subDomain"), get("subPort"), get("subPath"))
	}
	if !subCertUsable(get("subCertFile"), "sub.example.com", 15*24*time.Hour, time.Now()) {
		t.Fatalf("订阅证书随整库数据带过来、用于订阅: %q", get("subCertFile"))
	}
	var b Became
	if err := json.Unmarshal([]byte(get(KeyBecame)), &b); err != nil {
		t.Fatal(err)
	}
	for _, w := range b.Identity.Warnings {
		if w.Code == idwSubNotTaken {
			t.Fatalf("设为主机不该说没接管订阅: %+v", b.Identity.Warnings)
		}
	}
}
