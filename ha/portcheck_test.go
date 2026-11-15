package ha

import (
	"errors"
	"strings"
	"testing"
)

// 备用提前自检接任后要用的端口:主机在状态里带给备用,备用试着绑,被别的程序占着的随报告报上来;
// 主机就绪检查列出、Telegram 告一次(同一台同一组端口不重复);备用自己正监听的端口不算。
func TestStandbyPortSelfCheck(t *testing.T) {
	fastReplicas(t)
	cl := readyCluster(t, 1)
	m, c := cl.m, cl.cands[0]
	var sent []string
	m.d.Notify = func(_, msg string) {
		if strings.Contains(msg, "端口被别的程序占着") {
			sent = append(sent, msg)
		}
	}
	c.set(map[string]string{"resellerPort": "2054"})
	cl.h.RunOnce()
	if err := m.ReplicateNow(); err != nil { // 排出备用名单、发第一份副本
		t.Fatal(err)
	}
	cl.h.RunOnce() // 主机知道了它是备用、它的代理面板端口,推给它的状态里带上要留的端口
	st, ok := c.NodeState()
	if !ok || len(st.Ports) == 0 || st.Ports[0].Port != 2054 {
		t.Fatalf("推给备用的状态里带着它接任后要用的端口: %+v", st.Ports)
	}
	old := bindTest
	t.Cleanup(func() { bindTest = old })
	bindTest = func(port int) bool { return port != 2054 } // 2054 被别的程序占着
	c.portCheckTick()
	if b := c.busyPorts(); len(b) != 1 || b[0].Port != 2054 || b[0].What != TakeoverReseller {
		t.Fatalf("自检查出被占的端口: %+v", b)
	}
	cl.h.RunOnce()                        // 随报告报上去
	infos, _ := m.rep.compute(m.Config()) // 主机按最新的报告算备用信息(后台每 10 秒算一次,面板读的就是它)
	found := false
	for _, ch := range m.readiness(m.Config(), infos) {
		if ch.Code == "standbyPortBusy" && ch.Level == "warn" && len(ch.Names) == 1 && strings.HasSuffix(ch.Names[0], ":2054") {
			found = true
		}
	}
	if !found {
		t.Fatalf("主机就绪检查列出来: %+v", m.readiness(m.Config(), infos))
	}
	m.alertBusyPorts(infos)
	m.alertBusyPorts(infos)
	if len(sent) != 1 || !strings.Contains(sent[0], "2054") || !strings.Contains(sent[0], "ss -ltnp") {
		t.Fatalf("告一次,写清怎么查: %q", sent)
	}
	// 自己正监听的端口(面板、订阅)不算被占
	c.set(map[string]string{"webPort": "2054"})
	c.portCheckTick()
	if b := c.busyPorts(); len(b) != 0 {
		t.Fatalf("自己正监听的不算: %+v", b)
	}
}

// 刚接任 / 被设为主机时,迁来的端口和本机原来的都绑不上:临时改用空闲端口、写进告警;不是刚成为主机时不改。
func TestFreePortFallback(t *testing.T) {
	s := testService(t, false, map[string]string{"subPort": "2096"})
	s.set(map[string]string{KeyBecame: mustJSON(Became{Identity: identityResult{SubURLKept: true}})})
	if !s.JustBecame() {
		t.Fatal("刚成为主机")
	}
	if !s.FreePortFallback("sub", 2096, 2097, errors.New("address already in use")) || s.setting("subPort") != "2097" {
		t.Fatalf("改用空闲端口: %s", s.setting("subPort"))
	}
	var b Became
	s.getJSON(KeyBecame, &b)
	if b.Identity.SubURLKept || len(b.Identity.Warnings) != 1 || b.Identity.Warnings[0].Code != idwPortAuto || b.Identity.Warnings[0].Args["to"] != "2097" {
		t.Fatalf("记进告警、订阅地址不再算不变: %+v", b.Identity)
	}
	if txt := identityWarnText(b.Identity.Warnings[0]); !strings.Contains(txt, "2097") || !strings.Contains(txt, "防火墙") {
		t.Fatalf("Telegram 文案写清新端口、提醒放行防火墙: %s", txt)
	}
	b.Notified = true
	s.set(map[string]string{KeyBecame: mustJSON(b)})
	if s.JustBecame() || s.FreePortFallback("panel", 8443, 8444, errors.New("x")) {
		t.Fatal("不是刚成为主机:端口绑不上照常报错,不偷偷改")
	}
}
