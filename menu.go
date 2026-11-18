package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/acme"
	"github.com/Maoyangui/m-ui/backup"
	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/runner"
	"github.com/Maoyangui/m-ui/selfupdate"
)

const (
	unitPath    = "/etc/systemd/system/m-ui.service"
	binPath     = "/usr/local/bin/m-ui"
	repoAPI     = "https://api.github.com/repos/" + brand.RepoPath + "/releases/latest"
	defaultDB   = "/etc/m-ui/m-ui.db"
	colorReset  = "\033[0m"
	colorBold   = "\033[1m"
	colorGreen  = "\033[32m"
	colorRed    = "\033[31m"
	colorCyan   = "\033[36m"
	colorYellow = "\033[33m"
	colorGray   = "\033[90m"
)

var reExecDB = regexp.MustCompile(`-db\s+(\S+)`)

// menuDBPath 从 systemd 单元里找数据库路径;找不到用默认。
func menuDBPath() string {
	if p := os.Getenv("M_UI_DB"); p != "" {
		return p
	}
	if b, err := os.ReadFile(unitPath); err == nil {
		if m := reExecDB.FindStringSubmatch(string(b)); m != nil {
			return m[1]
		}
	}
	if runtime.GOOS != "linux" {
		return "m-ui.db"
	}
	return defaultDB
}

func serviceState() string {
	if runtime.GOOS != "linux" {
		return "-"
	}
	out, _ := exec.Command("systemctl", "is-active", "m-ui").Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "unknown"
	}
	return s
}

func systemctl(args ...string) {
	if runtime.GOOS != "linux" {
		fmt.Println(colorGray + "(非 Linux,跳过 systemctl " + strings.Join(args, " ") + ")" + colorReset)
		return
	}
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Println(colorRed+"systemctl 失败:", err, colorReset)
	}
}

// settingsOf 读取设置(离线打开数据库)。
func settingsOf(dbPath string) (map[string]string, error) {
	db, err := database.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer database.Close(db)
	type kv struct{ Key, Value string }
	var rows []kv
	db.Raw("SELECT key, value FROM settings").Scan(&rows)
	m := map[string]string{}
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	return m, nil
}

// settingsRO 只读取设置:升级守护和健康检查地址用,不建库、不迁移。
func settingsRO(dbPath string) (map[string]string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	db, err := database.OpenReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	if sq, err := db.DB(); err == nil {
		defer sq.Close()
	}
	type kv struct{ Key, Value string }
	var rows []kv
	if err := db.Raw("SELECT key, value FROM settings").Scan(&rows).Error; err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	return m, nil
}

// PanelInfo 组装面板地址信息(安装脚本与菜单共用)。
func panelInfo(dbPath string) (url string, user string, defaultPw bool, err error) {
	s, err := settingsOf(dbPath)
	if err != nil {
		return "", "", false, err
	}
	host := infoHost(s)
	scheme := "http"
	if s["webCertFile"] != "" && s["webKeyFile"] != "" {
		scheme = "https"
	}
	port := s["webPort"]
	if port == "" {
		port = "2053"
	}
	path := s["webPath"]
	if path == "" {
		path = "/app/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	portNum, _ := strconv.Atoi(port)
	return fmt.Sprintf("%s://%s%s", scheme, model.URLHost(scheme, host, portNum), path), adminName(dbPath), s["adminDefault"] == "true", nil
}

// adminName 库里现有管理员的用户名(可以在面板里改,不一定叫 admin);读不到按 admin。
func adminName(dbPath string) string {
	if _, err := os.Stat(dbPath); err != nil {
		return "admin"
	}
	db, err := database.OpenReadOnly(dbPath)
	if err != nil {
		return "admin"
	}
	if sq, e := db.DB(); e == nil {
		defer sq.Close()
	}
	var name string
	db.Model(&model.Admin{}).Order("id asc").Limit(1).Pluck("username", &name)
	if strings.TrimSpace(name) == "" {
		return "admin"
	}
	return name
}

// infoHost 面板地址里的主机名:面板域名 → 公网 IP(不用订阅域名:订阅可能单独一个域名,换过主机后还是随主机身份迁来的)。
func infoHost(s map[string]string) string {
	for _, k := range []string{"webDomain", "publicIp"} {
		if v := strings.TrimSpace(s[k]); v != "" {
			return v
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if ip := acme.PublicIP(ctx); ip != "" {
		return ip
	}
	return "<服务器IP>"
}

func printPanelInfo(dbPath string) {
	url, user, def, err := panelInfo(dbPath)
	if err != nil {
		fmt.Println(colorRed+"读取数据库失败:", err, colorReset)
		return
	}
	fmt.Println()
	fmt.Println(colorBold + "  面板地址: " + colorCyan + url + colorReset)
	fmt.Println(colorBold + "  用户名:   " + colorReset + user)
	if def {
		fmt.Println(colorBold + "  密码:     " + colorReset + "admin  " + colorRed + "(默认密码,请登录后立即修改)" + colorReset)
	} else {
		fmt.Println(colorBold + "  密码:     " + colorReset + colorGray + "(已修改;忘了就用菜单里的「重置管理员密码」)" + colorReset)
	}
	s, _ := settingsOf(dbPath)
	role := "主机"
	if strings.EqualFold(s["nodeMode"], "true") {
		role = "副机"
	}
	subPort := s["subPort"]
	if subPort == "" {
		subPort = "2056"
	}
	fmt.Println(colorBold+"  角色:     "+colorReset+role, "   订阅端口:", subPort, "   数据库:", dbPath)
	if !strings.EqualFold(s["resellerEnabled"], "false") && !strings.EqualFold(s["nodeMode"], "true") {
		rp, rpath := s["resellerPort"], s["resellerPath"]
		if rp == "" {
			rp = "2054"
		}
		if rpath == "" {
			rpath = "/dl/"
		}
		scheme := "http"
		if s["webCertFile"] != "" && s["webKeyFile"] != "" {
			scheme = "https"
		}
		fmt.Println(colorBold + "  代理面板: " + colorReset + colorGray +
			fmt.Sprintf("%s://%s%s", scheme, urlHostStr(scheme, infoHost(s), rp), rpath) + colorReset)
	}
	fmt.Println()
}

// menuRow 主菜单的一行:左边两个字的分组名(同组的后几行留空),右边是这一行的各项。
type menuRow struct{ Group, Text string }

// menuRows 主菜单。按「来这里是要干什么」分组,整张不超过 20 行:24 行的终端里一屏放得下,不用往回翻。
// 这里的编号改了,帮助页和 README 不用跟着改 —— 那里写的是项目的名字和对应的命令(m-ui doctor / logs / restart …)。
var menuRows = []menuRow{
	{"排查", " 1. 面板地址 / 账号"},
	{"", " 2. 一键体检(面板打不开、节点连不上,先跑这个)"},
	{"", " 3. 日志"},
	{"服务", " 4. 重启       5. 停止       6. 启动       7. 更新到最新版"},
	{"登录", " 8. 重置管理员密码(同时关掉两步验证)"},
	{"", " 9. 只关闭两步验证(手机丢了)"},
	{"", "10. 防火墙与黑名单(把自己关在外面了 / 解封 IP)"},
	{"设置", "11. 修改端口 / 路径 / 监听地址"},
	{"", "12. 生成自签证书(没有域名时)"},
	{"", "13. 重置全部设置(线路、用户、上游、服务器保留)"},
	{"数据", "14. 立即备份          15. 还原备份"},
	{"多台", "16. 主机 / 副机(切换角色、副机接入信息)"},
	{"", "17. 主机容灾 / 设为主机"},
	{"其它", "18. 卸载 m-ui         0. 退出"},
}

// statusLine m-ui status:一行说清这台现在的样子(脚本、巡检用;不带颜色)。
func statusLine(dbPath string) string {
	out := "m-ui v" + version + "  服务:" + serviceState()
	s, err := settingsRO(dbPath)
	if err != nil {
		return out + "  数据库读不出来:" + dbPath
	}
	out += "  角色:" + map[bool]string{false: "主机", true: "副机"}[isTrue(s["nodeMode"])]
	if u := quickPanelURL(s); u != "" {
		out += "  面板:" + u
	}
	return out
}

// quickPanelURL 菜单顶上那一行面板地址:只用库里现成的(面板域名、记下的公网 IP),不为它去联网探测。
func quickPanelURL(s map[string]string) string {
	host := ""
	for _, k := range []string{"webDomain", "publicIp"} {
		if v := strings.TrimSpace(s[k]); v != "" {
			host = v
			break
		}
	}
	if host == "" {
		return ""
	}
	scheme := "http"
	if s["webCertFile"] != "" && s["webKeyFile"] != "" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s%s", scheme, model.URLHost(scheme, host, settingPort(s, "webPort", 2053)), menuNormPath(or(s["webPath"], "/app/")))
}

func printMenu(dbPath string) {
	s, _ := settingsRO(dbPath)
	head := colorBold + colorCyan + "  m-ui 管理菜单  " + colorReset + colorGray + "v" + version + colorReset + "   服务:" + stateColored()
	if s != nil {
		head += "   角色:" + map[bool]string{false: "主机", true: "副机"}[isTrue(s["nodeMode"])]
	}
	fmt.Println()
	fmt.Println(head)
	if u := quickPanelURL(s); u != "" {
		fmt.Println(colorGray + "  面板:" + u + colorReset)
	}
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		fmt.Println(colorYellow + "  现在不是 root:大部分操作做不了,请用 sudo m-ui" + colorReset)
	}
	if err := requireDB(dbPath); err != nil {
		fmt.Println(colorRed + "  没有找到数据库 " + dbPath + ":m-ui 可能还没装好(重新运行安装脚本),或用 M_UI_DB=路径 m-ui 指定" + colorReset)
	}
	fmt.Println(colorGray + "  ──────────────────────────────────────────────────────────────" + colorReset)
	for _, r := range menuRows {
		g := "    "
		if r.Group != "" {
			g = colorCyan + r.Group + colorReset
		}
		fmt.Println("  " + g + "  " + r.Text)
	}
}

// runMenu 交互式管理菜单(m-ui 不带参数时进入)。
// 面板能做的事这里大多也有一份 —— 它是面板打不开、登录不上时的另一条路,所以每一项都得跟面板现在的规则一致。
func runMenu() {
	in := bufio.NewReader(os.Stdin)
	eof := false
	ask := func(prompt string) string {
		fmt.Print(prompt)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			eof = true // 输入关了(管道读完、终端断开):别在菜单里空转
		}
		return strings.TrimSpace(line)
	}
	dbPath := menuDBPath()
	needDB := func() bool {
		if err := requireDB(dbPath); err != nil {
			fmt.Println(colorRed+"  ", err, colorReset)
			return false
		}
		return true
	}
	for !eof {
		printMenu(dbPath)
		choice := ask(colorBold + "  请输入数字: " + colorReset)
		switch strings.ToLower(choice) {
		case "0", "q", "exit", "quit":
			return
		case "": // 手滑多按了一下回车:重画菜单,不退出
			continue
		case "1":
			if needDB() {
				printPanelInfo(dbPath)
			}
		case "2":
			runDoctor(dbPath)
		case "3":
			menuLogs(ask)
		case "4":
			haPauseBefore(dbPath, 5, "重启前")
			systemctl("restart", "m-ui")
			time.Sleep(2 * time.Second)
			fmt.Println("  状态:", stateColored())
		case "5":
			systemctl("stop", "m-ui")
			fmt.Println("  状态:", stateColored())
		case "6":
			systemctl("start", "m-ui")
			time.Sleep(2 * time.Second)
			fmt.Println("  状态:", stateColored())
		case "7":
			if err := selfUpdate(ask); err != nil {
				fmt.Println(colorRed+"  更新失败:", err, colorReset)
			}
		case "8":
			if !needDB() {
				break
			}
			pw := ask("  新密码(回车 = 随机生成): ")
			who, newPw, err := runner.ResetPassword(dbPath, "", pw) // 现有管理员,不管叫什么名字
			if err != nil {
				fmt.Println(colorRed+"  失败:", err, colorReset)
				break
			}
			_ = runner.SetSettings(dbPath, map[string]string{"adminDefault": "false", "totpEnabled": "false", "totpSecret": ""})
			fmt.Println(colorGreen+"  管理员 "+who+" 新密码: "+colorBold+newPw+colorReset, colorGray+"(运行中的面板立即生效;两步验证已一并关闭)"+colorReset)
		case "9":
			if !needDB() {
				break
			}
			if err := runner.SetSettings(dbPath, map[string]string{"totpEnabled": "false", "totpSecret": ""}); err != nil {
				fmt.Println(colorRed+"  失败:", err, colorReset)
				break
			}
			fmt.Println(colorGreen + "  已关闭两步验证,现在只需密码即可登录(运行中的面板立即生效)" + colorReset)
		case "10":
			fwMenu(dbPath, ask)
		case "11":
			if needDB() {
				menuPorts(dbPath, ask)
			}
		case "12":
			if !needDB() {
				break
			}
			hosts := ask("  域名或 IP(逗号分隔,回车 = 自动用公网 IP): ")
			if hosts == "" {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				hosts = acme.PublicIP(ctx)
				cancel()
			}
			if hosts == "" {
				fmt.Println(colorRed + "  无法探测公网 IP,请手动输入" + colorReset)
				break
			}
			dir := filepath.Join(filepath.Dir(dbPath), "cert")
			crt, key := filepath.Join(dir, "selfsigned.crt"), filepath.Join(dir, "selfsigned.key")
			if err := certutil.GenerateSelfSigned(strings.Split(hosts, ","), crt, key, 3650); err != nil {
				fmt.Println(colorRed+"  失败:", err, colorReset)
				break
			}
			_ = runner.SetSettings(dbPath, map[string]string{"certFile": crt, "keyFile": key, "certSource": "selfsign"})
			fmt.Println(colorGreen+"  自签证书已生成,并设为线路用的证书:", crt, colorReset)
			fmt.Println(colorGray + "  订阅会自动带 allow-insecure;重启后生效。面板自己要走 HTTPS,到面板「证书」页给面板选证书。" + colorReset)
			if ask("  现在重启? [Y/n]: ") != "n" {
				haPauseBefore(dbPath, 5, "重启前")
				systemctl("restart", "m-ui")
			}
		case "13":
			if !needDB() {
				break
			}
			if ask(colorRed+"  将清空全部设置(端口/路径/证书/通知/角色等恢复默认;线路、用户、上游、服务器保留)。输入 yes 确认: "+colorReset) != "yes" {
				fmt.Println("  已取消")
				break
			}
			if err := resetSettings(dbPath); err != nil {
				fmt.Println(colorRed+"  失败:", err, colorReset)
				break
			}
			fmt.Println(colorGreen + "  设置已重置(管理员密码保留)" + colorReset)
			systemctl("restart", "m-ui")
			time.Sleep(2 * time.Second)
			printPanelInfo(dbPath)
		case "14":
			if needDB() {
				menuBackup(dbPath)
			}
		case "15":
			menuRestore(dbPath, ask)
		case "16":
			if needDB() {
				menuRole(dbPath, ask)
			}
		case "17":
			haMenu(dbPath, ask)
		case "18":
			if menuUninstall(dbPath, ask) {
				return
			}
		default:
			fmt.Println("  没有这一项:输入菜单里的数字(0 退出)")
		}
		if !eof {
			ask(colorGray + "  按回车回到菜单 " + colorReset)
		}
	}
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func stateColored() string {
	s := serviceState()
	switch s {
	case "active":
		return colorGreen + "运行中" + colorReset
	case "inactive", "failed":
		return colorRed + s + colorReset
	}
	return colorGray + s + colorReset
}

// resetSettings 清空设置表(管理员表、线路、用户等不动)。主机容灾的本机状态(纪元、认的主机、签名密钥……)留着:
// 它们回退了,副机会把本机当成旧主机拒掉,甚至本机把自己降成副机。
func resetSettings(dbPath string) error {
	db, err := database.Open(dbPath)
	if err != nil {
		return err
	}
	defer database.Close(db)
	var keys []string
	if err := db.Raw("SELECT key FROM settings").Scan(&keys).Error; err != nil {
		return err
	}
	for _, k := range keys {
		if strings.HasPrefix(k, "ha") && ha.IsLocal(k) {
			continue
		}
		if err := db.Exec("DELETE FROM settings WHERE key = ?", k).Error; err != nil {
			return err
		}
	}
	return nil
}

// haResetLocal 清掉本机的主机容灾状态(菜单切换角色时用)。
func haResetLocal(dbPath string) error {
	db, err := database.Open(dbPath)
	if err != nil {
		return err
	}
	defer database.Close(db)
	return ha.ResetLocal(db, filepath.Dir(dbPath))
}

// haPauseBefore 升级、重启前暂停接任(本机是开着主机容灾的主机时):服务在跑就交给它(马上推给备用),
// 没在跑或没回应就直接写 haPauseUntil。已经暂停得更久的不缩短;没开容灾、本机是副机时什么都不做。
// 不暂停的话,升级重启那一两分钟备用会判主机失联、发确认接任的提示。
func haPauseBefore(dbPath string, minutes int, why string) {
	s, err := settingsRO(dbPath)
	if err != nil || strings.EqualFold(s["nodeMode"], "true") {
		return
	}
	if e, _ := strconv.ParseUint(strings.TrimSpace(s[ha.KeyEpoch]), 10, 64); e == 0 {
		return
	}
	switch strings.TrimSpace(s[ha.KeyMode]) {
	case ha.ModeConfirm, ha.ModeAuto:
	default:
		return
	}
	until := time.Now().Add(time.Duration(minutes) * time.Minute).Unix()
	if cur, _ := strconv.ParseInt(strings.TrimSpace(s[ha.KeyPauseUntil]), 10, 64); cur >= until {
		return
	}
	if serviceState() == "active" {
		if db, err := database.Open(dbPath); err == nil {
			r, err := ha.SubmitCmd(db, ha.Cmd{Op: "pause", Min: minutes}, 20*time.Second)
			database.Close(db)
			if err == nil && r.OK {
				fmt.Println(colorGray + "  " + why + "已暂停接任 " + strconv.Itoa(minutes) + " 分钟(备用不会误判主机失联)" + colorReset)
				return
			}
		}
	}
	if err := runner.SetSettings(dbPath, map[string]string{ha.KeyPauseUntil: strconv.FormatInt(until, 10)}); err != nil {
		fmt.Println(colorRed+"  "+why+"暂停接任失败:", err, colorReset)
		return
	}
	fmt.Println(colorGray + "  " + why + "已暂停接任 " + strconv.Itoa(minutes) + " 分钟" + colorReset)
}

// selfUpdate:菜单里的"更新到最新版"。与面板按钮共用 selfupdate 的升级事务:
// 升级前备份 → 下载校验 → 旧程序留作 .prev、新程序换上 → 重启 → 在这里同步等新版本健康,起不来就自动回滚。
func selfUpdate(ask func(string) string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("只支持 Linux 在线更新")
	}
	// 直连 GitHub 不通(纯 IPv6 机器)时经本机 WARP 再试
	selfupdate.SetFallbackPort(func() int {
		if m, err := settingsOf(menuDBPath()); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(m["warpPort"])); err == nil && p > 0 {
				return p
			}
		}
		return 40000
	})
	fmt.Println("  查询最新版本…")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	info, err := selfupdate.Check(ctx, version)
	cancel()
	if err != nil {
		return err
	}
	tag := info.Latest
	fmt.Printf("  当前 v%s → 最新 %s\n", version, tag)
	if !info.HasUpdate {
		if ask("  已是最新版,仍要重新安装? [y/N]: ") != "y" {
			return nil
		}
	}
	dbPath := menuDBPath()
	dataDir := filepath.Dir(dbPath)
	// 升级前备份:回滚时旧程序对新库不放心就还原它;失败不挡升级,只是回滚时少一道保险
	bk := ""
	if _, err := os.Stat(dbPath); err == nil {
		dir := filepath.Join(dataDir, "backups")
		os.MkdirAll(dir, 0o750)
		bk = filepath.Join(dir, selfupdate.BackupName("v"+version))
		if err := runBackup(dbPath, bk); err != nil {
			fmt.Println(colorRed+"  升级前备份失败(继续,回滚时只换回旧程序):", err, colorReset)
			bk = ""
		} else {
			fmt.Println("  升级前备份:", bk)
			selfupdate.Prune(dir, selfupdate.BackupPrefix, selfupdate.KeepBackups)
		}
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer dcancel()
	logf := func(f string, a ...interface{}) { fmt.Printf("  "+f+"\n", a...) }
	newPath, err := selfupdate.Stage(dctx, tag, binPath, logf)
	if err != nil {
		return err
	}
	if err := selfupdate.Swap(binPath, newPath, selfupdate.PrevPath(binPath)); err != nil {
		return err
	}
	plan := selfupdate.Plan{
		Bin: binPath, Prev: selfupdate.PrevPath(binPath), Failed: selfupdate.FailedPath(binPath),
		From: "v" + version, To: tag, URL: localPanelURL(dbPath), Service: "m-ui", OldPID: selfupdate.MainPID("m-ui"),
		DBPath: dbPath, Backup: bk, StatusPath: selfupdate.StatusPath(dataDir), Timeout: selfupdate.DefaultTimeout,
	}
	selfupdate.ClearStatus(plan.StatusPath)
	fmt.Println(colorGreen + "  已换上 " + tag + ",重启服务并等它起来(最多 90 秒)…" + colorReset)
	haPauseBefore(dbPath, 15, "升级前")
	systemctl("restart", "m-ui")
	st, err := selfupdate.Watch(plan, os.Stdout)
	switch {
	case err == nil:
		fmt.Println(colorGreen+"  更新完成:", st.Message, colorReset)
	case errors.Is(err, selfupdate.ErrRolledBack):
		fmt.Println(colorRed+"  "+st.Message, colorReset)
		fmt.Println("  日志:journalctl -u m-ui -n 50")
	default:
		return err
	}
	fmt.Println("  状态:", stateColored())
	return nil
}

// localPanelURL 按数据库里的设置拼出面板在本机的地址,健康检查用;只读打开,服务在跑、升级途中都能用。
func localPanelURL(dbPath string) string {
	s, err := settingsRO(dbPath)
	if err != nil {
		return ""
	}
	port := 2053
	if n, err := strconv.Atoi(strings.TrimSpace(s["webPort"])); err == nil && n > 0 {
		port = n
	}
	path := s["webPath"]
	if path == "" {
		path = "/app/"
	}
	return selfupdate.LocalURL(s["webCertFile"] != "", s["webListen"], port, path)
}

// printInstallSummary 供安装脚本调用:m-ui info -db <db>
func printInstallSummary(dbPath string) {
	printPanelInfo(dbPath)
	fmt.Println(colorGray + "  输入 m-ui 打开管理菜单(查看地址 / 一键体检 / 重置密码 / 改端口 / 备份还原 / 更新);面板打不开先跑 m-ui doctor" + colorReset)
	fmt.Println()
}

var _ = backup.DBName

// urlHostStr 同 model.URLHost,端口是字符串(设置里读出来的)。
func urlHostStr(scheme, host, port string) string {
	n, err := strconv.Atoi(port)
	if err != nil {
		return net.JoinHostPort(host, port)
	}
	return model.URLHost(scheme, host, n)
}
