package runner

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/fw"
)

// fwFake 假的系统命令执行器:记下每次调用,按「命令 参数」的前缀配输出 / 错误;on 在每次调用后跑(用来模拟「ufw disable 之后状态变了」)。
type fwFake struct {
	mu    sync.Mutex
	bins  map[string]bool
	out   map[string]string
	fail  map[string]string
	calls []string
	stdin []string
	on    func(f *fwFake, line string)
}

func newFwFake(bins ...string) *fwFake {
	f := &fwFake{bins: map[string]bool{}, out: map[string]string{}, fail: map[string]string{}}
	for _, b := range bins {
		f.bins[b] = true
	}
	return f
}

func (f *fwFake) Has(name string) bool { return f.bins[name] }

func (f *fwFake) Run(stdin, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, line)
	f.stdin = append(f.stdin, stdin)
	if f.on != nil {
		defer f.on(f, line)
	}
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

func (f *fwFake) with(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var got []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			got = append(got, c)
		}
	}
	return got
}

// lastScript 最近一次喂给 nft -f - 的脚本。
func (f *fwFake) lastScript() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i] == "nft -f -" {
			return f.stdin[i]
		}
	}
	return ""
}

func (f *fwFake) reset() {
	f.mu.Lock()
	f.calls, f.stdin = nil, nil
	f.mu.Unlock()
}

func fwTestRunner(t *testing.T, ex fw.Runner) *Runner {
	t.Helper()
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	r.fw.exec = ex
	// 本机(id 1)和另一台副机(id 2)
	var local model.Node
	if r.db.Where("is_local = ?", true).First(&local).Error != nil {
		local = model.Node{Name: "主机", IsLocal: true, Enabled: true}
		if err := r.db.Create(&local).Error; err != nil {
			t.Fatal(err)
		}
	}
	r.db.Model(&model.Node{}).Where("id = ?", local.Id).Update("public_ip", "198.51.100.10")
	tw := model.Node{Name: "tw", Enabled: true, PublicIP: "198.51.100.20", PublicIP6: "2001:db8:c::20", Addr: "https://node.example.com:2053"}
	if err := r.db.Create(&tw).Error; err != nil {
		t.Fatal(err)
	}
	if local.Id != 1 || tw.Id != 2 {
		t.Fatalf("测试假定本机是 1 号、另一台是 2 号: %d %d", local.Id, tw.Id)
	}
	return r
}

func ids(t *testing.T, v ...uint) json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(v)
	return b
}

// 一轮对账:只有启用着、没到期、管得到这台的规则才落;放行端口不进 m-ui 自己那张表;
// 集群里的服务器在 m-ui 的端口上排在所有规则前面放行;规则没变时下一轮不碰系统。
func TestFwTickRules(t *testing.T) {
	f := newFwFake("nft")
	r := fwTestRunner(t, f)
	now := time.Now().Unix()
	rows := []model.FwRule{
		{Id: 1, Kind: fw.KindDeny, Sources: "203.0.113.9/32", Enabled: true},
		{Id: 2, Kind: fw.KindOnly, Proto: "tcp", Ports: "22", Sources: "198.51.100.7/32", Enabled: true},
		{Id: 3, Kind: fw.KindAllow, Proto: "tcp", Ports: "8080", Enabled: true},
		{Id: 4, Kind: fw.KindDeny, Sources: "203.0.113.40/32", Enabled: true, Expiry: now - 5},         // 到期了
		{Id: 5, Kind: fw.KindDeny, Sources: "203.0.113.50/32", Enabled: true, NodeIds: ids(t, 2)},      // 只管另一台
		{Id: 6, Kind: fw.KindDeny, Sources: "203.0.113.60/32", Enabled: true, NodeIds: ids(t, 1, 2)},   // 点名了这台
		{Id: 7, Kind: fw.KindDeny, Sources: "203.0.113.70/32", Enabled: true, Expiry: now + 3600},      // 还没到期
		{Id: 8, Kind: fw.KindOnly, Proto: "tcp", Ports: "", Sources: "198.51.100.7/32", Enabled: true}, // 坏的:只允许却没填端口
	}
	for i := range rows {
		if err := r.db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	off := model.FwRule{Id: 9, Kind: fw.KindDeny, Sources: "203.0.113.90/32", Enabled: true}
	r.db.Create(&off)
	r.db.Model(&model.FwRule{}).Where("id = ?", 9).Update("enabled", false) // 关着的

	st := r.fwTick(true)
	s := f.lastScript()
	for _, want := range []string{
		"203.0.113.9", "203.0.113.60", "203.0.113.70",
		`tcp dport { 22 } ip saddr { 198.51.100.7 } accept comment "rule 2"`,
		`tcp dport { 22 } drop comment "rule 2"`,
		// 集群里的另一台(公网 v4、v6)在面板、订阅端口上放行;域名形式的连接地址认不出,不算
		`ip saddr { 198.51.100.10, 198.51.100.20 } tcp dport { 2053, 2056 } accept comment "cluster"`,
		`ip6 saddr { 2001:db8:c::20 } tcp dport { 2053, 2056 } accept comment "cluster"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("脚本里缺少 %q:\n%s", want, s)
		}
	}
	for _, not := range []string{"8080", "203.0.113.40", "203.0.113.50", "203.0.113.90"} {
		if strings.Contains(s, not) {
			t.Errorf("脚本里不该有 %q:\n%s", not, s)
		}
	}
	if strings.Index(s, `comment "cluster"`) > strings.Index(s, "@ban4 drop") {
		t.Errorf("集群放行要排在拉黑前面:\n%s", s)
	}
	if st.Rules != 5 || st.Engine != fw.EngineNFT || st.Frontend != fw.FrontNone || st.Blocking || st.Allow != "" || st.Ports != 2 || st.At == 0 {
		t.Fatalf("状态不对: %+v", st)
	}
	if !strings.Contains(st.Error, "#8") {
		t.Fatalf("坏规则应该报在状态里: %q", st.Error)
	}
	// 主机负责把到期的删掉(副机不删,等主机的快照)
	var n int64
	r.db.Model(&model.FwRule{}).Where("id = ?", 4).Count(&n)
	if n != 0 {
		t.Fatal("到期的规则主机应该删掉")
	}

	// 规则没变:下一轮不碰系统
	f.reset()
	r.fwTick(false)
	if len(f.with("")) != 0 {
		t.Fatalf("规则没变不该跑系统命令: %v", f.with(""))
	}
	// 加一条:马上重建
	r.db.Create(&model.FwRule{Id: 20, Kind: fw.KindDeny, Sources: "203.0.113.200/32", Enabled: true})
	r.fwTick(false)
	if !strings.Contains(f.lastScript(), "203.0.113.200") {
		t.Fatalf("新规则没落下去:\n%s", f.lastScript())
	}
	// 集群里加了一台:放行名单跟着变
	r.db.Create(&model.Node{Id: 3, Name: "jp", Enabled: true, PublicIP: "198.51.100.30"})
	f.reset()
	r.fwTick(false)
	if !strings.Contains(f.lastScript(), "198.51.100.30") {
		t.Fatalf("新服务器没进放行名单:\n%s", f.lastScript())
	}

	// 命令行暂停:拉黑、只允许全撤(表删掉),状态里标出来;恢复后回来
	if err := r.setSetting(fwPausedKey, "true"); err != nil {
		t.Fatal(err)
	}
	st = r.fwTick(false)
	if s := f.lastScript(); s != "table inet m_ui\ndelete table inet m_ui\n" || !st.Paused {
		t.Fatalf("暂停后应把表撤掉: paused=%v\n%s", st.Paused, s)
	}
	_ = r.setSetting(fwPausedKey, "")
	if st = r.fwTick(false); st.Paused || !strings.Contains(f.lastScript(), "203.0.113.9") {
		t.Fatalf("恢复后规则应回来: %+v\n%s", st, f.lastScript())
	}

	// 命令行暂停后马上恢复,服务一轮都没赶上「暂停着」(m-ui firewall pause 是当场把表撤掉的):恢复后的这一轮要把规则补回去。
	// 以前恢复存回空串,服务看到的设置和撤表之前一模一样,以为规则还在,要等 5 分钟一次的核对才补(实机测出来的)。
	f.reset()
	r.fwTick(false)
	if len(f.with("")) != 0 {
		t.Fatalf("规则没变不该跑系统命令: %v", f.with(""))
	}
	_ = saveSettings(r.db, map[string]string{fwPausedKey: fwPausedValue(true)})  // 命令行暂停(表被它当场撤掉)
	_ = saveSettings(r.db, map[string]string{fwPausedKey: fwPausedValue(false)}) // 紧接着恢复
	f.reset()
	if st = r.fwTick(false); st.Paused || !strings.Contains(f.lastScript(), "203.0.113.9") {
		t.Fatalf("暂停后马上恢复:这一轮应把规则补回去: %+v\n%s", st, f.lastScript())
	}
	// 恢复之后再没动过:又回到不碰系统
	f.reset()
	r.fwTick(false)
	if len(f.with("")) != 0 {
		t.Fatalf("恢复并重建之后不该每轮都跑系统命令: %v", f.with(""))
	}
	if a, b := fwPausedValue(false), fwPausedValue(false); a == "true" || a == "" || strings.EqualFold(a, "true") {
		t.Fatalf("恢复时存的值不能是 true 或空: %q %q", a, b)
	}

	// 副机不删到期的规则(主机那份是底),但到期的不再生效
	_ = r.setSetting("nodeMode", "true")
	r.db.Create(&model.FwRule{Id: 30, Kind: fw.KindDeny, Sources: "203.0.113.230/32", Enabled: true, Expiry: now - 1})
	r.fwTick(true)
	r.db.Model(&model.FwRule{}).Where("id = ?", 30).Count(&n)
	if n != 1 || strings.Contains(f.lastScript(), "203.0.113.230") {
		t.Fatalf("副机应留着到期的行、但不让它生效: 行数 %d\n%s", n, f.lastScript())
	}
}

// 系统防火墙开着:m-ui 的端口和「放行端口」的规则在它那里放行,记下哪些是自己加的;
// 关掉自动放行,m-ui 的端口收回、规则要的留着;用户自己早就放行的一条不碰。
func TestFwTickAllow(t *testing.T) {
	f := newFwFake("ufw", "nft")
	added := []string{"ufw allow 22/tcp"}
	render := func() {
		f.out["ufw show added"] = "Added user rules (see 'ufw status' for running firewall):\n" + strings.Join(added, "\n") + "\n"
	}
	render()
	f.out["ufw status"] = "Status: active\n"
	f.on = func(f *fwFake, line string) { // 模拟 ufw 自己的那张表
		if strings.HasPrefix(line, "ufw allow ") {
			added = append(added, strings.TrimSuffix(line, " comment m-ui")+" comment 'm-ui'")
		}
		if strings.HasPrefix(line, "ufw delete allow ") {
			spec := strings.TrimPrefix(line, "ufw delete allow ")
			var keep []string
			for _, a := range added {
				if !strings.HasPrefix(a, "ufw allow "+spec+" ") && a != "ufw allow "+spec {
					keep = append(keep, a)
				}
			}
			added = keep
		}
		render()
	}
	r := fwTestRunner(t, f)
	r.db.Create(&model.FwRule{Id: 1, Kind: fw.KindAllow, Proto: "tcp", Ports: "8080", Enabled: true})
	r.db.Create(&model.FwRule{Id: 2, Kind: fw.KindAllow, Ports: "9000-9010", Enabled: true, NodeIds: ids(t, 2)}) // 只管另一台

	st := r.fwTick(true)
	if got := strings.Join(f.with("ufw allow"), "|"); got != "ufw allow 2053/tcp comment m-ui|ufw allow 2056/tcp comment m-ui|ufw allow 8080/tcp comment m-ui" {
		t.Fatalf("放行不对: %s", got)
	}
	if st.Frontend != fw.FrontUFW || !st.Blocking || st.Allow != model.FwAllowOK || st.Allowed != 3 || st.Ports != 2 || st.Rules != 1 || st.Error != "" {
		t.Fatalf("状态不对: %+v", st)
	}
	if rec := fwAllowedOf(r.db); rec.Frontend != fw.FrontUFW || strings.Join(rec.Ports, ",") != "2053/tcp,2056/tcp,8080/tcp" {
		t.Fatalf("记录不对: %+v", rec)
	}
	// 下一轮什么都没变:不碰系统
	f.reset()
	r.fwTick(false)
	if len(f.with("")) != 0 {
		t.Fatalf("没变不该跑系统命令: %v", f.with(""))
	}
	// 关掉自动放行:m-ui 自己的两个端口收回;规则要的 8080 留着;用户自己的 22 不碰
	_ = r.setSetting(fwAutoAllowKey, "false")
	st = r.fwTick(false)
	if got := strings.Join(f.with("ufw delete"), "|"); got != "ufw delete allow 2053/tcp|ufw delete allow 2056/tcp" {
		t.Fatalf("收回不对: %s", got)
	}
	if st.Allow != model.FwAllowOff || st.Allowed != 1 || strings.Join(added, "|") != "ufw allow 22/tcp|ufw allow 8080/tcp comment 'm-ui'" {
		t.Fatalf("关掉自动放行后: %+v / %v", st, added)
	}
	// 规则删了:8080 也收回
	r.db.Where("id = ?", 1).Delete(&model.FwRule{})
	st = r.fwTick(false)
	if st.Allowed != 0 || strings.Join(added, "|") != "ufw allow 22/tcp" {
		t.Fatalf("规则删掉后应收回: %+v / %v", st, added)
	}
	// 放行出错:状态里说没成,原因带出来
	_ = r.setSetting(fwAutoAllowKey, "true")
	f.fail["ufw allow 2053/tcp"] = "ERROR: problem running ufw-init"
	st = r.fwTick(false)
	if st.Allow != model.FwAllowFail || !strings.Contains(st.Error, "ufw-init") {
		t.Fatalf("放行失败应报出来: %+v", st)
	}
}

// 放开 / 撤销;放开之后有人又把系统防火墙开回来,以现在为准。
func TestFwOpenUndo(t *testing.T) {
	f := newFwFake("ufw", "nft")
	f.out["ufw status"] = "Status: active\n"
	f.on = func(f *fwFake, line string) {
		switch line {
		case "ufw disable":
			f.out["ufw status"] = "Status: inactive\n"
		case "ufw --force enable":
			f.out["ufw status"] = "Status: active\n"
		}
	}
	r := fwTestRunner(t, f)
	_ = r.setSetting(fwAutoAllowKey, "false") // 这条测试只看放开 / 撤销
	st, err := r.FwOpen()
	if err != nil || len(f.with("ufw disable")) != 1 {
		t.Fatalf("放开: %v %v", err, f.calls)
	}
	if !st.Opened || st.Blocking || st.OpenedFrom != fw.FrontUFW || st.OpenedAt == 0 || st.Allow != "" {
		t.Fatalf("放开后的状态不对: %+v", st)
	}
	if o, was := fwOpenedOf(r.db); !was || strings.Join(o.Actions, ",") != fw.ActUFW {
		t.Fatalf("放开的记录不对: %+v %v", o, was)
	}
	// 再点一次:不重复做
	if _, err := r.FwOpen(); err != nil || len(f.with("ufw disable")) != 1 {
		t.Fatalf("已放开不该再做一遍: %v %v", err, f.with("ufw disable"))
	}
	// 撤销:回到放开前
	st, err = r.FwUndo()
	if err != nil || len(f.with("ufw --force enable")) != 1 || st.Opened || !st.Blocking {
		t.Fatalf("撤销: %v %+v %v", err, st, f.calls)
	}
	if _, was := fwOpenedOf(r.db); was {
		t.Fatal("撤销后记录应清掉")
	}
	// 没放开过:撤销什么都不做
	f.reset()
	if _, err := r.FwUndo(); err != nil || len(f.with("ufw --force")) != 0 {
		t.Fatalf("没放开过不该动: %v %v", err, f.calls)
	}

	// 放开之后,管理员自己又把 ufw 开回来了:记录作废,按「还拦着」处理
	if _, err := r.FwOpen(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.out["ufw status"] = "Status: active\n"
	f.mu.Unlock()
	st = r.fwTick(true)
	if st.Opened || !st.Blocking || st.Allow != model.FwAllowOff {
		t.Fatalf("系统防火墙被开回来后: %+v", st)
	}
	if _, was := fwOpenedOf(r.db); was {
		t.Fatal("被开回来后记录应作废")
	}
}

// iptables(Oracle 镜像那种)放开后重启,那条全放行被发行版的存档冲掉:对账时补回去。
func TestFwEnsureOpenAfterReboot(t *testing.T) {
	const rejecting = "-P INPUT ACCEPT\n-A INPUT -p tcp -m state --state NEW -m tcp --dport 22 -j ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"
	const open = "-P INPUT ACCEPT\n-A INPUT -m comment --comment m-ui-open -j ACCEPT\n-A INPUT -p tcp -m state --state NEW -m tcp --dport 22 -j ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"
	f := newFwFake("iptables", "nft")
	f.out["iptables -S INPUT"] = rejecting
	f.on = func(f *fwFake, line string) {
		if strings.HasPrefix(line, "iptables -I INPUT 1 -m comment --comment m-ui-open") {
			f.out["iptables -S INPUT"] = open
		}
	}
	r := fwTestRunner(t, f)
	st, err := r.FwOpen()
	if err != nil || !st.Opened || st.Blocking || st.OpenedFrom != fw.FrontIPTables {
		t.Fatalf("放开: %v %+v", err, st)
	}
	// 重启:规则回到存档里的样子
	f.mu.Lock()
	f.out["iptables -S INPUT"] = rejecting
	f.mu.Unlock()
	f.reset()
	st = r.fwTick(true)
	if len(f.with("iptables -I INPUT 1 -m comment --comment m-ui-open -j ACCEPT")) != 1 || !st.Opened || st.Blocking {
		t.Fatalf("被冲掉的全放行没补回: %+v %v", st, f.calls)
	}
	if _, was := fwOpenedOf(r.db); !was {
		t.Fatal("记录不该丢")
	}
}

// 副机记下主机实际连过来的地址:面板规则不挡它;只留最近几个。
func TestFwNotePeer(t *testing.T) {
	f := newFwFake("nft")
	r := fwTestRunner(t, f)
	r.db.Create(&model.FwRule{Id: 1, Kind: fw.KindOnly, Proto: "tcp", Ports: "2053", Sources: "198.51.100.7/32", Enabled: true})
	r.FwNotePeer("203.0.113.77")
	r.FwNotePeer("203.0.113.77") // 没变:什么都不做
	r.FwNotePeer("127.0.0.1")    // 回环不记
	r.FwNotePeer("bogus")
	if got := strings.Join(fwPeersOf(r.db), ","); got != "203.0.113.77" {
		t.Fatalf("记下的主机地址不对: %s", got)
	}
	if !r.clusterAddr(fwCluster(r.db)[len(fwCluster(r.db))-1].Addr()) {
		t.Fatal("clusterAddr 应该认得集群里的地址")
	}
	r.fwTick(true)
	if s := f.lastScript(); !strings.Contains(s, "203.0.113.77") || !strings.Contains(s, `tcp dport { 2053 } drop comment "rule 1"`) {
		t.Fatalf("主机连过来的地址没进放行名单:\n%s", s)
	}
	for _, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4"} {
		r.FwNotePeer(ip)
	}
	if got := fwPeersOf(r.db); len(got) != fwMaxPeers || got[0] != "203.0.113.4" || strings.Contains(strings.Join(got, ","), "203.0.113.77") {
		t.Fatalf("只留最近 %d 个: %v", fwMaxPeers, got)
	}
}

// 这台上 m-ui 用到的端口:面板、订阅、线路(TCP / UDP 按协议分)、端口跳跃的范围、由这台来转的中转入口。
func TestFwPorts(t *testing.T) {
	r := fwTestRunner(t, newFwFake())
	_ = r.setSetting("webPort", "8443")
	r.db.Create(&model.Line{Id: 1, Name: "vless", Protocol: "vless", Port: 443, Enabled: true})
	r.db.Create(&model.Line{Id: 2, Name: "hy2", Protocol: "hysteria2", Port: 8444, Enabled: true, Options: json.RawMessage(`{"port_hopping":"20000-20100"}`)})
	r.db.Create(&model.Line{Id: 3, Name: "ss", Protocol: "shadowsocks", Port: 8388, Enabled: true})
	r.db.Create(&model.Line{Id: 4, Name: "off", Protocol: "vless", Port: 9443, Enabled: true})
	r.db.Model(&model.Line{}).Where("id = ?", 4).Update("enabled", false)
	got := strings.Join(r.FwPorts(), " ")
	want := "443/tcp 2056/tcp 8388/tcp 8388/udp 8443/tcp 8444/udp 20000-20100/udp"
	if got != want {
		t.Fatalf("端口清单不对:\n得到 %s\n应为 %s", got, want)
	}
}
