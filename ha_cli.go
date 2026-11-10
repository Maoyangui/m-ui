package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/ha"

	"gorm.io/gorm"
)

// m-ui ha …:主机容灾与设为主机的命令行(要 root)。操作交给运行中的服务去做(经数据库传话),这里只等结果、打印出来。
//
//	m-ui ha status                 本机的角色、主机容灾的状态、备用
//	m-ui ha takeover [--force]     本机(备用)接任为主机;--force:主机看起来没失联、或没开主机容灾也接任
//	m-ui ha set-master <服务器>     在主机上把一台副机(名字或 id)设为主机
//	m-ui ha pause <分钟>            在主机上暂停接任(升级、维护前)
//	m-ui ha resume                 恢复接任
func runHA(args []string) {
	fs := flag.NewFlagSet("ha", flag.ExitOnError)
	dbPath := fs.String("db", menuDBPath(), "m-ui 数据库路径")
	force := fs.Bool("force", false, "takeover:不管主机最近有没有联系、有没有开主机容灾")
	yes := fs.Bool("y", false, "不再问确认")
	if len(args) == 0 {
		haUsage()
		os.Exit(2)
	}
	op := args[0]
	fs.Parse(args[1:])
	if err := requireRoot(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := requireDB(*dbPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	db, err := database.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开数据库失败:", err)
		os.Exit(1)
	}
	defer database.Close(db)
	ask := func(prompt string) string {
		fmt.Print(prompt)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimSpace(line)
	}
	if err := haCommand(db, op, fs.Args(), *force, *yes, ask); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func haUsage() {
	fmt.Println("用法:")
	fmt.Println("  m-ui ha status                 查看本机角色、主机容灾状态、备用")
	fmt.Println("  m-ui ha takeover [--force]     本机(备用)接任为主机")
	fmt.Println("  m-ui ha set-master <服务器>     在主机上把一台副机设为主机(名字或 id)")
	fmt.Println("  m-ui ha pause <分钟>            在主机上暂停接任")
	fmt.Println("  m-ui ha resume                 恢复接任")
}

// requireRoot 主机容灾的命令要 root(库是 0600,操作会让服务重启、换主机)。
func requireRoot() error {
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		return fmt.Errorf("需要以 root 运行(sudo m-ui ha …)")
	}
	return nil
}

func haCommand(db *gorm.DB, op string, args []string, force, yes bool, ask func(string) string) error {
	switch op {
	case "status":
		return haStatus(db)
	case "takeover":
		if force && !yes {
			fmt.Println(colorRed + "  --force:不管主机最近有没有联系本机都接任。主机其实还活着时,会出现两台各管一部分副机,要人来收拾。" + colorReset)
			if ask("  确定要接任,输入 takeover: ") != "takeover" {
				return fmt.Errorf("已取消")
			}
		} else if !yes && ask("  本机接任为主机(用手里的数据副本),马上重启。继续?[y/N] ") != "y" {
			return fmt.Errorf("已取消")
		}
		return haSubmit(db, ha.Cmd{Op: "takeover", Force: force}, 60*time.Second)
	case "set-master":
		if len(args) != 1 {
			return fmt.Errorf("用法:m-ui ha set-master <服务器名或 id>")
		}
		if !yes && ask("  把主机的全部数据移到「"+args[0]+"」、它成为主机,本机变为副机继续服务;两台各重启一次。继续?[y/N] ") != "y" {
			return fmt.Errorf("已取消")
		}
		if err := haSubmit(db, ha.Cmd{Op: "set-master", Target: args[0]}, 60*time.Second); err != nil {
			return err
		}
		return haWaitSwitch(db)
	case "pause":
		if len(args) != 1 {
			return fmt.Errorf("用法:m-ui ha pause <分钟>")
		}
		n, err := strconv.Atoi(args[0])
		if err != nil || n <= 0 {
			return fmt.Errorf("分钟数要是正整数")
		}
		return haSubmit(db, ha.Cmd{Op: "pause", Min: n}, 30*time.Second)
	case "resume":
		return haSubmit(db, ha.Cmd{Op: "resume"}, 30*time.Second)
	}
	haUsage()
	return fmt.Errorf("不认识的子命令 %q", op)
}

func haSubmit(db *gorm.DB, c ha.Cmd, wait time.Duration) error {
	r, err := ha.SubmitCmd(db, c, wait)
	if err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("没做成:%s", r.Err)
	}
	if r.Text != "" {
		fmt.Println(colorGreen + "  " + r.Text + colorReset)
	}
	return nil
}

// haWaitSwitch 设为主机:等它做完(本机最后会变为副机并重启),打印新面板地址。
func haWaitSwitch(db *gorm.DB) error {
	deadline := time.Now().Add(20 * time.Minute)
	last := ""
	for time.Now().Before(deadline) {
		var raw string
		db.Raw("SELECT value FROM settings WHERE key = ?", ha.KeySwitch).Scan(&raw)
		var st ha.SwitchState
		if json.Unmarshal([]byte(raw), &st) == nil {
			if st.Stage != last {
				fmt.Println("  …", switchStageText(st.Stage))
				last = st.Stage
			}
			switch st.Stage {
			case ha.StageDone:
				fmt.Println(colorGreen + "  完成。新面板地址:" + st.NewPanel + "(账号、密码、两步验证不变,需要重新登录)" + colorReset)
				if st.SubKept && !st.DnsAuto {
					fmt.Println("  还剩一件事:把 " + st.SubHost + " 的 DNS 改指向新主机的 IP " + st.TargetIP + ",用户的订阅地址就不变。")
				}
				return nil
			case ha.StageFailed:
				return fmt.Errorf("没有完成:%s(本机照旧是主机)", st.Err)
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("等了 20 分钟还没完成,到面板上看进度")
}

func switchStageText(stage string) string {
	switch stage {
	case ha.StagePrepare:
		return "准备目标机器"
	case ha.StageExport:
		return "打包并加密数据"
	case ha.StageSend:
		return "传到目标机器"
	case ha.StageGrant:
		return "通知其它副机"
	case ha.StageCommit:
		return "目标机器接手"
	case ha.StageDone:
		return "本机变为副机"
	case ha.StageFailed:
		return "失败"
	}
	return stage
}

// haStatus 打印状态:服务在跑就问它(有租约、证据等运行时信息),没在跑就读库。
func haStatus(db *gorm.DB) error {
	var v ha.View
	if r, err := ha.SubmitCmd(db, ha.Cmd{Op: "status"}, 8*time.Second); err == nil && r.OK {
		_ = json.Unmarshal(r.Data, &v)
	} else {
		fmt.Println(colorGray + "  (m-ui 服务没有回应,下面是库里记的)" + colorReset)
		get := func(k string) string {
			var s string
			db.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&s)
			return s
		}
		v.Role = "master"
		if strings.EqualFold(get("nodeMode"), "true") {
			v.Role = "node"
		}
		v.Epoch, _ = strconv.ParseUint(get(ha.KeyEpoch), 10, 64)
		v.Mode = get(ha.KeyMode)
	}
	roles := map[string]string{"master": "主机", "node": "副机", "demoted": "副机(原主机,等新主机来接)", "promoting": "正在接任为主机"}
	modes := map[string]string{"": "关闭", ha.ModeOff: "关闭", ha.ModeConfirm: "主机失联后确认再接任", ha.ModeAuto: "主机失联后自动接任"}
	fmt.Printf("  本机:%s(%s),纪元 %d\n", or(v.SelfName, "-"), roles[v.Role], v.Epoch)
	fmt.Printf("  主机容灾:%s\n", modes[v.Mode])
	if v.MasterName != "" && v.Role != "master" {
		fmt.Printf("  主机:%s\n", v.MasterName)
	}
	if n := v.Node; n != nil {
		if n.Rank > 0 {
			fmt.Printf("  本机是备用 %d", n.Rank)
			if n.Replica != nil {
				fmt.Printf(",数据副本截至 %s", time.Unix(n.Replica.AsOf, 0).Format("2006-01-02 15:04:05"))
			}
			fmt.Println()
		}
		if n.Lost {
			fmt.Printf(colorRed+"  主机已 %d 秒没联系本机"+colorReset+"\n", n.LeaseAgo)
		}
		if n.CanTakeover {
			fmt.Println("  可以接任:m-ui ha takeover")
		}
	}
	if v.Role == "master" {
		names := map[uint]string{}
		for _, s := range v.Standbys {
			names[s.Id] = s.Name
		}
		for i, id := range v.StandbyIds {
			fmt.Printf("  备用 %d:%s\n", i+1, or(names[id], "#"+strconv.FormatUint(uint64(id), 10)))
		}
		if v.Config != nil && v.Config.PauseUntil > time.Now().Unix() {
			fmt.Printf("  接任暂停到 %s\n", time.Unix(v.Config.PauseUntil, 0).Format("2006-01-02 15:04:05"))
		}
		if v.Former != nil {
			fmt.Printf("  原主机 %s 失联中,回来后自动变为副机\n", v.Former.Name)
		}
	}
	return nil
}

// haMenu 管理菜单里的「主机容灾 / 设为主机」。
func haMenu(dbPath string, ask func(string) string) {
	if err := requireRoot(); err != nil {
		fmt.Println(colorRed+"  ", err, colorReset)
		return
	}
	db, err := database.Open(dbPath)
	if err != nil {
		fmt.Println(colorRed+"  打开数据库失败:", err, colorReset)
		return
	}
	defer database.Close(db)
	_ = haStatus(db)
	fmt.Println()
	fmt.Println("   1. 接任为主机(本机是备用、主机失联时)")
	fmt.Println("   2. 设为主机(在主机上:把一台副机设为主机)")
	fmt.Println("   3. 暂停接任(升级、维护前)")
	fmt.Println("   4. 恢复接任")
	fmt.Println("   0. 返回")
	var err2 error
	switch ask(colorBold + "  请输入数字: " + colorReset) {
	case "1":
		err2 = haCommand(db, "takeover", nil, false, false, ask)
	case "2":
		name := ask("  要设为主机的服务器(名字或 id): ")
		err2 = haCommand(db, "set-master", []string{name}, false, false, ask)
	case "3":
		err2 = haCommand(db, "pause", []string{or(ask("  暂停多少分钟 [60]: "), "60")}, false, true, ask)
	case "4":
		err2 = haCommand(db, "resume", nil, false, true, ask)
	}
	if err2 != nil {
		fmt.Println(colorRed+"  ", err2, colorReset)
	}
}
