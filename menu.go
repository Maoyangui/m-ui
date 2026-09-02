package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fangjunsheng555/m-ui/acme"
	"github.com/fangjunsheng555/m-ui/backup"
	"github.com/fangjunsheng555/m-ui/brand"
	"github.com/fangjunsheng555/m-ui/certutil"
	"github.com/fangjunsheng555/m-ui/database"
	"github.com/fangjunsheng555/m-ui/runner"
)

const (
	unitPath   = "/etc/systemd/system/m-ui.service"
	binPath    = "/usr/local/bin/m-ui"
	repoAPI    = "https://api.github.com/repos/" + brand.RepoPath + "/releases/latest"
	defaultDB  = "/etc/m-ui/m-ui.db"
	colorReset = "\033[0m"
	colorBold  = "\033[1m"
	colorGreen = "\033[32m"
	colorRed   = "\033[31m"
	colorCyan  = "\033[36m"
	colorGray  = "\033[90m"
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

// PanelInfo 组装面板地址信息(安装脚本与菜单共用)。
func panelInfo(dbPath string) (url string, user string, defaultPw bool, err error) {
	s, err := settingsOf(dbPath)
	if err != nil {
		return "", "", false, err
	}
	host := s["subDomain"]
	if host == "" {
		host = s["webDomain"]
	}
	if host == "" {
		host = s["publicIp"]
	}
	if host == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		host = acme.PublicIP(ctx)
		cancel()
	}
	if host == "" {
		host = "<服务器IP>"
	}
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
	return fmt.Sprintf("%s://%s:%s%s", scheme, host, port, path), "admin", s["adminDefault"] == "true", nil
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
		fmt.Println(colorBold + "  密码:     " + colorReset + colorGray + "(已修改;忘记可用菜单 5 重置)" + colorReset)
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
	fmt.Println()
}

// runMenu 交互式管理菜单(m-ui 不带参数时进入)。
func runMenu() {
	in := bufio.NewReader(os.Stdin)
	ask := func(prompt string) string {
		fmt.Print(prompt)
		line, _ := in.ReadString('\n')
		return strings.TrimSpace(line)
	}
	dbPath := menuDBPath()
	for {
		fmt.Println()
		fmt.Println(colorBold + colorCyan + "  m-ui 管理菜单  " + colorReset + colorGray + "v" + version + "  服务状态: " + colorReset + stateColored())
		fmt.Println(colorGray + "  ────────────────────────────────────────" + colorReset)
		fmt.Println("   1. 查看面板地址 / 账号")
		fmt.Println("   2. 重启 m-ui")
		fmt.Println("   3. 停止 m-ui")
		fmt.Println("   4. 启动 m-ui")
		fmt.Println("   5. 重置管理员密码")
		fmt.Println("   6. 修改面板端口 / 路径 / 订阅端口")
		fmt.Println("   7. 重置全部设置(保留线路/用户/上游/服务器)")
		fmt.Println("   8. 查看最近日志")
		fmt.Println("   9. 立即备份(zip 到当前目录)")
		fmt.Println("  10. 更新到最新版")
		fmt.Println("  11. 生成自签证书(无域名时)")
		fmt.Println("  12. 切换 主机 / 副机 角色")
		fmt.Println("  13. 关闭两步验证(2FA,手机丢失时用)")
		fmt.Println("  14. 卸载 m-ui")
		fmt.Println("   0. 退出")
		choice := ask(colorBold + "  请输入数字: " + colorReset)
		switch choice {
		case "0", "q", "":
			return
		case "1":
			printPanelInfo(dbPath)
		case "2":
			systemctl("restart", "m-ui")
			time.Sleep(2 * time.Second)
			fmt.Println("  状态:", stateColored())
		case "3":
			systemctl("stop", "m-ui")
			fmt.Println("  状态:", stateColored())
		case "4":
			systemctl("start", "m-ui")
			time.Sleep(2 * time.Second)
			fmt.Println("  状态:", stateColored())
		case "5":
			pw := ask("  新密码(回车 = 随机生成): ")
			newPw, err := runner.ResetPassword(dbPath, "admin", pw)
			if err != nil {
				fmt.Println(colorRed+"  失败:", err, colorReset)
				break
			}
			_ = runner.SetSettings(dbPath, map[string]string{"adminDefault": "false", "totpEnabled": "false", "totpSecret": ""})
			fmt.Println(colorGreen+"  管理员 admin 新密码: "+colorBold+newPw+colorReset, colorGray+"(运行中的面板立即生效;两步验证已一并关闭)"+colorReset)
		case "6":
			s, _ := settingsOf(dbPath)
			kv := map[string]string{}
			if v := ask(fmt.Sprintf("  面板端口 [当前 %s,回车不改]: ", or(s["webPort"], "2053"))); v != "" {
				if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 65535 {
					fmt.Println(colorRed + "  端口无效" + colorReset)
					break
				}
				kv["webPort"] = v
			}
			if v := ask(fmt.Sprintf("  面板路径 [当前 %s,回车不改]: ", or(s["webPath"], "/app/"))); v != "" {
				if !strings.HasPrefix(v, "/") {
					v = "/" + v
				}
				if !strings.HasSuffix(v, "/") {
					v += "/"
				}
				kv["webPath"] = v
			}
			if v := ask(fmt.Sprintf("  订阅端口 [当前 %s,回车不改]: ", or(s["subPort"], "2056"))); v != "" {
				if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 65535 {
					fmt.Println(colorRed + "  端口无效" + colorReset)
					break
				}
				kv["subPort"] = v
			}
			if len(kv) == 0 {
				fmt.Println("  未改动")
				break
			}
			if err := runner.SetSettings(dbPath, kv); err != nil {
				fmt.Println(colorRed+"  写入失败:", err, colorReset)
				break
			}
			fmt.Println(colorGreen + "  已写入,重启后生效" + colorReset)
			if ask("  现在重启? [Y/n]: ") != "n" {
				systemctl("restart", "m-ui")
				time.Sleep(2 * time.Second)
				printPanelInfo(dbPath)
			}
		case "7":
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
		case "8":
			if runtime.GOOS == "linux" {
				cmd := exec.Command("journalctl", "-u", "m-ui", "-n", "60", "--no-pager")
				cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
				_ = cmd.Run()
			} else {
				fmt.Println("  非 Linux 无 journal")
			}
		case "9":
			out := "m-ui-" + time.Now().Format("20060102-150405") + ".zip"
			if err := runBackup(dbPath, out); err != nil {
				fmt.Println(colorRed+"  备份失败:", err, colorReset)
				break
			}
			abs, _ := filepath.Abs(out)
			fmt.Println(colorGreen+"  备份已写入:", abs, colorReset)
		case "10":
			if err := selfUpdate(ask); err != nil {
				fmt.Println(colorRed+"  更新失败:", err, colorReset)
			}
		case "11":
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
			_ = runner.SetSettings(dbPath, map[string]string{"certFile": crt, "keyFile": key})
			fmt.Println(colorGreen+"  自签证书已生成并设为数据面证书:", crt, colorReset)
			fmt.Println(colorGray + "  订阅会自动带 allow-insecure;重启后生效" + colorReset)
			if ask("  现在重启? [Y/n]: ") != "n" {
				systemctl("restart", "m-ui")
			}
		case "12":
			s, _ := settingsOf(dbPath)
			cur := strings.EqualFold(s["nodeMode"], "true")
			target := "副机(由主机管理线路/用户)"
			if cur {
				target = "主机"
			}
			if ask("  当前角色: "+map[bool]string{false: "主机", true: "副机"}[cur]+" → 切换为 "+target+"?输入 yes 确认: ") != "yes" {
				fmt.Println("  已取消")
				break
			}
			_ = runner.SetSettings(dbPath, map[string]string{"nodeMode": strconv.FormatBool(!cur)})
			fmt.Println(colorGreen + "  已切换,重启生效" + colorReset)
			systemctl("restart", "m-ui")
		case "13":
			if err := runner.SetSettings(dbPath, map[string]string{"totpEnabled": "false", "totpSecret": ""}); err != nil {
				fmt.Println(colorRed+"  失败:", err, colorReset)
				break
			}
			fmt.Println(colorGreen + "  已关闭两步验证,现在只需密码即可登录(运行中的面板立即生效)" + colorReset)
		case "14":
			if ask(colorRed+"  将停止并删除 m-ui 程序与服务单元。输入 yes 确认: "+colorReset) != "yes" {
				fmt.Println("  已取消")
				break
			}
			systemctl("disable", "--now", "m-ui")
			_ = os.Remove(unitPath)
			_ = exec.Command("systemctl", "daemon-reload").Run()
			if ask("  同时删除数据目录 "+filepath.Dir(dbPath)+"(数据库/证书/备份)? [y/N]: ") == "y" {
				_ = os.RemoveAll(filepath.Dir(dbPath))
				fmt.Println("  数据目录已删除")
			}
			_ = os.Remove(binPath)
			fmt.Println(colorGreen + "  m-ui 已卸载" + colorReset)
			return
		default:
			fmt.Println("  无效选项")
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

// resetSettings 清空 settings 表(保留其它表),管理员密码不在其中。
func resetSettings(dbPath string) error {
	db, err := database.Open(dbPath)
	if err != nil {
		return err
	}
	defer database.Close(db)
	return db.Exec("DELETE FROM settings").Error
}

// selfUpdate 从 GitHub Releases 下载最新版替换二进制并重启。
// latestTag 取最新 Release 的标签:先跟随 releases/latest 的重定向(不经 API,没有匿名限流),
// 失败再查 GitHub API(私有仓库可设 GITHUB_TOKEN)。
func latestTag(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://github.com/"+brand.RepoPath+"/releases/latest", nil)
	req.Header.Set("User-Agent", "m-ui")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		if p := resp.Request.URL.Path; resp.StatusCode == 200 && strings.Contains(p, "/tag/") {
			return p[strings.LastIndex(p, "/tag/")+5:], nil
		}
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", repoAPI, nil)
	req.Header.Set("User-Agent", "m-ui")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("无法读取 Releases(HTTP %d):还没有发布版本、网络受阻,或仓库为私有(可设置 GITHUB_TOKEN);也可手动下载后执行: bash install.sh <tar.gz>", resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil || rel.TagName == "" {
		return "", fmt.Errorf("解析 Releases 失败")
	}
	return rel.TagName, nil
}

func selfUpdate(ask func(string) string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("只支持 Linux 在线更新")
	}
	fmt.Println("  查询最新版本…")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tag, err := latestTag(ctx)
	if err != nil {
		return err
	}
	want := "m-ui-linux-" + runtime.GOARCH + ".tar.gz"
	url := "https://github.com/" + brand.RepoPath + "/releases/download/" + tag + "/" + want
	fmt.Printf("  当前 v%s → 最新 %s\n", version, tag)
	if strings.TrimPrefix(tag, "v") == version {
		if ask("  已是最新版,仍要重新安装? [y/N]: ") != "y" {
			return nil
		}
	}
	fmt.Println("  下载", url)
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer dcancel()
	dreq, _ := http.NewRequestWithContext(dctx, "GET", url, nil)
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		dreq.Header.Set("Authorization", "Bearer "+tok)
	}
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		return err
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != 200 {
		return fmt.Errorf("下载 HTTP %d", dresp.StatusCode)
	}
	gz, err := gzip.NewReader(dresp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	tmp := binPath + ".new"
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if filepath.Base(h.Name) == "m-ui" && h.Typeflag == tar.TypeReg {
			f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			found = true
		}
	}
	if !found {
		return fmt.Errorf("压缩包里没有 m-ui")
	}
	if out, err := exec.Command(tmp, "version").CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("新二进制无法运行: %v %s", err, out)
	}
	if err := os.Rename(tmp, binPath); err != nil {
		return err
	}
	fmt.Println(colorGreen + "  已更新为 " + tag + ",重启服务…" + colorReset)
	systemctl("restart", "m-ui")
	time.Sleep(2 * time.Second)
	fmt.Println("  状态:", stateColored())
	return nil
}

// printInstallSummary 供安装脚本调用:m-ui info -db <db>
func printInstallSummary(dbPath string) {
	printPanelInfo(dbPath)
	fmt.Println(colorGray + "  输入 m-ui 打开管理菜单(重启/更新/改密码/改端口/备份/卸载)" + colorReset)
	fmt.Println()
}

var _ = backup.DBName
