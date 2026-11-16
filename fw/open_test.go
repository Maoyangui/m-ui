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

func TestAllowPorts(t *testing.T) {
	ports := []PortProto{{PortRange{443, 443}, "tcp"}, {PortRange{20000, 20100}, "udp"}, {PortRange{443, 443}, "tcp"}, {PortRange{2053, 2053}, "tcp"}, {PortRange{1, 1}, "icmp"}}
	// ufw:一个端口一条,区间用冒号;重复的只加一次
	f := newFake("ufw")
	f.out["ufw status"] = "Status: active\n"
	if err := AllowPorts(f, ports); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "ufw allow") {
			got = append(got, c)
		}
	}
	if strings.Join(got, "|") != "ufw allow 443/tcp comment m-ui|ufw allow 2053/tcp comment m-ui|ufw allow 20000:20100/udp comment m-ui" {
		t.Fatalf("ufw 放行不对: %v", got)
	}
	// firewalld:当下生效一遍、永久一遍
	f = newFake("firewall-cmd")
	f.out["firewall-cmd --state"] = "running\n"
	if err := AllowPorts(f, ports); err != nil {
		t.Fatal(err)
	}
	if !f.called("firewall-cmd --add-port=20000-20100/udp") || !f.called("firewall-cmd --permanent --add-port=443/tcp") {
		t.Fatalf("firewalld 放行不对: %v", f.calls)
	}
	// iptables:自己的链整链重建,跳转补到 INPUT 第一条
	f = newFake("iptables", "iptables-restore")
	f.out["iptables -S INPUT"] = oracleInput
	f.fail["iptables -C INPUT -j M-UI-ALLOW"] = "No chain/target/match by that name"
	if err := AllowPorts(f, ports); err != nil {
		t.Fatal(err)
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
	// 没有防火墙在拦:不动
	f = newFake("iptables", "ufw")
	f.out["ufw status"] = "Status: inactive\n"
	f.out["iptables -S INPUT"] = "-P INPUT ACCEPT\n"
	if err := AllowPorts(f, ports); err != nil || f.called("ufw allow") || f.called("iptables-restore") {
		t.Fatalf("没拦时不该动: %v %v", err, f.calls)
	}
}
