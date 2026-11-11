package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
//	m-ui ha status                 本机的角色、主机容灾的状态、备用、本机的签名 / 备用公钥指纹
//	m-ui ha takeover [--force]     本机(备用)接任为主机;--force:主机看起来没失联、或没开主机容灾也接任;
//	                               在主机上加 --force:以高于已知全部纪元的新纪元把副机收回来
//	m-ui ha set-master <服务器> [--allow-plain]  在主机上把一台副机(名字或 id)设为主机;明文 http 链接要加 --allow-plain
//	m-ui ha pause <分钟>            在主机上暂停接任(升级、维护前)
//	m-ui ha resume                 恢复接任
//	m-ui ha reset [--force]        清掉本机的主机容灾状态(把主机降回旧版本前在每台副机上执行;主机上要 --force)
func runHA(args []string) {
	fs := flag.NewFlagSet("ha", flag.ExitOnError)
	dbPath := fs.String("db", menuDBPath(), "m-ui 数据库路径")
	force := fs.Bool("force", false, "takeover:不管主机最近有没有联系、有没有开主机容灾;reset:在主机上也重置")
	allowPlain := fs.Bool("allow-plain", false, "set-master:目标是明文 http 链接也继续(链接上的主动中间人能拿到并改写整库)")
	yes := fs.Bool("y", false, "不再问确认")
	if len(args) == 0 {
		haUsage()
		os.Exit(2)
	}
	op := args[0]
	// 选项可以写在服务器名前后:m-ui ha set-master tw --allow-plain
	var rest []string
	for left := args[1:]; ; {
		fs.Parse(left)
		left = fs.Args()
		if len(left) == 0 {
			break
		}
		rest = append(rest, left[0])
		left = left[1:]
	}
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
	if err := haCommand(db, op, rest, haFlags{force: *force, yes: *yes, allowPlain: *allowPlain, dataDir: filepath.Dir(*dbPath)}, ask); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func haUsage() {
	fmt.Println("用法:")
	fmt.Println("  m-ui ha status                 查看本机角色、主机容灾状态、备用、公钥指纹")
	fmt.Println("  m-ui ha takeover [--force]     本机(备用)接任为主机;在主机上加 --force:以更高的纪元把副机收回来")
	fmt.Println("  m-ui ha set-master <服务器> [--allow-plain]")
	fmt.Println("                                 在主机上把一台副机设为主机(名字或 id);它的链接是明文 http 时要加 --allow-plain")
	fmt.Println("  m-ui ha pause <分钟>            在主机上暂停接任")
	fmt.Println("  m-ui ha resume                 恢复接任")
	fmt.Println("  m-ui ha reset [--force]        清掉本机的主机容灾状态(主机降回旧版本前在每台副机上执行;主机上要 --force)")
}

// requireRoot 主机容灾的命令要 root(库是 0600,操作会让服务重启、换主机)。
func requireRoot() error {
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		return fmt.Errorf("需要以 root 运行(sudo m-ui ha …)")
	}
	return nil
}

// haFlags 命令行选项。
type haFlags struct {
	force, yes, allowPlain bool
	dataDir                string
}

func haCommand(db *gorm.DB, op string, args []string, f haFlags, ask func(string) string) error {
	force, yes := f.force, f.yes
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
		if f.allowPlain && !yes {
			fmt.Println(colorRed + "  --allow-plain:到目标的链接是明文 http,链接上的主动中间人能拿到并改写整库数据。只在你确信链路可信时继续。" + colorReset)
		}
		if !yes && ask("  把主机的全部数据移到「"+args[0]+"」、它成为主机,本机变为副机继续服务;两台各重启一次。继续?[y/N] ") != "y" {
			return fmt.Errorf("已取消")
		}
		if err := haSubmit(db, ha.Cmd{Op: "set-master", Target: args[0], AllowPlain: f.allowPlain}, 60*time.Second); err != nil {
			return err
		}
		return haWaitSwitch(db)
	case "reset":
		return haReset(db, f, ask)
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

// haWaitSwitch 设为主机:等它做完(本机最后会变为副机并重启),打印新面板地址;没完成、结果不明都把原因与冻结截止打出来。
func haWaitSwitch(db *gorm.DB) error {
	deadline := time.Now().Add(70 * time.Minute) // 冻结最长 60 分钟,再留出结果不明时的隔离
	last, lastErr := "", ""
	for time.Now().Before(deadline) {
		var raw string
		db.Raw("SELECT value FROM settings WHERE key = ?", ha.KeySwitch).Scan(&raw)
		var st ha.SwitchState
		if json.Unmarshal([]byte(raw), &st) == nil {
			if st.Stage != last {
				fmt.Println("  …", switchStageText(st.Stage))
				last = st.Stage
				if st.FreezeUntil > 0 && st.Stage != ha.StageDone && st.Stage != ha.StageFailed && st.Stage != ha.StageFenced {
					fmt.Println("    写操作暂停到 " + time.Unix(st.FreezeUntil, 0).Format("2006-01-02 15:04:05") + "(通常一两分钟就完成)")
				}
			}
			if st.Err != "" && st.Err != lastErr && st.Stage != ha.StageFailed {
				fmt.Println(colorRed + "    " + st.Err + colorReset)
				lastErr = st.Err
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
			case ha.StageFenced:
				return fmt.Errorf("结果不明,已以纪元 %d 隔离目标、本机继续当主机:%s", st.FenceEpoch, or(st.Err, "目标之后起来会被副机拒绝并自动变为副机"))
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("等了 70 分钟还没完成,到面板上看进度")
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
	case ha.StageUnknown:
		return "目标有没有接手还不确定:保持冻结,继续问目标(到期仍不明会隔离目标)"
	case ha.StageFenced:
		return "结果不明,已隔离目标"
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
	modes := map[string]string{"": "关闭", ha.ModeOff: "关闭", ha.ModeConfirm: "先确认再接任", ha.ModeAuto: "自动接任"}
	fmt.Printf("  本机:%s(%s),纪元 %d\n", or(v.SelfName, "-"), roles[v.Role], v.Epoch)
	if v.SignFP != "" {
		fmt.Printf("  本机签名公钥指纹:%s\n", v.SignFP)
	}
	if v.Node != nil && v.Node.PubFP != "" {
		fmt.Printf("  本机备用公钥指纹:%s(主机的服务器列表里这台显示的应与它一致)\n", v.Node.PubFP)
	}
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
		} else if n.Lost && n.Why != "" {
			fmt.Println("  现在不能接任:" + cantTakeoverText(n.Why))
		}
	}
	if v.Role == "master" {
		names := map[uint]string{}
		for _, s := range v.Standbys {
			names[s.Id] = s.Name
		}
		fps := map[uint]string{}
		for _, s := range v.Standbys {
			fps[s.Id] = s.PinnedFP
		}
		for i, id := range v.StandbyIds {
			line := fmt.Sprintf("  备用 %d:%s", i+1, or(names[id], "#"+strconv.FormatUint(uint64(id), 10)))
			if fps[id] != "" {
				line += "(公钥指纹 " + fps[id] + ")"
			}
			fmt.Println(line)
		}
		for _, d := range v.DropPend {
			fmt.Printf("  %s 已停用 / 删除,还没确认删掉手里的副本(重试 %d 次):%s\n", d.Name, d.Tries, or(d.Err, "等它上线"))
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

// cantTakeoverText 不能接任的原因(命令行用中文直说)。
func cantTakeoverText(why string) string {
	m := map[string]string{
		"master": "本机已经是主机", "promoting": "正在接任", "off": "主机容灾没有开启", "notStandby": "本机不是备用",
		"noReplica": "本机还没有数据副本", "masterOnline": "主机还在联系本机", "paused": "主机暂停了接任",
		"noPrompt": "还没到确认时间,或证据不足(同伴还联系得上主机等)", "auto": "自动接任模式由系统判定;确需人工接任用 --force",
		"oldReplica":     "手里的副本比本机纪元旧:用 --force 接任会丢上一任新主机期间的改动",
		"peerSeesMaster": "别的副机还联系得上主机", "witnessUp": "境外测点还能连上主机", "betterStandby": "排在前面的备用能接任",
		"higherEpoch": "已经有纪元更高的主机",
	}
	if t, ok := m[why]; ok {
		return t
	}
	return why
}

// haReset m-ui ha reset:清掉本机的容灾状态。服务在跑就交给它(做完重启),没在跑直接改库。
func haReset(db *gorm.DB, f haFlags, ask func(string) string) error {
	var mode, epoch string
	db.Raw("SELECT value FROM settings WHERE key = ?", "nodeMode").Scan(&mode)
	db.Raw("SELECT value FROM settings WHERE key = ?", ha.KeyEpoch).Scan(&epoch)
	master := !strings.EqualFold(mode, "true")
	if master && epoch != "" && epoch != "0" && !f.force {
		return fmt.Errorf("本机是主机:重置后副机不再认本机(会回 409)。确实要这样做(比如要把主机降回旧版本、之后每台副机也会重置),加 --force")
	}
	if !f.yes {
		fmt.Println("  清掉本机的主机容灾状态:纪元、跟随的主机、手里的数据副本、备用密钥。")
		if master {
			fmt.Println(colorRed + "  本机是主机:之后还认本机纪元的副机都要在它们上面也执行 m-ui ha reset。" + colorReset)
		} else {
			fmt.Println("  本机是副机:主机下一次推容灾状态时会重新接上(要再当备用,主机那边对本机「重新信任」)。")
		}
		if ask("  继续?[y/N] ") != "y" {
			return fmt.Errorf("已取消")
		}
	}
	if r, err := ha.SubmitCmd(db, ha.Cmd{Op: "reset", Force: f.force}, 30*time.Second); err == nil {
		if !r.OK {
			return fmt.Errorf("没做成:%s", r.Err)
		}
		fmt.Println(colorGreen + "  " + r.Text + colorReset)
		return nil
	}
	if err := ha.ResetLocal(db, f.dataDir); err != nil {
		return err
	}
	fmt.Println(colorGreen + "  已清掉本机的主机容灾状态(m-ui 服务没在跑:启动后生效)" + colorReset)
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
		err2 = haCommand(db, "takeover", nil, haFlags{dataDir: filepath.Dir(dbPath)}, ask)
	case "2":
		name := ask("  要设为主机的服务器(名字或 id): ")
		err2 = haCommand(db, "set-master", []string{name}, haFlags{dataDir: filepath.Dir(dbPath)}, ask)
	case "3":
		err2 = haCommand(db, "pause", []string{or(ask("  暂停多少分钟 [60]: "), "60")}, haFlags{yes: true, dataDir: filepath.Dir(dbPath)}, ask)
	case "4":
		err2 = haCommand(db, "resume", nil, haFlags{yes: true, dataDir: filepath.Dir(dbPath)}, ask)
	}
	if err2 != nil {
		fmt.Println(colorRed+"  ", err2, colorReset)
	}
}
