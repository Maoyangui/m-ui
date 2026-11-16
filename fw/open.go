package fw

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 「放开系统防火墙」做过的动作(记下来,撤销时照着还原;也用来在开机后把被冲掉的那条补回去)。
const (
	ActUFW       = "ufw-disable"      // ufw disable
	ActFirewalld = "firewalld-stop"   // systemctl disable --now firewalld
	ActIPTables  = "iptables-accept"  // iptables -I INPUT 1 -m comment --comment m-ui-open -j ACCEPT
	ActIP6Tables = "ip6tables-accept" // ip6tables 同上
)

// Opened 一次「放开」的记录。
type Opened struct {
	Frontend string   `json:"frontend"` // 放开前是哪种系统防火墙
	Actions  []string `json:"actions"`
	Backup   string   `json:"backup"` // 改之前的规则备份在哪个目录
	At       int64    `json:"at"`
}

// Open 放开系统防火墙:让它不再把没放行的入站挡在外面。可逆(Undo),改之前把现状备份到 backupRoot/fw-backup-<时间>/。
//
//	ufw 启用中           → ufw disable
//	firewalld 运行中     → systemctl disable --now firewalld
//	iptables 会拦         → INPUT 最前面插一条全放行(带注释 m-ui-open);原来的规则一条不动,删掉这一条就恢复原样
//	原生 nftables 会拦   → 不自动改(规则是用户自己写的,面板猜不出该怎么改),返回说明
//	本来就没拦           → 什么都不做
//
// 真要挡端口,在云厂商的安全组 / 防火墙里做;面板自己的规则(拉黑、端口只允许某些来源)不受这一步影响。
func Open(r Runner, backupRoot string, now time.Time) (Opened, error) {
	st := Detect(r)
	o := Opened{Frontend: st.Frontend, At: now.Unix()}
	if !st.Blocking {
		return o, nil
	}
	if st.Frontend == FrontNFTables {
		return o, fmt.Errorf("这台用的是原生 nftables 规则(%s),面板不自动改它:请在规则里放行 m-ui 用到的端口,或者自己把 input 的默认策略改成 accept", st.Detail)
	}
	o.Backup = backup(r, backupRoot, now)
	switch st.Frontend {
	case FrontUFW:
		if _, err := r.Run("", "ufw", "disable"); err != nil {
			return o, err
		}
		o.Actions = append(o.Actions, ActUFW)
	case FrontFirewalld:
		if _, err := r.Run("", "systemctl", "disable", "--now", "firewalld"); err != nil {
			return o, err
		}
		o.Actions = append(o.Actions, ActFirewalld)
	case FrontIPTables:
		for _, x := range []struct{ bin, act string }{{"iptables", ActIPTables}, {"ip6tables", ActIP6Tables}} {
			if !r.Has(x.bin) {
				continue
			}
			out, err := r.Run("", x.bin, "-S", "INPUT")
			if err != nil {
				continue
			}
			opened, why := inputBlocks(out)
			if why == "" || opened {
				continue // 这一族本来就不拦,或者已经放开过
			}
			if err := insertOpen(r, x.bin); err != nil {
				return o, err
			}
			o.Actions = append(o.Actions, x.act)
		}
		if r.Has("netfilter-persistent") { // 有持久化工具就顺手存一份;没有也不要紧,面板开机会补(EnsureOpen)
			_, _ = r.Run("", "netfilter-persistent", "save")
		}
	}
	return o, nil
}

func insertOpen(r Runner, bin string) error {
	if _, err := r.Run("", bin, "-I", "INPUT", "1", "-m", "comment", "--comment", openComment, "-j", "ACCEPT"); err != nil {
		return err
	}
	return pinFirst(r, bin, false) // 面板规则落在 iptables 里的机器:拉黑要排在这条全放行前面
}

// EnsureOpen 开机后、以及定时对账时调:放开时加的那条全放行要是不在了(重启后规则从发行版的存档里恢复,没有它),补回去。
// 只管 iptables 这一种:ufw disable、停用 firewalld 自己就是持久的。
func EnsureOpen(r Runner, o Opened) {
	for _, act := range o.Actions {
		bin := map[string]string{ActIPTables: "iptables", ActIP6Tables: "ip6tables"}[act]
		if bin == "" || !r.Has(bin) {
			continue
		}
		out, err := r.Run("", bin, "-S", "INPUT")
		if err != nil {
			continue
		}
		if opened, _ := inputBlocks(out); !opened {
			_ = insertOpen(r, bin)
		}
	}
}

// Undo 撤销一次「放开」:按记下的动作反着做。
func Undo(r Runner, o Opened) error {
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for _, act := range o.Actions {
		switch act {
		case ActUFW:
			_, err := r.Run("", "ufw", "--force", "enable")
			keep(err)
		case ActFirewalld:
			_, err := r.Run("", "systemctl", "enable", "--now", "firewalld")
			keep(err)
		case ActIPTables, ActIP6Tables:
			bin := "iptables"
			if act == ActIP6Tables {
				bin = "ip6tables"
			}
			for i := 0; i < 8; i++ { // 万一补过不止一条:删到没有为止
				if _, err := r.Run("", bin, "-D", "INPUT", "-m", "comment", "--comment", openComment, "-j", "ACCEPT"); err != nil {
					break
				}
			}
			if r.Has("netfilter-persistent") {
				_, _ = r.Run("", "netfilter-persistent", "save")
			}
		}
	}
	return first
}

// backup 把现在的规则另存一份(能存多少存多少,存不下不拦着后面的动作);返回目录,一样都没存下返回空。
func backup(r Runner, root string, now time.Time) string {
	if root == "" {
		return ""
	}
	dir := filepath.Join(root, "fw-backup-"+now.Format("20060102-150405"))
	wrote := false
	save := func(file, name string, args ...string) {
		if !r.Has(name) {
			return
		}
		out, err := r.Run("", name, args...)
		if err != nil || strings.TrimSpace(out) == "" {
			return
		}
		if os.MkdirAll(dir, 0o700) != nil {
			return
		}
		if os.WriteFile(filepath.Join(dir, file), []byte(out), 0o600) == nil {
			wrote = true
		}
	}
	save("iptables.rules", "iptables-save")
	save("ip6tables.rules", "ip6tables-save")
	save("ufw-status.txt", "ufw", "status", "verbose")
	save("firewalld-zones.txt", "firewall-cmd", "--list-all-zones")
	save("nft-ruleset.nft", "nft", "list", "ruleset")
	if !wrote {
		return ""
	}
	return dir
}

// PortProto 一个要放行的端口(或一段)和协议。
type PortProto struct {
	Port  PortRange
	Proto string // tcp | udp
}

// String "443/tcp"、"20000-30000/udp"。
func (p PortProto) String() string { return p.Port.String() + "/" + p.Proto }

// Allowed 系统防火墙里由 m-ui 加进去的放行。记下来是为了以后只收回自己加的:
// 加之前那里就已经放行的端口是用户自己的,不记、也永远不删。
type Allowed struct {
	Frontend string   `json:"frontend"` // 记的是哪种系统防火墙里的;换了一种,旧记录作废
	Ports    []string `json:"ports"`    // "443/tcp"、"20000-30000/udp"
}

// SyncAllow 用户留着系统防火墙时,让它里面「m-ui 放行的端口」正好是 want:缺的加上;以前加过、现在用不着的收回
// (只收 had 里记着是 m-ui 加的)。返回新的记录,下次当 had 传进来。不改默认策略,不碰别人的规则。
//
//	ufw        → ufw allow 端口/协议 comment m-ui;收回用 ufw delete allow
//	firewalld  → firewall-cmd --add-port / --remove-port,当下生效一遍、--permanent 一遍
//	iptables   → m-ui 自己的一条链 M-UI-ALLOW,整链重建;一个端口都不要了就把链撤掉
//	原生 nft   → 不自动改,有要放行的端口时返回说明
//	没在拦     → 什么都不做(没有系统防火墙、或已经放开),记录原样返回
func SyncAllow(r Runner, st Status, want []PortProto, had Allowed) (Allowed, error) {
	if !st.Blocking {
		return had, nil
	}
	want = normPorts(want)
	if had.Frontend != st.Frontend {
		had = Allowed{}
	}
	mine := map[string]bool{}
	for _, p := range had.Ports {
		mine[p] = true
	}
	wanted := map[string]bool{}
	for _, p := range want {
		wanted[p.String()] = true
	}
	now := Allowed{Frontend: st.Frontend}
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	// add / drop 各种系统防火墙自己的加法和收回法;has 这个端口那里现在放行着没有
	var has func(p string) bool
	var add, drop func(p string) error
	switch st.Frontend {
	case FrontUFW:
		exists := ufwAllowed(r)
		ufwSpec := func(p string) string { return strings.Replace(p, "-", ":", 1) }
		has = func(p string) bool { return exists[p] }
		add = func(p string) error { _, err := r.Run("", "ufw", "allow", ufwSpec(p), "comment", "m-ui"); return err }
		drop = func(p string) error { _, err := r.Run("", "ufw", "delete", "allow", ufwSpec(p)); return err }
	case FrontFirewalld:
		has = func(p string) bool {
			out, err := r.Run("", "firewall-cmd", "--query-port="+p)
			return err == nil && strings.TrimSpace(out) == "yes"
		}
		add = func(p string) error {
			if _, err := r.Run("", "firewall-cmd", "--add-port="+p); err != nil {
				return err
			}
			_, err := r.Run("", "firewall-cmd", "--permanent", "--add-port="+p)
			return err
		}
		drop = func(p string) error {
			_, err := r.Run("", "firewall-cmd", "--remove-port="+p)
			if _, err2 := r.Run("", "firewall-cmd", "--permanent", "--remove-port="+p); err == nil {
				err = err2
			}
			return err
		}
	case FrontIPTables:
		if err := allowIPTables(r, want); err != nil {
			return had, err
		}
		for _, p := range want {
			now.Ports = append(now.Ports, p.String())
		}
		return now, nil
	case FrontNFTables:
		if len(want) > 0 {
			return had, fmt.Errorf("这台用的是原生 nftables 规则,面板不自动改它:请自己在规则里放行这些端口")
		}
		return now, nil
	default:
		return had, nil
	}
	for _, p := range want {
		k := p.String()
		switch {
		case has(k):
			if mine[k] { // 上次加的还在:接着记;不是 m-ui 加的(用户自己早就放行了)不记
				now.Ports = append(now.Ports, k)
			}
		default:
			if err := add(k); err != nil {
				keep(err)
				continue
			}
			now.Ports = append(now.Ports, k)
		}
	}
	for _, k := range had.Ports {
		if wanted[k] {
			continue
		}
		if has(k) {
			if err := drop(k); err != nil {
				keep(err)
				now.Ports = append(now.Ports, k) // 没收回成:接着记,下次再试
			}
		}
	}
	sort.Strings(now.Ports)
	return now, first
}

// ufwAllowed 读 `ufw show added`:现在有哪些「对所有人放行某个端口」的规则("443/tcp"、"20000-30000/udp")。
// 只认最简单的那种写法(ufw allow 端口[/协议]);没写协议的两种协议都算。带来源、带网卡的不算:那不是对所有人放行。
func ufwAllowed(r Runner) map[string]bool {
	out, err := r.Run("", "ufw", "show", "added")
	got := map[string]bool{}
	if err != nil {
		return got
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "ufw" || f[1] != "allow" || (len(f) > 3 && f[3] != "comment") {
			continue
		}
		spec, proto, _ := strings.Cut(f[2], "/")
		ports, err := ParsePorts(spec)
		if err != nil || len(ports) == 0 {
			continue // 应用名(OpenSSH)之类:不是端口
		}
		for _, pr := range []string{"tcp", "udp"} {
			if proto != "" && proto != pr {
				continue
			}
			for _, p := range ports {
				got[p.String()+"/"+pr] = true
			}
		}
	}
	return got
}

const allowChain = "M-UI-ALLOW"

func allowIPTables(r Runner, ports []PortProto) error {
	if len(ports) == 0 { // 一个端口都不要放行了:把自己的链撤掉(本来就没有的不算错)
		for _, bin := range []string{"iptables", "ip6tables"} {
			if !r.Has(bin) {
				continue
			}
			if _, err := r.Run("", bin, "-C", "INPUT", "-j", allowChain); err == nil {
				_, _ = r.Run("", bin, "-D", "INPUT", "-j", allowChain)
				_, _ = r.Run("", bin, "-F", allowChain)
				_, _ = r.Run("", bin, "-X", allowChain)
			}
		}
		return nil
	}
	by := map[string][]PortRange{}
	for _, p := range ports {
		by[p.Proto] = append(by[p.Proto], p.Port)
	}
	lines := []string{}
	for _, proto := range []string{"tcp", "udp"} {
		for _, mp := range multiport(by[proto]) {
			lines = append(lines, fmt.Sprintf("-A %s -p %s -m multiport --dports %s -m comment --comment %s -j ACCEPT", allowChain, proto, strings.Join(mp, ","), allowComment))
		}
	}
	script := "*filter\n:" + allowChain + " - [0:0]\n-F " + allowChain + "\n" + strings.Join(lines, "\n")
	if len(lines) > 0 {
		script += "\n"
	}
	script += "COMMIT\n"
	for _, bin := range []string{"iptables", "ip6tables"} {
		if !r.Has(bin) || !r.Has(bin+"-restore") {
			continue
		}
		if _, err := r.Run(script, bin+"-restore", "--noflush"); err != nil {
			return err
		}
		if _, err := r.Run("", bin, "-C", "INPUT", "-j", allowChain); err != nil {
			if _, err := r.Run("", bin, "-I", "INPUT", "1", "-j", allowChain); err != nil {
				return err
			}
			if err := pinFirst(r, bin, false); err != nil { // 面板规则落在 iptables 里的机器:拉黑要排在放行前面
				return err
			}
		}
	}
	return nil
}

// normPorts 去重、排序(先 TCP 后 UDP,端口从小到大),结果稳定:同一组端口每次生成的规则一字不差。
func normPorts(ports []PortProto) []PortProto {
	seen := map[string]bool{}
	var out []PortProto
	for _, p := range ports {
		if p.Proto != "tcp" && p.Proto != "udp" {
			continue
		}
		if p.Port.From < 1 || p.Port.To > 65535 || p.Port.From > p.Port.To {
			continue
		}
		k := p.Proto + "/" + strconv.Itoa(p.Port.From) + "-" + strconv.Itoa(p.Port.To)
		if !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Proto != out[j].Proto {
			return out[i].Proto < out[j].Proto
		}
		if out[i].Port.From != out[j].Port.From {
			return out[i].Port.From < out[j].Port.From
		}
		return out[i].Port.To < out[j].Port.To
	})
	return out
}
