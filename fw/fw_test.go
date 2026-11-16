package fw

import (
	"errors"
	"strings"
	"testing"
)

// fake 假的命令执行器:记下每次调用;按「命令 参数」的前缀配输出 / 错误。
type fake struct {
	bins  map[string]bool
	out   map[string]string
	fail  map[string]string
	calls []string
	stdin []string
}

func newFake(bins ...string) *fake {
	f := &fake{bins: map[string]bool{}, out: map[string]string{}, fail: map[string]string{}}
	for _, b := range bins {
		f.bins[b] = true
	}
	return f
}

func (f *fake) Has(name string) bool { return f.bins[name] }

func (f *fake) Run(stdin, name string, args ...string) (string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, line)
	f.stdin = append(f.stdin, stdin)
	for k, msg := range f.fail {
		if strings.HasPrefix(line, k) {
			return "", errors.New(msg)
		}
	}
	best, val := "", ""
	for k, v := range f.out {
		if strings.HasPrefix(line, k) && len(k) > len(best) {
			best, val = k, v
		}
	}
	return val, nil
}

func (f *fake) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestParsePorts(t *testing.T) {
	got, err := ParsePorts(" 443, 8000-9000 22;10000:10010,53 ")
	if err != nil {
		t.Fatal(err)
	}
	want := "22 53 443 8000-9000 10000-10010"
	var parts []string
	for _, p := range got {
		parts = append(parts, p.String())
	}
	if strings.Join(parts, " ") != want {
		t.Fatalf("端口解析 / 排序不对: %v", parts)
	}
	if p, err := ParsePorts(""); err != nil || p != nil {
		t.Fatalf("空串应是全部端口(nil): %v %v", p, err)
	}
	for _, bad := range []string{"0", "65536", "90-80", "abc", "80-", "-80"} {
		if _, err := ParsePorts(bad); err == nil {
			t.Errorf("%q 应该报错", bad)
		}
	}
}

func TestParseSources(t *testing.T) {
	got, err := ParseSources("203.0.113.9,203.0.113.77/24 2001:db8::1\n::ffff:198.51.100.7、203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, p := range got {
		parts = append(parts, p.String())
	}
	// 单个 IP 成 /32、/128;网段按掩码取整;v4 映射地址还原成 v4;重复的只留一个
	want := "198.51.100.7/32 2001:db8::1/128 203.0.113.0/24 203.0.113.9/32"
	if strings.Join(parts, " ") != want {
		t.Fatalf("来源解析不对: %v", parts)
	}
	for _, bad := range []string{"0.0.0.0/0", "::/0", "1.2.3", "example.com", "1.2.3.4/33", "fe80::1%eth0"} {
		if _, err := ParseSources(bad); err == nil {
			t.Errorf("%q 应该报错", bad)
		}
	}
}

func rule(id uint, kind, proto, ports, sources string) Rule {
	p, err := ParsePorts(ports)
	if err != nil {
		panic(err)
	}
	s, err := ParseSources(sources)
	if err != nil {
		panic(err)
	}
	return Rule{ID: id, Kind: kind, Proto: proto, Ports: p, Sources: s}
}

func TestRuleCheck(t *testing.T) {
	ok := []Rule{rule(1, KindDeny, "", "", "203.0.113.9"), rule(2, KindDeny, "tcp", "22", "203.0.113.0/24"), rule(3, KindOnly, "", "2053", "198.51.100.7"),
		rule(9, KindAllow, "tcp", "80,443", ""), rule(0, KindTrust, "udp", "443", "198.51.100.1")}
	for _, r := range ok {
		if err := r.Check(); err != nil {
			t.Errorf("规则 %d 不该报错: %v", r.ID, err)
		}
	}
	bad := []Rule{rule(4, KindDeny, "", "22", ""), rule(5, KindOnly, "", "", "198.51.100.7"), rule(6, KindOnly, "", "22", ""), rule(7, KindAllow, "", "22", "198.51.100.7"), rule(8, KindDeny, "icmp", "", "203.0.113.9"),
		rule(10, KindAllow, "", "", ""), rule(11, KindTrust, "", "443", "198.51.100.1"), rule(12, "nope", "", "22", "198.51.100.7")}
	for _, r := range bad {
		if err := r.Check(); err == nil {
			t.Errorf("规则 %d 应该报错", r.ID)
		}
	}
}

func TestNFTScript(t *testing.T) {
	// 一条规则都没有:只声明再删表,不留空壳
	if s := NFTScript(nil); s != "table inet m_ui\ndelete table inet m_ui\n" {
		t.Fatalf("没有规则时的脚本不对:\n%s", s)
	}
	s := NFTScript([]Rule{
		rule(3, KindOnly, "", "2053", "198.51.100.7,2001:db8::/32"),
		rule(1, KindDeny, "", "", "203.0.113.9,2001:db8:bad::1"),
		rule(2, KindDeny, "tcp", "22,8000-9000", "203.0.113.0/24"),
		rule(4, KindDeny, "udp", "", "192.0.2.1"),
		rule(5, KindAllow, "tcp", "8080", ""),
		rule(0, KindTrust, "tcp", "443,2053", "198.51.100.1,2001:db8:c::1"),
		rule(0, KindTrust, "udp", "443", "198.51.100.1,2001:db8:c::1"),
	})
	if strings.Contains(s, "8080") {
		t.Errorf("放行端口的规则不该进 m-ui 自己这张表:\n%s", s)
	}
	for _, want := range []string{
		`ip saddr { 198.51.100.1 } tcp dport { 443, 2053 } accept comment "cluster"`,
		`ip6 saddr { 2001:db8:c::1 } udp dport { 443 } accept comment "cluster"`,
		"table inet m_ui\ndelete table inet m_ui\ntable inet m_ui {",
		"elements = { 203.0.113.9 }", "elements = { 2001:db8:bad::1 }",
		"type filter hook input priority -10; policy accept;",
		`iifname "lo" accept`,
		"ip saddr @ban4 drop", "ip6 saddr @ban6 drop",
		`ip saddr { 203.0.113.0/24 } tcp dport { 22, 8000-9000 } drop comment "rule 2"`,
		`ip saddr { 192.0.2.1 } meta l4proto udp drop comment "rule 4"`,
		`meta l4proto { tcp, udp } th dport { 2053 } ip saddr { 198.51.100.7 } accept comment "rule 3"`,
		`meta l4proto { tcp, udp } th dport { 2053 } ip6 saddr { 2001:db8::/32 } accept comment "rule 3"`,
		`meta l4proto { tcp, udp } th dport { 2053 } drop comment "rule 3"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("脚本里缺少 %q:\n%s", want, s)
		}
	}
	// 顺序:回环 → 集群里的服务器 → 整台拉黑 → 带端口的拉黑 → 只允许(先放行名单,再拒绝其余)
	order := []string{`iifname "lo" accept`, `accept comment "cluster"`, "ip saddr @ban4 drop", `comment "rule 2"`, `comment "rule 4"`, "ip saddr { 198.51.100.7 } accept", `th dport { 2053 } drop`}
	last := -1
	for _, o := range order {
		i := strings.Index(s, o)
		if i < last {
			t.Fatalf("规则顺序不对,%q 排到了前面:\n%s", o, s)
		}
		last = i
	}
	// 没有要拦的(只有放行端口、只有集群放行):表也不建
	if s := NFTScript([]Rule{rule(5, KindAllow, "tcp", "8080", ""), rule(0, KindTrust, "tcp", "443", "198.51.100.1")}); s != "table inet m_ui\ndelete table inet m_ui\n" {
		t.Fatalf("没有要拦的规则时不该建表:\n%s", s)
	}
}

func TestIPTablesScript(t *testing.T) {
	rules := []Rule{
		rule(1, KindDeny, "", "", "203.0.113.9,2001:db8:bad::1"),
		rule(2, KindDeny, "tcp", "22,8000-9000", "203.0.113.0/24"),
		rule(3, KindOnly, "", "2053", "198.51.100.7,2001:db8::/32"),
	}
	v4 := IPTablesScript(rules, false)
	for _, want := range []string{
		"*filter\n:M-UI-IN - [0:0]\n-F M-UI-IN\n-A M-UI-IN -i lo -j RETURN\n",
		"-A M-UI-IN -s 203.0.113.9 -j DROP",
		"-A M-UI-IN -s 203.0.113.0/24 -p tcp -m multiport --dports 22,8000:9000 -j DROP",
		"-A M-UI-IN -s 198.51.100.7 -p tcp -m multiport --dports 2053 -j RETURN\n-A M-UI-IN -p tcp -m multiport --dports 2053 -j DROP",
		"-A M-UI-IN -s 198.51.100.7 -p udp -m multiport --dports 2053 -j RETURN\n-A M-UI-IN -p udp -m multiport --dports 2053 -j DROP",
		"\nCOMMIT\n",
	} {
		if !strings.Contains(v4, want) {
			t.Errorf("v4 脚本里缺少 %q:\n%s", want, v4)
		}
	}
	if strings.Contains(v4, "2001:db8") {
		t.Errorf("v4 脚本里不该有 v6 地址:\n%s", v4)
	}
	v6 := IPTablesScript(rules, true)
	if !strings.Contains(v6, "-A M-UI-IN -s 2001:db8:bad::1 -j DROP") || !strings.Contains(v6, "-A M-UI-IN -s 2001:db8::/32 -p tcp -m multiport --dports 2053 -j RETURN") || strings.Contains(v6, "203.0.113") {
		t.Errorf("v6 脚本不对:\n%s", v6)
	}
	// 集群里的服务器:排在回环后面、所有拉黑前面;放行端口的规则不进这条链
	tr := IPTablesScript(append([]Rule{rule(0, KindTrust, "tcp", "443,2053", "198.51.100.1,2001:db8:c::1"), rule(5, KindAllow, "tcp", "8080", "")}, rules...), false)
	if !strings.Contains(tr, "-A M-UI-IN -i lo -j RETURN\n-A M-UI-IN -s 198.51.100.1 -p tcp -m multiport --dports 443,2053 -j RETURN\n-A M-UI-IN -s 203.0.113.9 -j DROP") || strings.Contains(tr, "8080") {
		t.Errorf("集群放行的位置不对,或放行端口进了链:\n%s", tr)
	}
	// 只允许的端口,这一族没有允许的来源:照样全拒(v6 的允许名单为空 = v6 谁也进不来)
	only4 := IPTablesScript([]Rule{rule(9, KindOnly, "tcp", "22", "198.51.100.7")}, true)
	if !strings.Contains(only4, "-A M-UI-IN -p tcp -m multiport --dports 22 -j DROP") || strings.Contains(only4, " -s ") {
		t.Errorf("v6 应该只有拒绝那一条:\n%s", only4)
	}
}

func TestMultiportSplit(t *testing.T) {
	// multiport 一条最多 15 个端口,一段算两个:20 个单端口拆成 15 + 5
	var ports []PortRange
	for i := 1; i <= 20; i++ {
		ports = append(ports, PortRange{i * 10, i * 10})
	}
	got := multiport(ports)
	if len(got) != 2 || len(got[0]) != 15 || len(got[1]) != 5 {
		t.Fatalf("拆分不对: %v", got)
	}
	// 8 段 = 16 个名额:拆成 7 段 + 1 段
	ports = nil
	for i := 1; i <= 8; i++ {
		ports = append(ports, PortRange{i * 100, i*100 + 9})
	}
	got = multiport(ports)
	if len(got) != 2 || len(got[0]) != 7 || len(got[1]) != 1 {
		t.Fatalf("端口段的拆分不对: %v", got)
	}
}

func TestDetect(t *testing.T) {
	// ufw 启用中
	f := newFake("ufw", "iptables", "nft")
	f.out["ufw status"] = "Status: active\n\nTo Action From\n"
	if st := Detect(f); st.Frontend != FrontUFW || !st.Blocking || st.Engine != EngineNFT {
		t.Fatalf("ufw: %+v", st)
	}
	// ufw 装了但没启用,firewalld 在跑
	f = newFake("ufw", "firewall-cmd", "iptables")
	f.out["ufw status"] = "Status: inactive\n"
	f.out["firewall-cmd --state"] = "running\n"
	if st := Detect(f); st.Frontend != FrontFirewalld || !st.Blocking || st.Engine != EngineIPTables {
		t.Fatalf("firewalld: %+v", st)
	}
	// Oracle 镜像那种:没有前端,INPUT 里有 REJECT
	f = newFake("iptables", "ip6tables", "nft")
	f.out["iptables -S INPUT"] = "-P INPUT ACCEPT\n-A INPUT -m state --state RELATED,ESTABLISHED -j ACCEPT\n-A INPUT -p tcp -m state --state NEW -m tcp --dport 22 -j ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"
	f.out["ip6tables -S INPUT"] = "-P INPUT ACCEPT\n"
	if st := Detect(f); st.Frontend != FrontIPTables || !st.Blocking || st.Opened || !strings.Contains(st.Detail, "REJECT") {
		t.Fatalf("iptables 有 REJECT: %+v", st)
	}
	// 放开之后:最前面是 m-ui-open 那条,不再算会拦
	f.out["iptables -S INPUT"] = "-P INPUT ACCEPT\n-A INPUT -j M-UI-IN\n-A INPUT -m comment --comment m-ui-open -j ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"
	if st := Detect(f); st.Frontend != FrontIPTables || st.Blocking || !st.Opened {
		t.Fatalf("放开之后: %+v", st)
	}
	// 策略 DROP
	f.out["iptables -S INPUT"] = "-P INPUT DROP\n-A INPUT -i lo -j ACCEPT\n"
	if st := Detect(f); st.Frontend != FrontIPTables || !st.Blocking || !strings.Contains(st.Detail, "policy DROP") {
		t.Fatalf("策略 DROP: %+v", st)
	}
	// 只有 m-ui 自己的链和规则:不算系统防火墙
	f.out["iptables -S INPUT"] = "-P INPUT ACCEPT\n-A INPUT -j M-UI-IN\n"
	if st := Detect(f); st.Frontend != FrontNone || st.Blocking {
		t.Fatalf("只有 m-ui 的链: %+v", st)
	}
	// 原生 nftables:别的表的 input 基链策略 drop;m-ui 自己那张不算
	f = newFake("nft")
	f.out["nft list ruleset"] = "table inet m_ui {\n\tchain input {\n\t\ttype filter hook input priority -10; policy accept;\n\t}\n}\ntable inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t\ttcp dport 22 accept\n\t}\n}\n"
	if st := Detect(f); st.Frontend != FrontNFTables || !st.Blocking || !strings.Contains(st.Detail, "inet filter") {
		t.Fatalf("原生 nft: %+v", st)
	}
	f.out["nft list ruleset"] = "table inet m_ui {\n\tchain input {\n\t\ttype filter hook input priority -10; policy drop;\n\t}\n}\n"
	if st := Detect(f); st.Frontend != FrontNone {
		t.Fatalf("只有 m-ui 的表: %+v", st)
	}
	// 什么都没有
	if st := Detect(newFake()); st.Frontend != FrontNone || st.Blocking || st.Engine != EngineNone {
		t.Fatalf("什么都没有: %+v", st)
	}
}

func TestApply(t *testing.T) {
	rules := []Rule{rule(1, KindDeny, "", "", "203.0.113.9")}
	// 有 nft:脚本从标准输入喂给 nft -f -
	f := newFake("nft", "iptables")
	if err := Apply(f, rules); err != nil {
		t.Fatal(err)
	}
	if !f.called("nft -f -") || !strings.Contains(f.stdin[len(f.stdin)-1], "elements = { 203.0.113.9 }") {
		t.Fatalf("nft 没按脚本应用: %v", f.calls)
	}
	// nft 用不了(内核没有 nf_tables):退到 iptables 自建链,并把跳转补到 INPUT 第一条
	f = newFake("nft", "iptables", "iptables-restore", "ip6tables", "ip6tables-restore")
	f.fail["nft list tables"] = "Operation not supported"
	if err := Apply(f, rules); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"iptables-restore --noflush", "iptables -I INPUT 1 -j M-UI-IN", "ip6tables-restore --noflush", "ip6tables -I INPUT 1 -j M-UI-IN"} {
		if !f.called(want) {
			t.Errorf("没有执行 %q: %v", want, f.calls)
		}
	}
	// 已经挂着跳转、就在第一条:不重复加
	f = newFake("iptables", "iptables-restore")
	f.out["iptables -S INPUT"] = "-P INPUT ACCEPT\n-A INPUT -j M-UI-IN\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"
	if err := Apply(f, rules); err != nil {
		t.Fatal(err)
	}
	if f.called("iptables -I INPUT") || f.called("iptables -D INPUT") {
		t.Fatalf("跳转已在第一条,不该再动: %v", f.calls)
	}
	// 挂着,但前面被插了放行(「放开」的那条、放行端口的链):挪回第一条 —— 先插新的,再按行号删旧的
	f = newFake("iptables", "iptables-restore")
	f.out["iptables -S INPUT"] = "-P INPUT ACCEPT\n-A INPUT -m comment --comment m-ui-open -j ACCEPT\n-A INPUT -j M-UI-ALLOW\n-A INPUT -j M-UI-IN\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"
	if err := Apply(f, rules); err != nil {
		t.Fatal(err)
	}
	ins, del := -1, -1
	for i, c := range f.calls {
		if c == "iptables -I INPUT 1 -j M-UI-IN" {
			ins = i
		}
		if c == "iptables -D INPUT 4" {
			del = i
		}
	}
	if ins < 0 || del < ins {
		t.Fatalf("应该先在第一条插入、再删掉原来在第 3 条(插入后是第 4 条)的那条: %v", f.calls)
	}
	// 一条规则都没有 / 只剩放行端口的:把自己的链撤掉
	for _, rs := range [][]Rule{nil, {rule(5, KindAllow, "tcp", "8080", "")}} {
		f = newFake("iptables", "iptables-restore")
		if err := Apply(f, rs); err != nil {
			t.Fatal(err)
		}
		if !f.called("iptables -D INPUT -j M-UI-IN") || !f.called("iptables -X M-UI-IN") || f.called("iptables-restore") {
			t.Fatalf("撤链不对: %v", f.calls)
		}
	}
	// 两样都没有
	if err := Apply(newFake(), rules); err == nil {
		t.Fatal("没有 nft 也没有 iptables 应该报错")
	}
	// 规则本身不对:不碰内核
	f = newFake("nft")
	if err := Apply(f, []Rule{{ID: 7, Kind: KindOnly}}); err == nil || f.called("nft -f") {
		t.Fatalf("坏规则应在应用前拦下: %v %v", err, f.calls)
	}
}
