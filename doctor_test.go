package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/fw"
)

func texts(fs []finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString([]string{"OK ", "·  ", "!  ", "X  "}[f.Lvl])
		b.WriteString(f.Text)
		if f.Fix != "" {
			b.WriteString(" => " + f.Fix)
		}
		for _, m := range f.More {
			b.WriteString(" ++ " + m)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func mustHave(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Fatalf("应当有 %q,实际:\n%s", w, got)
		}
	}
}

func mustNot(t *testing.T, got string, bans ...string) {
	t.Helper()
	for _, w := range bans {
		if strings.Contains(got, w) {
			t.Fatalf("不该有 %q,实际:\n%s", w, got)
		}
	}
}

func TestDoctorService(t *testing.T) {
	show := "LoadState=loaded\nActiveState=active\nUnitFileState=enabled\nNRestarts=0\nResult=success\nMainPID=812\n"
	v := parseSystemctlShow(show)
	if !v.Known || !v.Loaded || v.Active != "active" || v.Enabled != "enabled" || v.MainPID != 812 {
		t.Fatalf("解析不对: %+v", v)
	}
	if got := texts(checkService(v, nil)); got != "OK 服务在运行\n" {
		t.Fatalf("正常的服务只该有一行:\n%s", got)
	}
	// 反复重启:带上最近的报错
	v.Restarts = 5
	mustHave(t, texts(checkService(v, []string{"ERROR - 端口被占"})), "!  服务自动重启过 5 次", "m-ui logs -e", "++ ERROR - 端口被占")
	// 没设开机自启
	v.Restarts, v.Enabled = 0, "disabled"
	mustHave(t, texts(checkService(v, nil)), "OK 服务在运行", "!  没有设成开机自启(disabled)", "systemctl enable m-ui")
	// 各种没在跑
	for result, want := range map[string]string{
		"exit-code":       "上次是出错退出的",
		"oom-kill":        "内存不够被系统杀掉",
		"start-limit-hit": "systemctl reset-failed m-ui",
		"success":         "X  服务没在运行(状态 inactive)",
	} {
		got := texts(checkService(svcInfo{Known: true, Loaded: true, Active: "inactive", Enabled: "enabled", Result: result}, []string{"ERROR - x"}))
		mustHave(t, got, "X  服务没在运行", want, "++ ERROR - x")
	}
	mustHave(t, texts(checkService(svcInfo{Known: true, Loaded: true, Active: "activating", Enabled: "enabled"}, nil)), "!  服务正在启动")
	// 一启动就退出、systemd 在反复重试:不是「正在启动」,是起不来,报错要带上
	loop := parseSystemctlShow("LoadState=loaded\nActiveState=activating\nSubState=auto-restart\nUnitFileState=enabled\nNRestarts=7\nResult=exit-code\nMainPID=0\n")
	if loop.Sub != "auto-restart" || loop.Restarts != 7 {
		t.Fatalf("解析不对: %+v", loop)
	}
	got := texts(checkService(loop, []string{"启动失败: 启动订阅服务: listen tcp 0.0.0.0:2056: bind: address already in use"}))
	mustHave(t, got, "X  服务起不来:一启动就退出,系统在反复重试(已经重试 7 次)", "++ 启动失败: 启动订阅服务")
	mustNot(t, got, "正在启动")
	mustHave(t, texts(checkService(svcInfo{Known: true}, nil)), "X  没有找到 m-ui 的服务", "安装脚本")
	mustHave(t, texts(checkService(svcInfo{}, nil)), "·  这台问不到 systemd")
}

func TestDoctorParseSS(t *testing.T) {
	out := `State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process
LISTEN 0      4096         0.0.0.0:22        0.0.0.0:*     users:(("sshd",pid=700,fd=3))
LISTEN 0      511          0.0.0.0:2053      0.0.0.0:*     users:(("nginx",pid=901,fd=6),("nginx",pid=900,fd=6))
LISTEN 0      4096               *:2056            *:*     users:(("m-ui",pid=812,fd=14))
LISTEN 0      4096            [::]:12053        [::]:*     users:(("other",pid=5,fd=1))
LISTEN 0      4096       127.0.0.1:2054      0.0.0.0:*
LISTEN 0      4096         0.0.0.0:8443      0.0.0.0:*     users:(("caddy",pid=77,fd=9))
LISTEN 0      4096            [::]:8443         [::]:*     users:(("m-ui",pid=812,fd=15))
`
	for port, want := range map[int]listenerInfo{
		22:   {true, "sshd", 700},
		2053: {true, "nginx", 901}, // 不能被 12053 那一行带偏
		2056: {true, "m-ui", 812},
		2054: {true, "", 0},       // 看不到进程名(不是 root)
		8443: {true, "m-ui", 812}, // 同一个端口有几条:优先报 m-ui 自己
		9999: {},
	} {
		if got := parseSS(out, port); got != want {
			t.Errorf("端口 %d: 要 %+v,得 %+v", port, want, got)
		}
	}
}

func TestDoctorPorts(t *testing.T) {
	master := map[string]string{}
	if w := wantedPorts(master); len(w) != 3 || w[0].Port != 2053 || w[1].Port != 2056 || w[2].Port != 2054 || w[2].Name != "代理面板" {
		t.Fatalf("主机默认三个端口: %+v", w)
	}
	if w := wantedPorts(map[string]string{"nodeMode": "true"}); len(w) != 2 {
		t.Fatalf("副机不起代理面板: %+v", w)
	}
	if w := wantedPorts(map[string]string{"resellerEnabled": "false", "webPort": "3000", "webListen": " 10.0.0.5 "}); len(w) != 2 || w[0].Port != 3000 || w[0].Listen != "10.0.0.5" {
		t.Fatalf("关了代理面板 / 自定义端口: %+v", w)
	}
	look := func(m map[int]listenerInfo) func(portWant) listenerInfo {
		return func(w portWant) listenerInfo { return m[w.Port] }
	}
	local := []string{"127.0.0.1", "10.0.0.5", "203.0.113.7"}
	// 都好
	got := texts(checkPorts(wantedPorts(master), look(map[int]listenerInfo{2053: {true, "m-ui", 812}, 2056: {true, "m-ui", 812}, 2054: {true, "", 0}}), true, 812, local))
	if got != "OK 面板 2053/tcp 在监听\nOK 订阅 2056/tcp 在监听\nOK 代理面板 2054/tcp 在监听\n" {
		t.Fatalf("都正常时:\n%s", got)
	}
	// 被别的程序占着;服务在跑却没监听
	got = texts(checkPorts(wantedPorts(master), look(map[int]listenerInfo{2053: {true, "nginx", 901}}), true, 812, local))
	mustHave(t, got, "X  面板端口 2053 被别的程序占着(nginx,进程号 901)", "X  订阅端口 2056 没在监听(服务在运行)", "菜单「修改端口 / 路径 / 监听地址」")
	// 进程名不是 m-ui 但进程号对得上(改过名的二进制):算自己的
	mustHave(t, texts(checkPorts(wantedPorts(master)[:1], look(map[int]listenerInfo{2053: {true, "m-ui-new", 812}}), true, 812, local)), "OK 面板 2053/tcp 在监听")
	// 服务没在跑:不重复报「没在监听」,但占用者照样点名(多半就是起不来的原因)
	got = texts(checkPorts(wantedPorts(master), look(map[int]listenerInfo{2056: {true, "caddy", 77}}), false, 0, local))
	mustHave(t, got, "X  订阅端口 2056 被别的程序占着(caddy")
	mustNot(t, got, "没在监听")
	// 设置里撞了
	mustHave(t, texts(checkPorts(wantedPorts(map[string]string{"subPort": "2053"}), look(nil), false, 0, local)), "X  面板和订阅的端口都是 2053")
	// 监听地址不在这台上;只监听本机
	got = texts(checkPorts(wantedPorts(map[string]string{"webListen": "198.51.100.9", "resellerEnabled": "false"}), look(map[int]listenerInfo{2056: {true, "m-ui", 1}}), true, 1, local))
	mustHave(t, got, "X  面板的监听地址 198.51.100.9 不在这台机器的网卡上", "把监听地址清空")
	got = texts(checkPorts(wantedPorts(map[string]string{"webListen": "127.0.0.1", "resellerEnabled": "false"}), look(map[int]listenerInfo{2053: {true, "m-ui", 1}, 2056: {true, "m-ui", 1}}), true, 1, local))
	mustHave(t, got, "OK 面板 2053/tcp 在监听(只在 127.0.0.1 上)", "·  面板只监听本机(127.0.0.1)")
	// 网卡上有的地址:正常
	got = texts(checkPorts(wantedPorts(map[string]string{"webListen": "10.0.0.5", "resellerEnabled": "false"}), look(map[int]listenerInfo{2053: {true, "m-ui", 1}, 2056: {true, "m-ui", 1}}), true, 1, local))
	mustHave(t, got, "OK 面板 2053/tcp 在监听(只在 10.0.0.5 上)")
	mustNot(t, got, "X  ", "只监听本机")
}

func TestDoctorPanel(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	url := "https://127.0.0.1:2053/app/"
	// 打不开:服务在跑才报(没在跑的上面已经说了)
	mustHave(t, texts(checkPanel(panelProbe{URL: url, Err: errors.New("dial tcp 127.0.0.1:2053: connect: connection refused")}, "0.8.0", true, now)),
		"X  面板在本机打不开("+url+"):连接被拒绝", "m-ui logs -e")
	if got := checkPanel(panelProbe{URL: url, Err: errors.New("x")}, "0.8.0", false, now); got != nil {
		t.Fatalf("服务没在跑时不重复报: %v", got)
	}
	// 老版本没有健康接口:只说能打开
	if got := texts(checkPanel(panelProbe{URL: url, Code: 200}, "0.8.0", true, now)); got != "OK 面板在本机能打开("+url+")\n" {
		t.Fatalf("没有健康接口时:\n%s", got)
	}
	mustHave(t, texts(checkPanel(panelProbe{URL: url, Code: 502}, "0.8.0", true, now)), "X  面板在本机应答 502")
	h := &healthInfo{OK: true, Core: true, Inbounds: 4, Lines: 4, Version: "0.8.0", StartedAt: now.Unix() - 3*86400 - 2*3600}
	got := texts(checkPanel(panelProbe{URL: url, Code: 200, Health: h}, "0.8.0", true, now))
	mustHave(t, got, "·  运行中的版本 v0.8.0,本次已运行 3 天 2 小时", "OK 数据面正常:4 条线路都在监听")
	mustNot(t, got, "!  ", "X  ")
	// 换了程序还没重启
	mustHave(t, texts(checkPanel(panelProbe{URL: url, Code: 200, Health: h}, "0.8.1", true, now)), "!  运行中的是 v0.8.0,磁盘上的程序是 v0.8.1", "m-ui restart")
	// 没有线路
	mustHave(t, texts(checkPanel(panelProbe{URL: url, Code: 200, Health: &healthInfo{OK: true, Version: "0.8.0"}}, "0.8.0", true, now)), "·  这台没有部署线路")
	// 数据面没起 / 只起了一部分,带上重载的报错
	down := &healthInfo{Lines: 3, Version: "0.8.0"}
	down.Reload.At, down.Reload.Error = now.Unix()-60, "listen tcp :443: bind: address already in use"
	mustHave(t, texts(checkPanel(panelProbe{URL: url, Code: 200, Health: down}, "0.8.0", true, now)),
		"X  数据面(sing-box)没在跑:这台的 3 条线路都连不上", "m-ui render", "++ 最近一次重载失败", "address already in use")
	part := &healthInfo{Core: true, Inbounds: 2, Lines: 3, Version: "0.8.0"}
	mustHave(t, texts(checkPanel(panelProbe{URL: url, Code: 200, Health: part}, "0.8.0", true, now)), "X  数据面只监听上 2 个入站,应当有 3 个")
}

func TestDoctorRole(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	mustHave(t, texts(checkRole(map[string]string{}, nil, now)), "·  这台是主机")
	mustHave(t, texts(checkRole(map[string]string{}, &healthInfo{Nodes: 5}, now)), "OK 这台是主机:5 台副机都联系得上")
	mustHave(t, texts(checkRole(map[string]string{}, &healthInfo{Nodes: 5, NodesDown: 2}, now)), "!  这台是主机:5 台副机里有 2 台联系不上", "「服务器」页")
	node := map[string]string{"nodeMode": "true"}
	mustHave(t, texts(checkRole(node, &healthInfo{}, now)), "·  这台是副机,还没有主机来接入", "接入信息")
	node["hubRevision"] = "r9"
	mustHave(t, texts(checkRole(node, nil, now)), "·  这台是副机,已有主机接入")
	mustHave(t, texts(checkRole(node, &healthInfo{HubSeenAt: now.Unix() - 20}, now)), "OK 这台是副机,主机 20 秒前联系过")
	mustHave(t, texts(checkRole(node, &healthInfo{HubSeenAt: now.Unix() - 3600}, now)), "!  这台是副机,主机已经 1 小时没联系这台", "令牌")
	mustHave(t, texts(checkRole(node, &healthInfo{StartedAt: now.Unix() - 1800}, now)), "!  这台是副机,本次启动以来(30 分钟)主机还没联系过")
	mustHave(t, texts(checkRole(node, &healthInfo{StartedAt: now.Unix() - 30}, now)), "·  这台是副机,刚启动")
	// 主机容灾开着才提
	mustNot(t, texts(checkRole(map[string]string{"haMode": "auto"}, nil, now)), "主机容灾")
	mustHave(t, texts(checkRole(map[string]string{"haMode": "auto", "haEpoch": "3"}, nil, now)), "·  主机容灾开着(主机失联后自动接任)", "m-ui ha status")
	mustHave(t, texts(checkRole(map[string]string{"haMode": "confirm", "haEpoch": "1"}, nil, now)), "先确认再接任")
	mustNot(t, texts(checkRole(map[string]string{"haMode": "off", "haEpoch": "3"}, nil, now)), "主机容灾")
}

func TestDoctorCerts(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	// 没给面板配证书:只提醒一句,不算问题
	if got := texts(checkCerts(map[string]string{}, nil, now)); !strings.HasPrefix(got, "·  面板走的是 HTTP") || strings.Count(got, "\n") != 1 {
		t.Fatalf("没配证书时:\n%s", got)
	}
	s := map[string]string{
		"webCertFile": "/c/a.pem", "webKeyFile": "/c/a.key",
		"subCertFile": "/c/a.pem", "subKeyFile": "/c/a.key", // 和面板同一张:合成一行
		"certFile": "/c/line.pem", "keyFile": "/c/line.key",
		"resellerCertFile": "/c/r.pem", "resellerKeyFile": "/c/r.key",
	}
	w := wantedCerts(s)
	if len(w) != 3 || w[0].Name != "面板、订阅" || w[1].Name != "代理面板" || w[2].Name != "线路" {
		t.Fatalf("证书清单不对: %+v", w)
	}
	s["nodeMode"] = "true" // 副机不起代理面板:它的证书不查
	if w := wantedCerts(s); len(w) != 2 {
		t.Fatalf("副机不该查代理面板的证书: %+v", w)
	}
	delete(s, "nodeMode")
	stat := func(c certWant) certState {
		switch c.Cert {
		case "/c/a.pem":
			return certState{NotAfter: now.Add(60 * 24 * time.Hour)}
		case "/c/line.pem":
			return certState{NotAfter: now.Add(5 * 24 * time.Hour)}
		}
		return certState{Err: errors.New("证书文件不存在: /c/r.pem")}
	}
	got := texts(checkCerts(s, stat, now))
	mustHave(t, got, "OK 面板、订阅的证书有效,还有 60 天(到 2027-01-17)", "!  线路的证书还有 5 天到期(2026-11-23)", "X  代理面板的证书用不了:证书文件不存在", "「证书」页")
	mustNot(t, got, "HTTP")
	expired := func(certWant) certState { return certState{NotAfter: now.Add(-48 * time.Hour)} }
	mustHave(t, texts(checkCerts(map[string]string{"webCertFile": "/a", "webKeyFile": "/b"}, expired, now)), "X  面板的证书已经过期(2026-11-16 到期)")
}

func TestDoctorDomains(t *testing.T) {
	s := map[string]string{"webDomain": " Panel.Example.com ", "subDomain": "sub.example.com"}
	if d := wantedDomains(s); len(d) != 2 || d[0] != "panel.example.com" || d[1] != "sub.example.com" {
		t.Fatalf("域名清单: %v", d)
	}
	if d := wantedDomains(map[string]string{"webDomain": "203.0.113.7", "subDomain": "a.example.com"}); len(d) != 1 || d[0] != "a.example.com" {
		t.Fatalf("填的是 IP 就不查解析: %v", d)
	}
	if d := wantedDomains(map[string]string{"webDomain": "a.example.com", "subDomain": "A.example.com"}); len(d) != 1 {
		t.Fatalf("同一个域名只查一次: %v", d)
	}
	dns := map[string][]string{
		"panel.example.com": {"203.0.113.7"},
		"sub.example.com":   {"198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4"},
		"v6.example.com":    {"2001:db8::7"},
	}
	lookup := func(h string) ([]string, error) {
		if a, ok := dns[h]; ok {
			return a, nil
		}
		return nil, errors.New("lookup " + h + ": no such host")
	}
	self := []string{"", "10.0.0.5", "203.0.113.7", "2001:0db8::7", "fe80::1"}
	got := texts(checkDomains([]string{"panel.example.com", "sub.example.com", "v6.example.com", "gone.example.com"}, lookup, self))
	mustHave(t, got,
		"OK 域名 panel.example.com 解析到这台(203.0.113.7)",
		"!  域名 sub.example.com 解析到 198.51.100.1、198.51.100.2、198.51.100.3 等 4 个,不是这台的地址(203.0.113.7、2001:db8::7)",
		"CDN",
		"OK 域名 v6.example.com 解析到这台(2001:db8::7)", // 写法不同的同一个 IPv6 也认
		"X  域名 gone.example.com 解析不出来:没有这个域名")
	// 探测不到自己的公网地址:只陈述,不判断
	mustHave(t, texts(checkDomains([]string{"sub.example.com"}, lookup, []string{"10.0.0.5", "127.0.0.1"})), "·  域名 sub.example.com 解析到", "没法比对")
	// 内网地址对得上也算这台(域名直接解析到内网的)
	dns["lan.example.com"] = []string{"10.0.0.5"}
	mustHave(t, texts(checkDomains([]string{"lan.example.com"}, lookup, self)), "OK 域名 lan.example.com 解析到这台(10.0.0.5)")
}

func TestDoctorSystem(t *testing.T) {
	free, total, ok := parseDF("Filesystem     1024-blocks     Used Available Capacity Mounted on\n/dev/root         40581564 11350424  29214756      28% /\n")
	if !ok || total != 40581564<<10 || free != 29214756<<10 {
		t.Fatalf("df 解析不对: %d %d %v", free, total, ok)
	}
	if _, _, ok := parseDF("df: /nope: No such file or directory\n"); ok {
		t.Fatal("乱七八糟的输出不该解析成功")
	}
	mustHave(t, texts(checkDisk(free, total, true)), "OK 磁盘还剩 27.9 GB(共 38.7 GB)")
	mustHave(t, texts(checkDisk(800<<20, 40<<30, true)), "!  数据目录所在的磁盘只剩 800 MB", "磁盘清理")
	mustHave(t, texts(checkDisk(3<<30, 100<<30, true)), "!  数据目录所在的磁盘只剩 3.0 GB") // 不到 5%
	mustHave(t, texts(checkDisk(100<<20, 40<<30, true)), "X  数据目录所在的磁盘只剩 100 MB", "流量记不上")
	if checkDisk(0, 0, false) != nil || checkClock(false, false) != nil {
		t.Fatal("问不到的不报")
	}
	mustHave(t, texts(checkClock(true, true)), "OK 系统时间已经和网络对过时")
	mustHave(t, texts(checkClock(false, true)), "!  系统时间没有和网络对时", "timedatectl set-ntp true")
	for d, want := range map[time.Duration]string{
		45 * time.Second: "45 秒", 12 * time.Minute: "12 分钟", 3*time.Hour + 5*time.Minute: "3 小时 5 分钟", 2 * time.Hour: "2 小时",
		50 * time.Hour: "2 天 2 小时", 72 * time.Hour: "3 天", -time.Second: "0 秒",
	} {
		if got := spanText(d); got != want {
			t.Errorf("spanText(%v) = %q,要 %q", d, got, want)
		}
	}
}

func TestDoctorFirewall(t *testing.T) {
	ports := wantedPorts(map[string]string{})
	mustHave(t, texts(checkFirewall(fwProbe{Skip: "防火墙状态读不出来:x"})), "·  防火墙状态读不出来")
	got := texts(checkFirewall(fwProbe{Ports: ports}))
	mustHave(t, got, "·  系统防火墙没有在拦入站", "云厂商的安全组", "TCP 2053、2056、2054")
	mustNot(t, got, "黑名单")
	mustHave(t, texts(checkFirewall(fwProbe{Ports: ports, Opened: true, Status: fw.Status{Frontend: fw.FrontUFW}})), "·  系统防火墙已经放开", "m-ui firewall undo")
	mustHave(t, texts(checkFirewall(fwProbe{Ports: ports, Status: fw.Status{Frontend: fw.FrontUFW, Blocking: true}})), "·  系统防火墙(ufw)在拦没放行的入站;m-ui 用到的端口会自动在它那里放行")
	mustHave(t, texts(checkFirewall(fwProbe{Ports: ports, Status: fw.Status{Frontend: fw.FrontNFTables, Blocking: true}})), "!  系统防火墙是手写的 nftables 规则", "TCP 2053、2056、2054", "m-ui firewall open")
	mustHave(t, texts(checkFirewall(fwProbe{Ports: ports, Paused: true})), "!  这台暂停了面板「防火墙」页的规则", "m-ui firewall resume")
	// 把自己拉黑了:最要紧的一条
	mustHave(t, texts(checkFirewall(fwProbe{Ports: ports, Bans: 4, SSHIP: "203.0.113.5", SSHBanned: true})), "X  你现在的 IP 203.0.113.5 在黑名单里", "m-ui unban 203.0.113.5")
	mustHave(t, texts(checkFirewall(fwProbe{Ports: ports, Bans: 4, SSHIP: "203.0.113.5"})), "OK 你现在的 IP 203.0.113.5 不在黑名单里(名单里有 4 条)")
	mustHave(t, texts(checkFirewall(fwProbe{Ports: ports, Bans: 4})), "·  黑名单里有 4 条", "m-ui unban <IP>")
}

// sudo 会把 SSH_CONNECTION 丢掉:顺着父进程的环境往上找。
func TestDoctorSSHFromProc(t *testing.T) {
	env := "LANG=C.UTF-8\x00SSH_CLIENT=203.0.113.5 51234 22\x00SSH_CONNECTION=203.0.113.5 51234 172.31.0.9 22\x00HOME=/home/u\x00"
	if got := sshIPFromEnviron([]byte(env)); got != "203.0.113.5" {
		t.Fatalf("从环境里读来源地址: %q", got)
	}
	if got := sshIPFromEnviron([]byte("SSH_CONNECTION=2001:db8::9 51234 2001:db8::1 22\x00")); got != "2001:db8::9" {
		t.Fatalf("IPv6: %q", got)
	}
	for _, env := range []string{"", "HOME=/root\x00", "SSH_CONNECTION=\x00", "SSH_CONNECTION=不是地址 1 2 3\x00", "XSSH_CLIENT=203.0.113.5 1 2\x00"} {
		if got := sshIPFromEnviron([]byte(env)); got != "" {
			t.Errorf("%q 不该读出地址: %q", env, got)
		}
	}
	for stat, want := range map[string]int{
		"4242 (sudo) S 4100 4242 4100 34816 4242 4194560 ...": 4100,
		"77 (tmux: server (1)) S 1 77 77 0 -1 4194368 ...":    1, // 进程名里有空格和括号
		"9 (a) b) R 8 9 9 0": 8,
		"乱七八糟":               0,
		"12 (x)":             0,
	} {
		if got := procParent(stat); got != want {
			t.Errorf("procParent(%q) = %d,要 %d", stat, got, want)
		}
	}
}

func TestDoctorWhoAndLogs(t *testing.T) {
	for out, want := range map[string]string{
		"ubuntu   pts/0        2026-11-18 10:00 (203.0.113.5)\n":      "203.0.113.5",
		"root     pts/1        Nov 18 10:00 (2001:db8::9)\n":          "2001:db8::9",
		"root     pts/1        Nov 18 10:00 (fe80::1%eth0)\n":         "fe80::1",
		"root     tty1         2026-11-18 10:00\n":                    "",
		"root     pts/2        2026-11-18 10:00 (tmux(1234).%0)\n":    "",
		"root     pts/3        2026-11-18 10:00 (host.example.com)\n": "",
		"": "",
	} {
		if got := whoHost(out); got != want {
			t.Errorf("who -m %q → %q,要 %q", out, got, want)
		}
	}
	log := strings.Join([]string{
		"2026-11-18T10:00:00+0000 host m-ui[1]: 2026/11/18 10:00:00 INFO - 启动",
		"2026-11-18T10:00:01+0000 host m-ui[1]: 2026/11/18 10:00:01 ERROR - 订阅端口监听失败: listen tcp :2056: bind: address already in use",
		"2026-11-18T10:01:01+0000 host m-ui[1]: 2026/11/18 10:01:01 ERROR - 订阅端口监听失败: listen tcp :2056: bind: address already in use",
		"2026-11-18T10:02:00+0000 host m-ui[1]: 2026/11/18 10:02:00 WARNING - 别的",
		"2026-11-18T10:03:00+0000 host m-ui[1]: panic: runtime error",
		"2026-11-18T10:04:00+0000 host m-ui[1]: 2026/11/18 10:04:00 ERROR - " + strings.Repeat("长", 400),
		"",
	}, "\n")
	got := errLinesOf(log, 5)
	if len(got) != 3 {
		t.Fatalf("同一个错只留最近一条,应当 3 条: %d\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "10:01:01") || !strings.Contains(got[1], "panic:") || !strings.HasSuffix(got[2], "…") {
		t.Fatalf("顺序(旧 → 新)或截断不对:\n%s", strings.Join(got, "\n"))
	}
	if n := len(errLinesOf(log, 1)); n != 1 {
		t.Fatalf("只要 1 条时给 %d 条", n)
	}
	// 启动失败是程序直接打的(没有「级别 - 」那一段),每次重试进程号都不一样:也只留最近一条
	loop := "2026-11-18T03:00:59+00:00 h m-ui[23800]: 启动失败: 启动订阅服务: listen tcp 0.0.0.0:2056: bind: address already in use\n" +
		"2026-11-18T03:01:02+00:00 h m-ui[23808]: 启动失败: 启动订阅服务: listen tcp 0.0.0.0:2056: bind: address already in use\n"
	if got := errLinesOf(loop, 5); len(got) != 1 || !strings.Contains(got[0], "23808") {
		t.Fatalf("反复重试的同一个错应当只留最近一条: %v", got)
	}
	if errLinesOf("2026 INFO - 一切正常\n", 5) != nil {
		t.Fatal("没有报错时应当是空的")
	}
	for in, want := range map[string]string{
		"dial tcp 127.0.0.1:2053: connect: connection refused":                       "连接被拒绝(没有程序在监听)",
		"Get \"https://x\": context deadline exceeded (Client.Timeout exceeded ...)": "超时没有应答",
		"lookup a.example.com: no such host":                                         "没有这个域名",
		"tls: handshake failure":                                                     "handshake failure",
	} {
		if got := shortErr(errors.New(in)); got != want {
			t.Errorf("shortErr(%q) = %q,要 %q", in, got, want)
		}
	}
}

// 打印:不带颜色时一个转义符都没有(贴给别人、存成文件);结论数得对;返回有问题的条数。
func TestDoctorPrint(t *testing.T) {
	secs := []section{
		{"服务", []finding{fOK("服务在运行")}},
		{"空段不打印", nil},
		{"端口", []finding{fBad("面板端口 2053 被别的程序占着", "换个端口"), fWarn("证书快到期", "续期"), fInfo("说明一句")}},
	}
	var plain bytes.Buffer
	if bad := printDoctor(&plain, secs, false); bad != 1 {
		t.Fatalf("有问题的条数应当是 1,得 %d", bad)
	}
	out := plain.String()
	if strings.Contains(out, "\033[") {
		t.Fatalf("不带颜色时不该有转义符:\n%q", out)
	}
	mustHave(t, out, "m-ui 体检", "  服务\n   ✓ 服务在运行", "   ✗ 面板端口 2053 被别的程序占着\n     → 换个端口", "   ! 证书快到期", "   · 说明一句",
		"结论:1 项正常,1 项要留意,1 项有问题", "没有密码、令牌和订阅地址")
	mustNot(t, out, "空段不打印")
	var colored bytes.Buffer
	printDoctor(&colored, secs, true)
	if !strings.Contains(colored.String(), colorRed) || !strings.Contains(colored.String(), colorGreen) {
		t.Fatal("带颜色时应当有颜色")
	}
	var clean bytes.Buffer
	if bad := printDoctor(&clean, []section{{"服务", []finding{fOK("服务在运行"), fInfo("这台是主机")}}}, false); bad != 0 {
		t.Fatalf("没问题时返回 0,得 %d", bad)
	}
	mustHave(t, clean.String(), "结论:1 项正常,没发现问题")
}
