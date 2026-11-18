package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/backup"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hop"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/runner"
)

// 菜单里各项的动作。能单独当子命令用的(logs / restart / update / doctor)在 main.go 里也接了一份,
// 文档里写命令比写「菜单第几项」稳:菜单以后再调顺序,命令不变。

// ---- 日志 ----

// showLogs m-ui logs:最近 n 行;errOnly = 只看报错;follow = 实时跟随(Ctrl+C 停,停了回到菜单而不是退出菜单)。
func showLogs(n int, errOnly, follow bool) {
	if runtime.GOOS != "linux" {
		fmt.Println("  非 Linux 没有 journal")
		return
	}
	switch {
	case errOnly:
		lines := errLinesOf(journalText(3000), 40)
		if len(lines) == 0 {
			fmt.Println(colorGreen + "  最近 3000 行日志里没有报错" + colorReset)
			return
		}
		for _, l := range lines {
			fmt.Println(l)
		}
	case follow:
		// 终端里按 Ctrl+C,信号发给整个前台进程组:journalctl 收到就停;这边先忽略掉,不然连菜单一起退了
		signal.Ignore(os.Interrupt)
		defer signal.Reset(os.Interrupt)
		cmd := exec.Command("journalctl", "-u", "m-ui", "-f", "-n", "30", "--no-pager")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		_ = cmd.Run()
		fmt.Println()
	default:
		if n <= 0 {
			n = 80
		}
		cmd := exec.Command("journalctl", "-u", "m-ui", "-n", strconv.Itoa(n), "--no-pager")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		_ = cmd.Run()
	}
}

func menuLogs(ask func(string) string) {
	fmt.Println("   1. 最近 80 行      2. 只看报错      3. 实时跟随(Ctrl+C 停)      0. 返回")
	switch ask(colorBold + "  请输入数字(回车 = 1): " + colorReset) {
	case "", "1":
		showLogs(80, false, false)
	case "2":
		showLogs(0, true, false)
	case "3":
		showLogs(0, false, true)
	}
}

// ---- 端口 / 路径 / 监听地址 ----

// menuNormPath 把填的路径规整成 /xxx/(和设置页同一个规则)。
func menuNormPath(p string) string {
	p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), "\"'"))
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// localLinePorts 部署在本机的线路各占哪个端口(停用的也算,端口给它留着)。只读。
func localLinePorts(dbPath string) map[int]string {
	out := map[int]string{}
	db, err := database.OpenReadOnly(dbPath)
	if err != nil {
		return out
	}
	if sq, e := db.DB(); e == nil {
		defer sq.Close()
	}
	self := render.LocalNodeID(db)
	var lines []model.Line
	db.Select("id, name, port, node_ids").Order("sort asc, id asc").Find(&lines)
	for _, l := range lines {
		if l.Port > 0 && render.LineOnNode(l, self) {
			if _, dup := out[l.Port]; !dup {
				out[l.Port] = l.Name
			}
		}
	}
	return out
}

func tcpBindable(port int) bool {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// checkPortChange 命令行改端口时的校验,和设置页同一口径:面板 / 订阅 / 代理面板互不相同(代理面板不监听时不算);
// 这次新换上的端口不能是本机线路占着的,也不能被别的程序占着。填错了要等重启才发现,那时面板已经起不来。
func checkPortChange(cur, kv map[string]string, linePorts map[int]string, bindable func(int) bool) error {
	after := map[string]string{}
	for k, v := range cur {
		after[k] = v
	}
	for k, v := range kv {
		after[k] = v
	}
	seen := map[int]string{}
	for _, w := range wantedPorts(after) {
		if other, dup := seen[w.Port]; dup {
			return fmt.Errorf("%s与%s的端口都是 %d,不能重复", other, w.Name, w.Port)
		}
		seen[w.Port] = w.Name
	}
	before := map[int]bool{}
	for _, w := range wantedPorts(cur) {
		before[w.Port] = true
	}
	for _, w := range wantedPorts(after) {
		if before[w.Port] {
			continue // 没换,或只是在自己的几个服务之间对调:占着它的就是自己
		}
		if name, dup := linePorts[w.Port]; dup {
			return fmt.Errorf("%s端口 %d 已被线路「%s」占用,换一个", w.Name, w.Port, name)
		}
		if !bindable(w.Port) {
			return fmt.Errorf("%s端口 %d 已被本机其它程序占用,换一个", w.Name, w.Port)
		}
	}
	return nil
}

// parseListen 监听地址的填法:all / * / 0.0.0.0 / :: = 全部地址(存成空);否则要是这台网卡上的地址或回环地址。
func parseListen(v string, local []string) (string, error) {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "all", "*", "0.0.0.0", "::", "[::]":
		return "", nil
	}
	ip := normIP(v)
	if ip == "" {
		return "", fmt.Errorf("监听地址要填这台机器上的一个 IP(或 all = 全部地址)")
	}
	if isLoopbackListen(ip) {
		return ip, nil
	}
	for _, a := range local {
		if normIP(a) == ip {
			return ip, nil
		}
	}
	return "", fmt.Errorf("%s 不在这台机器的网卡上:填了它面板会起不来", ip)
}

func menuPorts(dbPath string, ask func(string) string) {
	s, err := settingsOf(dbPath)
	if err != nil {
		fmt.Println(colorRed+"  读取数据库失败:", err, colorReset)
		return
	}
	kv := map[string]string{}
	askPort := func(label, key string, def int) bool {
		v := ask(fmt.Sprintf("  %s端口 [当前 %d,回车不改]: ", label, settingPort(s, key, def)))
		if v == "" {
			return true
		}
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 65535 {
			fmt.Println(colorRed + "  端口要填 1-65535" + colorReset)
			return false
		}
		kv[key] = v
		return true
	}
	if !askPort("面板", "webPort", 2053) {
		return
	}
	if v := ask(fmt.Sprintf("  面板路径 [当前 %s,回车不改]: ", or(s["webPath"], "/app/"))); v != "" {
		kv["webPath"] = menuNormPath(v)
	}
	curListen := strings.TrimSpace(s["webListen"])
	if v := ask(fmt.Sprintf("  面板监听地址 [当前 %s,回车不改;all = 全部地址]: ", or(curListen, "全部地址"))); v != "" {
		l, err := parseListen(v, localAddrs())
		if err != nil {
			fmt.Println(colorRed+"  ", err, colorReset)
			return
		}
		if l != curListen {
			kv["webListen"] = l
		}
	}
	if !askPort("订阅", "subPort", 2056) {
		return
	}
	if resellerListens(s) && !askPort("代理面板", "resellerPort", 2054) {
		return
	}
	if len(kv) == 0 {
		fmt.Println("  未改动")
		return
	}
	if err := checkPortChange(s, kv, localLinePorts(dbPath), tcpBindable); err != nil {
		fmt.Println(colorRed+"  ", err, colorReset)
		return
	}
	if err := runner.SetSettings(dbPath, kv); err != nil {
		fmt.Println(colorRed+"  写入失败:", err, colorReset)
		return
	}
	fmt.Println(colorGreen + "  已写入,重启后生效" + colorReset + colorGray + "(云厂商的安全组里记得放行新端口)" + colorReset)
	if ask("  现在重启? [Y/n]: ") != "n" {
		haPauseBefore(dbPath, 5, "重启前")
		systemctl("restart", "m-ui")
		time.Sleep(2 * time.Second)
		printPanelInfo(dbPath)
	}
}

// ---- 备份与还原 ----

// backupDirOf 本地备份目录:和面板「备份」页是同一个(设置里的 backupDir,没设就是数据目录下的 backups)。
func backupDirOf(dbPath string) string {
	if s, err := settingsRO(dbPath); err == nil {
		if d := strings.TrimSpace(s["backupDir"]); d != "" {
			return d
		}
	}
	return filepath.Join(filepath.Dir(dbPath), "backups")
}

type backupFile struct {
	Path string
	Size int64
	Time time.Time
}

// listBackups 备份目录里的 zip,最新的在前。
func listBackups(dir string) []backupFile {
	entries, _ := os.ReadDir(dir)
	var out []backupFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".zip") {
			continue
		}
		if st, err := e.Info(); err == nil {
			out = append(out, backupFile{filepath.Join(dir, e.Name()), st.Size(), st.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

// menuBackup 立即备份:存进备份目录(面板「备份」页也看得到、能下载),不再丢在当前目录。
func menuBackup(dbPath string) {
	dir := backupDirOf(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Println(colorRed+"  建备份目录失败:", err, colorReset)
		return
	}
	out := filepath.Join(dir, "m-ui-"+time.Now().Format("20060102-150405")+".zip")
	meta, err := runBackupMeta(dbPath, out)
	if err != nil {
		os.Remove(out)
		fmt.Println(colorRed+"  备份失败:", err, colorReset)
		return
	}
	size := ""
	if st, err := os.Stat(out); err == nil {
		size = "(" + sizeText(st.Size()) + ")"
	}
	fmt.Println(colorGreen+"  备份已写入:", out, size, colorReset)
	if len(meta.CertsUnreadable) > 0 {
		fmt.Printf("  注意:库里引用的 %d 个证书文件读不出来,没打进备份:%s\n", len(meta.CertsUnreadable), strings.Join(meta.CertsUnreadable, "、"))
	}
	fmt.Println(colorGray + "  里面有全部用户凭据和证书私钥,别放到公开的地方。面板「备份」页也能看到、下载这一份。" + colorReset)
}

// pickBackup 把用户输入的编号或路径变成备份文件的路径。
func pickBackup(in string, files []backupFile) (string, error) {
	in = strings.Trim(strings.TrimSpace(in), "\"'")
	if n, err := strconv.Atoi(in); err == nil {
		if n < 1 || n > len(files) {
			return "", fmt.Errorf("没有第 %d 个", n)
		}
		return files[n-1].Path, nil
	}
	if st, err := os.Stat(in); err != nil || st.IsDir() {
		return "", fmt.Errorf("找不到文件 %s", in)
	}
	return in, nil
}

func menuRestore(dbPath string, ask func(string) string) {
	dir := backupDirOf(dbPath)
	files := listBackups(dir)
	if len(files) > 12 {
		files = files[:12]
	}
	if len(files) == 0 {
		fmt.Println("  备份目录 " + dir + " 里没有备份。把备份文件(zip)传到这台服务器上,然后输入它的路径。")
	} else {
		fmt.Println("  备份目录 " + dir + " 里最近的备份:")
		for i, f := range files {
			fmt.Printf("  %2d. %-40s %8s   %s\n", i+1, filepath.Base(f.Path), sizeText(f.Size), f.Time.Format("2006-01-02 15:04"))
		}
	}
	in := ask("  输入编号,或备份文件的路径(回车取消): ")
	if in == "" {
		fmt.Println("  已取消")
		return
	}
	path, err := pickBackup(in, files)
	if err != nil {
		fmt.Println(colorRed+"  ", err, colorReset)
		return
	}
	sum, err := backup.Inspect(path)
	if err != nil {
		fmt.Println(colorRed+"  这不是一份能用的备份:", err, colorReset)
		return
	}
	what := fmt.Sprintf("%d 个用户 / %d 条线路 / %d 个上游", sum.Users, sum.Lines, sum.Upstreams)
	if sum.Meta.Version != "" {
		what += ",来自 v" + strings.TrimPrefix(sum.Meta.Version, "v")
	}
	if sum.Meta.Time > 0 {
		what += "," + time.Unix(sum.Meta.Time, 0).Format("2006-01-02 15:04") + " 备份的"
	}
	fmt.Println("  这份备份:" + what)
	if ask(colorRed+"  还原会用它换掉现在的全部数据(线路、用户、设置、证书;现在的库会另存一份)。输入 yes 确认: "+colorReset) != "yes" {
		fmt.Println("  已取消")
		return
	}
	wasActive := serviceState() == "active"
	if wasActive {
		haPauseBefore(dbPath, 10, "还原前")
		fmt.Println("  停止服务…")
		systemctl("stop", "m-ui")
	}
	rerr := runRestore(dbPath, path, os.Stdout)
	if rerr != nil {
		fmt.Println(colorRed+"  ", rerr, colorReset)
	}
	if wasActive || rerr == nil {
		fmt.Println("  启动服务…")
		systemctl("start", "m-ui")
		time.Sleep(3 * time.Second)
		fmt.Println("  状态:", stateColored())
	}
	if rerr == nil {
		fmt.Println(colorGreen + "  还原完成。面板地址、账号都是备份里的:" + colorReset)
		printPanelInfo(dbPath)
	}
}

// ---- 主机 / 副机 ----

// nodeJoinInfo 副机的接入信息:主机「服务器」页添加这台时要填的 API 地址和令牌。令牌还没有就现在生成(运行中的服务马上认)。
func nodeJoinInfo(dbPath string) (url, token string, err error) {
	url, _, _, err = panelInfo(dbPath)
	if err != nil {
		return "", "", err
	}
	s, err := settingsOf(dbPath)
	if err != nil {
		return "", "", err
	}
	token = strings.TrimSpace(s["nodeToken"])
	if token == "" {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return "", "", err
		}
		token = hex.EncodeToString(b)
		if err := runner.SetSettings(dbPath, map[string]string{"nodeToken": token}); err != nil {
			return "", "", err
		}
	}
	return url, token, nil
}

func printNodeJoin(dbPath string) {
	url, token, err := nodeJoinInfo(dbPath)
	if err != nil {
		fmt.Println(colorRed+"  读不出接入信息:", err, colorReset)
		return
	}
	fmt.Println()
	fmt.Println(colorBold + "  副机接入信息" + colorReset + colorGray + "(到主机面板「服务器」页 → 添加服务器,把这两项填进去)" + colorReset)
	fmt.Println(colorBold + "  API 地址: " + colorCyan + url + colorReset)
	fmt.Println(colorBold + "  令牌:     " + colorReset + token)
	if strings.HasPrefix(url, "http://") {
		fmt.Println(colorGray + "  这台面板走的是 HTTP:令牌和用户凭据会明文过公网。建议先给这台签一张证书(自签也行,主机那边勾「跳过证书校验」)。" + colorReset)
	}
	fmt.Println(colorGray + "  令牌等同这台的管理权限,别贴到公开的地方。" + colorReset)
}

func menuRole(dbPath string, ask func(string) string) {
	s, err := settingsOf(dbPath)
	if err != nil {
		fmt.Println(colorRed+"  读取数据库失败:", err, colorReset)
		return
	}
	node := isTrue(s["nodeMode"])
	fmt.Println("  当前角色:" + map[bool]string{false: "主机(自己管线路和用户)", true: "副机(线路和用户由主机下发)"}[node])
	if node {
		printNodeJoin(dbPath)
		fmt.Println()
	}
	target := "副机"
	warn := "切成副机之后,这台现有的线路 / 用户 / 上游会被主机下发的整份替换(切换前自动备份)。"
	if node {
		target, warn = "主机", "切成主机之后,这台不再接受原来主机的管理,线路和用户由它自己管。"
	}
	fmt.Println(colorGray + "  " + warn + colorReset)
	if ask("  切换为"+target+"?输入 yes 确认(回车返回): ") != "yes" {
		return
	}
	if !node { // 切成副机后本机的线路 / 用户 / 上游会被主机快照整表替换,先留一份备份
		dir := backupDirOf(dbPath)
		os.MkdirAll(dir, 0o700)
		out := filepath.Join(dir, "before-node-"+time.Now().Format("20060102-150405")+".zip")
		if err := runBackup(dbPath, out); err != nil {
			fmt.Println(colorRed+"  切换前备份失败:", err, colorReset)
			if ask("  没有备份,还要继续吗? [y/N]: ") != "y" {
				fmt.Println("  已取消")
				return
			}
		} else {
			fmt.Println("  已备份到:", out)
		}
	}
	// 换了角色就是换了集群:清掉本机的主机容灾状态(纪元、认的主机、交接标记、副本……),
	// 不然新主机(或没开容灾的主机)发来的请求会一律被当成旧主机拒掉
	if err := haResetLocal(dbPath); err != nil {
		fmt.Println(colorRed+"  清掉本机的主机容灾状态失败:", err, colorReset)
		return
	}
	if err := runner.SetSettings(dbPath, map[string]string{"nodeMode": strconv.FormatBool(!node)}); err != nil {
		fmt.Println(colorRed+"  写入失败:", err, colorReset)
		return
	}
	fmt.Println(colorGreen + "  已切换为" + target + ",重启服务…" + colorReset)
	systemctl("restart", "m-ui")
	time.Sleep(2 * time.Second)
	fmt.Println("  状态:", stateColored())
	if !node {
		printNodeJoin(dbPath)
	}
}

// ---- 卸载 ----

func menuUninstall(dbPath string, ask func(string) string) bool {
	if ask(colorRed+"  将停止并删除 m-ui 的程序和服务。输入 yes 确认: "+colorReset) != "yes" {
		fmt.Println("  已取消")
		return false
	}
	systemctl("disable", "--now", "m-ui")
	// 防火墙收尾:m-ui 落在内核里的规则、在系统防火墙里替自己放行的端口、端口跳跃的转发,都撤掉(程序都没了,留着没有意义)
	if requireDB(dbPath) == nil && runtime.GOOS == "linux" && os.Geteuid() == 0 {
		td, err := runner.FwLocalTeardown(dbPath)
		_ = hop.Apply(nil)
		switch {
		case err != nil:
			fmt.Println(colorRed+"  防火墙收尾没做成:", err, colorReset)
		default:
			fmt.Println("  已撤掉 m-ui 自己的防火墙规则" + map[bool]string{true: ",并从系统防火墙里收回它放行的端口:" + strings.Join(td.Ports, "、"), false: ""}[len(td.Ports) > 0])
			for _, e := range td.Errs {
				fmt.Println(colorRed+"  ", e, colorReset)
			}
			if td.Opened {
				fmt.Println(colorYellow + "  这台的系统防火墙是 m-ui 放开过的(现在所有入站都放行)。" + colorReset)
				if ask("  恢复成放开之前的样子? [Y/n]: ") != "n" {
					if err := fwCommand(dbPath, "undo"); err != nil {
						fmt.Println(colorRed+"  恢复失败:", err, colorReset)
					}
				} else {
					fmt.Println(colorGray + "  保持放开。以后想收紧,用系统自己的防火墙(ufw / firewalld)或云厂商的安全组。" + colorReset)
				}
			}
		}
	}
	_ = os.Remove(unitPath)
	// 运维页写过的系统文件一并清掉:journald 上限、内核参数、服务的句柄上限 override
	for _, p := range []string{"/etc/systemd/journald.conf.d/99-m-ui.conf", "/etc/sysctl.d/99-m-ui-tune.conf", "/etc/systemd/system/m-ui.service.d/override.conf"} {
		_ = os.Remove(p)
	}
	_ = os.Remove("/etc/systemd/system/m-ui.service.d")
	if runtime.GOOS == "linux" {
		_ = exec.Command("systemctl", "daemon-reload").Run()
	}
	dir := filepath.Dir(dbPath)
	if ask("  同时删除数据目录 "+dir+"(数据库、证书、备份)? [y/N]: ") == "y" {
		_ = os.RemoveAll(dir)
		fmt.Println("  数据目录已删除")
	} else {
		fmt.Println(colorGray + "  数据目录留着:以后重新安装,原来的线路、用户都还在。" + colorReset)
	}
	for _, p := range []string{binPath, binPath + ".prev", binPath + ".failed", binPath + ".new"} {
		_ = os.Remove(p)
	}
	fmt.Println(colorGreen + "  m-ui 已卸载" + colorReset)
	return true
}
