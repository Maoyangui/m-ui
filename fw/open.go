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
	_, err := r.Run("", bin, "-I", "INPUT", "1", "-m", "comment", "--comment", openComment, "-j", "ACCEPT")
	return err
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

// AllowPorts 用户留着系统防火墙时,把 m-ui 用到的端口在它那里放行。只加放行,不删别人的规则。
//
//	ufw        → ufw allow 端口/协议(重复加没有副作用)
//	firewalld  → firewall-cmd --add-port(当下生效)+ --permanent --add-port(重启后还在)
//	iptables   → m-ui 自己的一条链 M-UI-ALLOW,整链重建后挂在 INPUT 第一条
//	原生 nft   → 不自动改,返回说明
//	没有防火墙 → 什么都不做
func AllowPorts(r Runner, ports []PortProto) error {
	st := Detect(r)
	if !st.Blocking {
		return nil
	}
	ports = normPorts(ports)
	switch st.Frontend {
	case FrontUFW:
		for _, p := range ports {
			if _, err := r.Run("", "ufw", "allow", strings.ReplaceAll(p.Port.String(), "-", ":")+"/"+p.Proto, "comment", "m-ui"); err != nil {
				return err
			}
		}
	case FrontFirewalld:
		for _, p := range ports {
			arg := "--add-port=" + p.Port.String() + "/" + p.Proto
			if _, err := r.Run("", "firewall-cmd", arg); err != nil {
				return err
			}
			if _, err := r.Run("", "firewall-cmd", "--permanent", arg); err != nil {
				return err
			}
		}
	case FrontIPTables:
		return allowIPTables(r, ports)
	case FrontNFTables:
		return fmt.Errorf("这台用的是原生 nftables 规则,面板不自动改它:请自己在规则里放行这些端口")
	}
	return nil
}

const allowChain = "M-UI-ALLOW"

func allowIPTables(r Runner, ports []PortProto) error {
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
