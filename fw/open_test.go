package fw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const oracleInput = "-P INPUT ACCEPT\n-A INPUT -m state --state RELATED,ESTABLISHED -j ACCEPT\n-A INPUT -p tcp -m state --state NEW -m tcp --dport 22 -j ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"

func TestOpenIPTables(t *testing.T) {
	// Oracle 镜像那种:v4 的 INPUT 有 REJECT,v6 干净 —— 只给 v4 插一条全放行;改之前先备份
	f := newFake("iptables", "ip6tables", "iptables-save", "netfilter-persistent")
	f.out["iptables -S INPUT"] = oracleInput
	f.out["ip6tables -S INPUT"] = "-P INPUT ACCEPT\n"
	f.out["iptables-save"] = "*filter\n:INPUT ACCEPT [0:0]\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\nCOMMIT\n"
	root := t.TempDir()
	o, err := Open(f, root, time.Unix(1790000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if o.Frontend != FrontIPTables || strings.Join(o.Actions, ",") != ActIPTables {
		t.Fatalf("动作不对: %+v", o)
	}
	if !f.called("iptables -I INPUT 1 -m comment --comment m-ui-open -j ACCEPT") || f.called("ip6tables -I INPUT") {
		t.Fatalf("应该只给 v4 插一条: %v", f.calls)
	}
	if !f.called("netfilter-persistent save") {
		t.Fatalf("有持久化工具应该顺手存一份: %v", f.calls)
	}
	b, err := os.ReadFile(filepath.Join(o.Backup, "iptables.rules"))
	if err != nil || !strings.Contains(string(b), "REJECT") || !strings.HasPrefix(filepath.Base(o.Backup), "fw-backup-") {
		t.Fatalf("改之前的规则没备份下来: %v %q", err, o.Backup)
	}

	// 开机后那条被冲掉了:补回去;还在就不动
	f2 := newFake("iptables", "ip6tables")
	f2.out["iptables -S INPUT"] = oracleInput
	EnsureOpen(f2, o)
	if !f2.called("iptables -I INPUT 1 -m comment --comment m-ui-open -j ACCEPT") {
		t.Fatalf("被冲掉的全放行没有补回: %v", f2.calls)
	}
	f3 := newFake("iptables")
	f3.out["iptables -S INPUT"] = "-P INPUT ACCEPT\n-A INPUT -m comment --comment m-ui-open -j ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"
	EnsureOpen(f3, o)
	if f3.called("iptables -I INPUT") {
		t.Fatalf("那条还在,不该再插: %v", f3.calls)
	}

	// 撤销:把那条删掉(删到报「没有了」为止)
	f4 := newFake("iptables")
	n := 0
	f4.fail = map[string]string{}
	if err := Undo(&countingFake{fake: f4, failAfter: 1, n: &n}, o); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应该删一次成功、第二次发现没有了就停: %d 次", n)
	}
}

// countingFake 让「iptables -D」前 failAfter 次成功、之后报错(模拟规则删完了)。
type countingFake struct {
	*fake
	failAfter int
	n         *int
}

func (c *countingFake) Run(stdin, name string, args ...string) (string, error) {
	if name == "iptables" && len(args) > 0 && args[0] == "-D" {
		*c.n++
		if *c.n > c.failAfter {
			return "", os.ErrNotExist
		}
	}
	return c.fake.Run(stdin, name, args...)
}

func TestOpenFrontends(t *testing.T) {
	now := time.Unix(1790000000, 0)
	// ufw
	f := newFake("ufw")
	f.out["ufw status"] = "Status: active\n"
	o, err := Open(f, "", now)
	if err != nil || strings.Join(o.Actions, ",") != ActUFW || !f.called("ufw disable") {
		t.Fatalf("ufw: %+v %v %v", o, err, f.calls)
	}
	f = newFake("ufw")
	if err := Undo(f, o); err != nil || !f.called("ufw --force enable") {
		t.Fatalf("ufw 撤销: %v %v", err, f.calls)
	}
	// firewalld
	f = newFake("firewall-cmd", "systemctl")
	f.out["firewall-cmd --state"] = "running\n"
	o, err = Open(f, "", now)
	if err != nil || strings.Join(o.Actions, ",") != ActFirewalld || !f.called("systemctl disable --now firewalld") {
		t.Fatalf("firewalld: %+v %v %v", o, err, f.calls)
	}
	f = newFake("systemctl")
	if err := Undo(f, o); err != nil || !f.called("systemctl enable --now firewalld") {
		t.Fatalf("firewalld 撤销: %v %v", err, f.calls)
	}
	// 原生 nftables:不自动改
	f = newFake("nft")
	f.out["nft list ruleset"] = "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}\n"
	if o, err := Open(f, "", now); err == nil || len(o.Actions) != 0 {
		t.Fatalf("原生 nft 不该自动改: %+v %v", o, err)
	}
	// 本来就没拦:什么都不做
	f = newFake("iptables")
	f.out["iptables -S INPUT"] = "-P INPUT ACCEPT\n"
	if o, err := Open(f, "", now); err != nil || len(o.Actions) != 0 || f.called("iptables -I") {
		t.Fatalf("没拦时不该动: %+v %v %v", o, err, f.calls)
	}
}

func calledWith(f *fake, prefix string) []string {
	var got []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			got = append(got, c)
		}
	}
	return got
}

func TestSyncAllow(t *testing.T) {
	ports := []PortProto{{PortRange{443, 443}, "tcp"}, {PortRange{20000, 20100}, "udp"}, {PortRange{443, 443}, "tcp"}, {PortRange{2053, 2053}, "tcp"}, {PortRange{1, 1}, "icmp"}}
	// ufw:一个端口一条,区间用冒号,重复的只加一次;用户自己早就放行的(2053,没带协议 = 两种都算)不加、也不记成 m-ui 的
	f := newFake("ufw")
	f.out["ufw status"] = "Status: active\n"
	f.out["ufw show added"] = "Added user rules (see 'ufw status' for running firewall):\nufw allow 22/tcp\nufw allow 2053\nufw allow from 198.51.100.7 to any port 443 proto tcp\nufw allow OpenSSH\n"
	got, err := SyncAllow(f, Detect(f), ports, Allowed{})
	if err != nil {
		t.Fatal(err)
	}
	if c := calledWith(f, "ufw allow"); strings.Join(c, "|") != "ufw allow 443/tcp comment m-ui|ufw allow 20000:20100/udp comment m-ui" {
		t.Fatalf("ufw 放行不对: %v", c)
	}
	if got.Frontend != FrontUFW || strings.Join(got.Ports, ",") != "20000-20100/udp,443/tcp" {
		t.Fatalf("记下的应该只有 m-ui 自己加的两条: %+v", got)
	}
	// 再来一轮:线路删了,只剩 443 —— 收回 m-ui 加过、现在用不着的那条;用户自己的 2053、22 一条不碰
	f = newFake("ufw")
	f.out["ufw status"] = "Status: active\n"
	f.out["ufw show added"] = "ufw allow 22/tcp\nufw allow 2053\nufw allow 443/tcp comment 'm-ui'\nufw allow 20000:20100/udp comment 'm-ui'\n"
	got, err = SyncAllow(f, Detect(f), []PortProto{{PortRange{443, 443}, "tcp"}}, got)
	if err != nil {
		t.Fatal(err)
	}
	if c := calledWith(f, "ufw delete"); strings.Join(c, "|") != "ufw delete allow 20000:20100/udp" || f.called("ufw allow") {
		t.Fatalf("应该只收回用不着的那一条: %v", f.calls)
	}
	if strings.Join(got.Ports, ",") != "443/tcp" {
		t.Fatalf("记录没跟上: %+v", got)
	}
	// 记着是 m-ui 加的、但那里已经没有了(管理员自己删了):补回去
	f = newFake("ufw")
	f.out["ufw status"] = "Status: active\n"
	got, err = SyncAllow(f, Detect(f), []PortProto{{PortRange{443, 443}, "tcp"}}, got)
	if err != nil || !f.called("ufw allow 443/tcp comment m-ui") || strings.Join(got.Ports, ",") != "443/tcp" {
		t.Fatalf("被删掉的放行没补回: %v %+v %v", err, got, f.calls)
	}
	// 加的时候出错:报出来,那一条不记(下次再试),别的照加
	f = newFake("ufw")
	f.out["ufw status"] = "Status: active\n"
	f.fail["ufw allow 443/tcp"] = "ERROR: could not find a profile"
	got, err = SyncAllow(f, Detect(f), ports, Allowed{})
	if err == nil || strings.Join(got.Ports, ",") != "20000-20100/udp,2053/tcp" {
		t.Fatalf("部分失败时应报错并只记成了的: %v %+v", err, got)
	}

	// firewalld:当下生效一遍、永久一遍;已经放行的不重复加;用不着的两处都收回;换了一种系统防火墙,旧记录作废
	f = newFake("firewall-cmd")
	f.out["firewall-cmd --state"] = "running\n"
	f.out["firewall-cmd --query-port=2053/tcp"] = "yes\n"
	f.out["firewall-cmd --query-port=8443/tcp"] = "yes\n"
	f.fail["firewall-cmd --query-port=443/tcp"] = "no"
	f.fail["firewall-cmd --query-port=20000-20100/udp"] = "no"
	got, err = SyncAllow(f, Detect(f), ports, Allowed{Frontend: FrontUFW, Ports: []string{"8443/tcp"}})
	if err != nil {
		t.Fatal(err)
	}
	if !f.called("firewall-cmd --add-port=20000-20100/udp") || !f.called("firewall-cmd --permanent --add-port=443/tcp") || f.called("firewall-cmd --add-port=2053/tcp") || f.called("firewall-cmd --remove-port") {
		t.Fatalf("firewalld 放行不对: %v", f.calls)
	}
	if got.Frontend != FrontFirewalld || strings.Join(got.Ports, ",") != "20000-20100/udp,443/tcp" {
		t.Fatalf("firewalld 的记录不对: %+v", got)
	}
	f = newFake("firewall-cmd")
	f.out["firewall-cmd --state"] = "running\n"
	f.out["firewall-cmd --query-port"] = "yes\n"
	got, err = SyncAllow(f, Detect(f), nil, got)
	if err != nil || !f.called("firewall-cmd --remove-port=443/tcp") || !f.called("firewall-cmd --permanent --remove-port=20000-20100/udp") || len(got.Ports) != 0 {
		t.Fatalf("firewalld 收回不对: %v %+v %v", err, got, f.calls)
	}

	// 原生 nftables:有要放行的就报说明,没有就没事
	f = newFake("nft")
	f.out["nft list ruleset"] = "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}\n"
	if _, err := SyncAllow(f, Detect(f), ports, Allowed{}); err == nil {
		t.Fatal("原生 nft 有端口要放行时应该报说明")
	}
	if _, err := SyncAllow(f, Detect(f), nil, Allowed{}); err != nil {
		t.Fatalf("原生 nft 没有端口要放行时不该报错: %v", err)
	}

	// iptables:自己的链整链重建,跳转补到 INPUT 第一条
	f = newFake("iptables", "iptables-restore")
	f.out["iptables -S INPUT"] = oracleInput
	f.fail["iptables -C INPUT -j M-UI-ALLOW"] = "No chain/target/match by that name"
	got, err = SyncAllow(f, Detect(f), ports, Allowed{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Ports, ",") != "443/tcp,2053/tcp,20000-20100/udp" {
		t.Fatalf("iptables 的记录应该是整条链的内容: %+v", got)
	}
	script := ""
	for i, c := range f.calls {
		if strings.HasPrefix(c, "iptables-restore --noflush") {
			script = f.stdin[i]
		}
	}
	for _, want := range []string{":M-UI-ALLOW - [0:0]\n-F M-UI-ALLOW\n", "-A M-UI-ALLOW -p tcp -m multiport --dports 443,2053 -m comment --comment m-ui-allow -j ACCEPT", "-A M-UI-ALLOW -p udp -m multiport --dports 20000:20100 -m comment --comment m-ui-allow -j ACCEPT", "COMMIT\n"} {
		if !strings.Contains(script, want) {
			t.Errorf("iptables 脚本里缺少 %q:\n%s", want, script)
		}
	}
	if !f.called("iptables -I INPUT 1 -j M-UI-ALLOW") {
		t.Fatalf("跳转没挂上: %v", f.calls)
	}
	// 一个端口都不要放行了:把自己的链撤掉
	f = newFake("iptables", "iptables-restore")
	f.out["iptables -S INPUT"] = oracleInput
	if got, err := SyncAllow(f, Detect(f), nil, got); err != nil || len(got.Ports) != 0 || !f.called("iptables -D INPUT -j M-UI-ALLOW") || !f.called("iptables -X M-UI-ALLOW") || f.called("iptables-restore") {
		t.Fatalf("iptables 撤链不对: %v %+v %v", err, got, f.calls)
	}
	// 没有防火墙在拦:不动,记录原样留着(以后系统防火墙又开回来,还认得哪些是自己加的)
	f = newFake("iptables", "ufw")
	f.out["ufw status"] = "Status: inactive\n"
	f.out["iptables -S INPUT"] = "-P INPUT ACCEPT\n"
	had := Allowed{Frontend: FrontUFW, Ports: []string{"443/tcp"}}
	if got, err := SyncAllow(f, Detect(f), ports, had); err != nil || f.called("ufw allow") || f.called("iptables-restore") || strings.Join(got.Ports, ",") != "443/tcp" {
		t.Fatalf("没拦时不该动: %v %+v %v", err, got, f.calls)
	}
}

func TestPinFirst(t *testing.T) {
	// 「放开」那条全放行要排在面板规则后面:面板规则落在 iptables 里的机器,插完全放行再把 M-UI-IN 挪回第一条
	f := newFake("iptables")
	f.out["iptables -S INPUT"] = "-P INPUT DROP\n-A INPUT -m comment --comment m-ui-open -j ACCEPT\n-A INPUT -j M-UI-IN\n"
	if err := insertOpen(f, "iptables"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, "|") != "iptables -I INPUT 1 -m comment --comment m-ui-open -j ACCEPT|iptables -S INPUT|iptables -I INPUT 1 -j M-UI-IN|iptables -D INPUT 3" {
		t.Fatalf("顺序不对: %v", f.calls)
	}
	// 面板规则不在 iptables 里(落在 nft,或者这台没有规则):不凭空加跳转
	f = newFake("iptables")
	f.out["iptables -S INPUT"] = oracleInput
	if err := insertOpen(f, "iptables"); err != nil || f.called("iptables -I INPUT 1 -j M-UI-IN") {
		t.Fatalf("没有面板规则的机器不该加跳转: %v %v", err, f.calls)
	}
	// 排在后面的全放行照样认得出「已放开」:前面只有 m-ui 自己的东西
	if opened, _ := inputBlocks("-P INPUT ACCEPT\n-A INPUT -j M-UI-IN\n-A INPUT -j M-UI-ALLOW\n-A INPUT -m comment --comment m-ui-open -j ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"); !opened {
		t.Fatal("全放行排在 m-ui 自己的链后面时也应认作已放开")
	}
}
