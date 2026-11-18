package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/fw"
	"github.com/Maoyangui/m-ui/runner"
)

// m-ui firewall … / m-ui unban …:本机防火墙的命令行(要 root)。不经运行中的服务,直接对本机动手,
// 结果记进库里;运行中的 m-ui 下一轮对账(30 秒内)就按新的来,面板「防火墙」页看到的也跟着变。
//
//	m-ui firewall status     系统防火墙是哪种、拦不拦、放开过没有;面板规则几条、暂停没有;黑名单几条
//	m-ui firewall open       放开系统防火墙(可撤销;改之前的规则备份在数据目录的 fw-backup-<时间>/)
//	m-ui firewall undo       撤销放开,系统防火墙回到放开之前的样子
//	m-ui firewall pause      这台暂停面板规则(拉黑、端口只给指定来源):一条规则把自己关在门外时,从控制台进来执行它
//	m-ui firewall resume     恢复面板规则
//	m-ui firewall detect     给安装脚本用:系统防火墙正在拦没放行的入站时打印它的种类,否则什么都不打印
//	m-ui unban <IP|网段>     把一个来源移出黑名单(面板 / 订阅 / API 的访问)
//	m-ui unban --all         清空黑名单

func fwUsage() {
	fmt.Fprintln(os.Stderr, "用法:m-ui firewall status|open|undo|pause|resume [-db 路径]")
	fmt.Fprintln(os.Stderr, "  status   系统防火墙是哪种、拦不拦、放开过没有;面板规则、黑名单各有几条")
	fmt.Fprintln(os.Stderr, "  open     放开系统防火墙(可撤销;改之前先备份)")
	fmt.Fprintln(os.Stderr, "  undo     撤销放开")
	fmt.Fprintln(os.Stderr, "  pause    这台暂停面板规则(一条规则把自己关在门外时用)")
	fmt.Fprintln(os.Stderr, "  resume   恢复面板规则")
	fmt.Fprintln(os.Stderr, "另:m-ui unban <IP|网段> / m-ui unban --all   把来源移出黑名单")
}

// frontName 系统防火墙的种类,给人看的写法。
func frontName(f string) string {
	switch f {
	case fw.FrontUFW:
		return "ufw"
	case fw.FrontFirewalld:
		return "firewalld"
	case fw.FrontIPTables:
		return "iptables 规则"
	case fw.FrontNFTables:
		return "nftables 规则"
	}
	return "没有"
}

// openedHow 放开时做了什么,一句话。
func openedHow(o fw.Opened) string {
	var parts []string
	for _, a := range o.Actions {
		switch a {
		case fw.ActUFW:
			parts = append(parts, "执行了 ufw disable")
		case fw.ActFirewalld:
			parts = append(parts, "停用了 firewalld(systemctl disable --now firewalld)")
		case fw.ActIPTables:
			parts = append(parts, "在 iptables 的 INPUT 最前面加了一条全放行(原有规则一条没动)")
		case fw.ActIP6Tables:
			parts = append(parts, "在 ip6tables 的 INPUT 最前面加了一条全放行(原有规则一条没动)")
		}
	}
	return strings.Join(parts, ";")
}

func runFirewall(args []string) {
	fs := flag.NewFlagSet("firewall", flag.ExitOnError)
	dbPath := fs.String("db", menuDBPath(), "m-ui 数据库路径")
	if len(args) == 0 {
		fwUsage()
		os.Exit(2)
	}
	op := args[0]
	fs.Parse(args[1:])
	if op == "help" || op == "-h" || op == "--help" {
		fwUsage()
		return
	}
	if op == "detect" { // 只看不改,不要求库已经在
		if st := fw.Detect(fw.Exec{}); st.Blocking {
			fmt.Println(st.Frontend)
		}
		return
	}
	if err := requireRootFor("m-ui firewall " + op); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := requireDB(*dbPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := fwCommand(*dbPath, op); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func requireRootFor(cmd string) error {
	if err := requireRoot(); err != nil {
		return fmt.Errorf("需要以 root 运行(sudo %s)", cmd)
	}
	return nil
}

func fwCommand(dbPath, op string) error {
	switch op {
	case "status":
		return fwStatus(dbPath)
	case "open":
		o, did, err := runner.FwLocalOpen(dbPath)
		if err != nil {
			return err
		}
		if !did {
			if len(o.Actions) > 0 {
				fmt.Println("系统防火墙之前已经放开过了(" + frontName(o.Frontend) + ")。要恢复:m-ui firewall undo")
			} else {
				fmt.Println("没有在拦入站的系统防火墙,不用放开。")
			}
			return nil
		}
		fmt.Println("已放开系统防火墙(" + frontName(o.Frontend) + "):" + openedHow(o) + "。")
		if o.Backup != "" {
			fmt.Println("改之前的规则备份在 " + o.Backup)
		}
		fmt.Println("要恢复:m-ui firewall undo(或在面板「防火墙」页点这台的「撤销放开」)。")
		fmt.Println("云厂商控制台里的安全组 / 防火墙不受影响,要挡端口在那里设;面板「防火墙」页的规则照常生效。")
	case "undo":
		o, did, err := runner.FwLocalUndo(dbPath)
		if err != nil {
			return err
		}
		if !did {
			fmt.Println("这台没有放开过系统防火墙,没有要撤销的。")
			return nil
		}
		fmt.Println("已撤销:系统防火墙(" + frontName(o.Frontend) + ")回到放开之前的样子。")
		fmt.Println("m-ui 用到的端口会自动在它那里放行(面板「防火墙」页的开关,默认开着);运行中的 m-ui 30 秒内完成。")
	case "pause":
		if err := runner.FwLocalPause(dbPath, true); err != nil {
			return err
		}
		fmt.Println("这台已暂停面板规则:拉黑、端口只给指定来源都不再生效(放行端口不受影响)。")
		fmt.Println("到面板「防火墙」页把有问题的规则改掉或删掉,再执行 m-ui firewall resume 恢复。")
	case "resume":
		if err := runner.FwLocalPause(dbPath, false); err != nil {
			return err
		}
		fmt.Println("已恢复:面板规则 30 秒内重新生效(m-ui 没在运行的话,启动后生效)。")
	default:
		fwUsage()
		return fmt.Errorf("不认识的操作 %q", op)
	}
	return nil
}

func fwStatus(dbPath string) error {
	l, err := runner.FwLocalStatus(dbPath)
	if err != nil {
		return err
	}
	st := l.Status
	switch {
	case l.Opened:
		line := "系统防火墙:已放开"
		if l.Record != nil {
			line += "(原来是 " + frontName(l.Record.Frontend) + "," + time.Unix(l.Record.At, 0).Format("2006-01-02 15:04") + " 放开"
			if l.Record.Backup != "" {
				line += ",原规则备份在 " + l.Record.Backup
			}
			line += ")"
		}
		fmt.Println(line + "。恢复:m-ui firewall undo")
	case st.Blocking:
		fmt.Println("系统防火墙:" + frontName(st.Frontend) + ",正在拦没放行的入站(" + st.Detail + ")。放开:m-ui firewall open")
	default:
		fmt.Println("系统防火墙:没有在拦入站的。")
	}
	switch {
	case l.Paused:
		fmt.Printf("面板规则:这台已暂停(库里启用着 %d 条)。恢复:m-ui firewall resume\n", l.Rules)
	case st.Engine == fw.EngineNone && l.Rules > 0:
		fmt.Printf("面板规则:启用着 %d 条,但这台没有 nft 也没有 iptables,落不下去。\n", l.Rules)
	default:
		where := map[string]string{fw.EngineNFT: "nft 表 inet m_ui", fw.EngineIPTables: "iptables 链 M-UI-IN"}[st.Engine]
		if where == "" {
			where = "这台没有 nft / iptables"
		}
		fmt.Printf("面板规则:启用着 %d 条(%s)。把自己关在门外了:m-ui firewall pause\n", l.Rules, where)
	}
	fmt.Printf("黑名单:%d 条(面板 / 订阅 / API 的访问)。移出:m-ui unban <IP>,清空:m-ui unban --all\n", l.Bans)
	return nil
}

func runUnban(args []string) {
	fs := flag.NewFlagSet("unban", flag.ExitOnError)
	dbPath := fs.String("db", menuDBPath(), "m-ui 数据库路径")
	all := fs.Bool("all", false, "清空黑名单")
	// 选项可以写在 IP 前后:m-ui unban 1.2.3.4 -db /x/m-ui.db
	var rest []string
	for left := args; ; {
		fs.Parse(left)
		left = fs.Args()
		if len(left) == 0 {
			break
		}
		rest = append(rest, left[0])
		left = left[1:]
	}
	if (!*all && len(rest) != 1) || (*all && len(rest) != 0) {
		fmt.Fprintln(os.Stderr, "用法:m-ui unban <IP|网段>   或   m-ui unban --all")
		os.Exit(2)
	}
	if err := requireRootFor("m-ui unban …"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := requireDB(*dbPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ip := ""
	if !*all {
		ip = rest[0]
	}
	if err := unbanCommand(*dbPath, ip, *all); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func unbanCommand(dbPath, ip string, all bool) error {
	n, err := runner.FwLocalUnban(dbPath, ip, all)
	if err != nil {
		return err
	}
	switch {
	case all:
		fmt.Printf("已清空黑名单(%d 条)。运行中的 m-ui 30 秒内生效。\n", n)
	case n == 0:
		fmt.Println(ip + " 不在黑名单里。(m-ui firewall status 看还有几条;面板「防火墙」页能看到全部)")
	default:
		fmt.Println("已把 " + ip + " 移出黑名单。运行中的 m-ui 30 秒内生效。")
	}
	return nil
}

// fwMenu 管理菜单里的「防火墙」。
func fwMenu(dbPath string, ask func(string) string) {
	if err := requireRoot(); err != nil {
		fmt.Println(colorRed+"  ", err, colorReset)
		return
	}
	if err := requireDB(dbPath); err != nil {
		fmt.Println(colorRed+"  ", err, colorReset)
		return
	}
	fmt.Println()
	if err := fwStatus(dbPath); err != nil {
		fmt.Println(colorRed+"  ", err, colorReset)
		return
	}
	// 最常见的来意:自己的 IP 被拉黑了、面板打不开。认得出这次 SSH 是从哪连进来的,就直接说它在不在名单里
	me := sshClientIP()
	if me != "" {
		if _, banned, err := runner.FwLocalPeek(dbPath, me); err == nil && banned {
			fmt.Println(colorRed + "你现在的 IP " + me + " 在黑名单里:选 5、直接回车就能移出。" + colorReset)
		} else if err == nil {
			fmt.Println(colorGray + "你现在的 IP " + me + " 不在黑名单里。" + colorReset)
		}
	}
	fmt.Println()
	fmt.Println("   1. 放开系统防火墙(云控制台放行了、外面还是连不上时;可撤销)")
	fmt.Println("   2. 撤销放开")
	fmt.Println("   3. 暂停这台的面板规则(一条规则把自己关在门外时)")
	fmt.Println("   4. 恢复面板规则")
	fmt.Println("   5. 把一个 IP 移出黑名单")
	fmt.Println("   6. 清空黑名单")
	fmt.Println("   0. 返回")
	var err error
	switch ask(colorBold + "  请输入数字: " + colorReset) {
	case "1":
		err = fwCommand(dbPath, "open")
	case "2":
		err = fwCommand(dbPath, "undo")
	case "3":
		err = fwCommand(dbPath, "pause")
	case "4":
		err = fwCommand(dbPath, "resume")
	case "5":
		prompt := "  要移出的 IP 或网段: "
		if me != "" {
			prompt = "  要移出的 IP 或网段(回车 = 你现在的 IP " + me + "): "
		}
		ip := ask(prompt)
		if ip == "" {
			ip = me
		}
		if ip != "" {
			err = unbanCommand(dbPath, ip, false)
		}
	case "6":
		if ask("  清空整个黑名单(自动拉黑的和手动加的)? [y/N]: ") == "y" {
			err = unbanCommand(dbPath, "", true)
		}
	}
	if err != nil {
		fmt.Println(colorRed+"  ", err, colorReset)
	}
}
