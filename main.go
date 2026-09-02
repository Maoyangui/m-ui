package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/backup"
	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/importer"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/runner"
	"github.com/Maoyangui/m-ui/web"
)

func init() {
	web.Version = version
	runner.Version = version
	runner.SetPanelStarter(func(r *runner.Runner) error {
		return web.NewServer(r).Start()
	})
}

var version = "0.1.1"

func usage() {
	fmt.Println("m-ui", version)
	fmt.Println()
	fmt.Println("不带参数运行 m-ui 进入管理菜单(重启 / 更新 / 查看地址账号 / 重置密码 / 备份 / 卸载)。")
	fmt.Println()
	fmt.Println("子命令:")
	rows := [][2]string{
		{"run [-db 路径]", "启动面板(主/副角色由设置 nodeMode 决定)"},
		{"menu", "管理菜单(同不带参数)"},
		{"info [-db 路径]", "打印面板地址与账号"},
		{"import -from <旧面板.db> [-to m-ui.db]", "从旧面板数据库迁移(兼容导入)"},
		{"backup -db <m-ui.db> [-out zip]", "生成备份(含证书)"},
		{"restore -db <m-ui.db> -from <zip|db>", "还原备份(服务停止时执行)"},
		{"selfsign -hosts <域名,IP>", "生成自签证书"},
		{"passwd -db <m-ui.db> [-password 新密码]", "重置管理员密码"},
		{"set -db <m-ui.db> key=value ...", "写入设置(如 webPort=3053 nodeMode=true)"},
		{"render -db <m-ui.db>", "打印并校验 sing-box 配置"},
		{"version", "显示版本"},
	}
	for _, r := range rows {
		fmt.Printf("  m-ui %-42s %s\n", r[0], r[1])
	}
}

func main() {
	if len(os.Args) < 2 {
		runMenu()
		return
	}
	switch os.Args[1] {
	case "menu":
		runMenu()
	case "help", "-h", "--help":
		usage()
	case "info":
		fs := flag.NewFlagSet("info", flag.ExitOnError)
		dbPath := fs.String("db", menuDBPath(), "m-ui 数据库路径")
		fs.Parse(os.Args[2:])
		printInstallSummary(*dbPath)
	case "version", "-v", "--version":
		fmt.Println("m-ui", version)
	case "import":
		fs := flag.NewFlagSet("import", flag.ExitOnError)
		from := fs.String("from", "", "旧面板数据库路径(只读打开,绝不修改源文件)")
		to := fs.String("to", "m-ui.db", "生成的 m-ui 数据库路径")
		order := fs.String("order", "", "线路排序文件:每行一个线路名,可选")
		title := fs.String("title", "", "订阅 Profile-Title,可选")
		force := fs.Bool("force", false, "目标已存在时覆盖")
		usersOnly := fs.Bool("users-only", false, "只把旧库的用户并入已有 m-ui 库(名称/启停/配额/到期/用量),线路与设置不动")
		noAssign := fs.Bool("no-assign", false, "users-only 时不给新用户分配现有线路")
		fs.Parse(os.Args[2:])
		if *from == "" {
			fmt.Fprintln(os.Stderr, "缺少 -from 参数")
			fs.Usage()
			os.Exit(2)
		}
		if *usersOnly {
			db, err := database.Open(*to)
			if err != nil {
				fmt.Fprintln(os.Stderr, "打开 m-ui 数据库失败:", err)
				os.Exit(1)
			}
			sum, err := importer.ImportUsersOnly(*from, db, !*noAssign)
			database.Close(db)
			if err != nil {
				fmt.Fprintln(os.Stderr, "导入失败:", err)
				os.Exit(1)
			}
			fmt.Printf("用户导入完成:新增 %d,更新 %d,分配线路 %d", sum.Created, sum.Updated, sum.Assigned)
			if len(sum.Skipped) > 0 {
				fmt.Printf(",跳过 %v", sum.Skipped)
			}
			fmt.Println()
			fmt.Println("m-ui 正在运行时请执行 systemctl restart m-ui,让数据面加载新用户")
			return
		}
		if err := importer.Run(*from, *to, *order, *title, *force); err != nil {
			fmt.Fprintln(os.Stderr, "导入失败:", err)
			os.Exit(1)
		}
	case "backup":
		fs := flag.NewFlagSet("backup", flag.ExitOnError)
		dbPath := fs.String("db", "m-ui.db", "m-ui 数据库路径")
		out := fs.String("out", "", "输出 zip 路径(默认 m-ui-<时间>.zip)")
		fs.Parse(os.Args[2:])
		if *out == "" {
			*out = "m-ui-" + time.Now().Format("20060102-150405") + ".zip"
		}
		if err := runBackup(*dbPath, *out); err != nil {
			fmt.Fprintln(os.Stderr, "备份失败:", err)
			os.Exit(1)
		}
		fmt.Println("备份已写入:", *out)
	case "restore":
		fs := flag.NewFlagSet("restore", flag.ExitOnError)
		dbPath := fs.String("db", "m-ui.db", "m-ui 数据库路径")
		from := fs.String("from", "", "备份文件(zip 或 .db)")
		fs.Parse(os.Args[2:])
		if *from == "" {
			fmt.Fprintln(os.Stderr, "缺少 -from 参数")
			os.Exit(2)
		}
		sum, err := backup.Inspect(*from)
		if err != nil {
			fmt.Fprintln(os.Stderr, "备份文件无效:", err)
			os.Exit(1)
		}
		if err := backup.Restore(*dbPath, *from); err != nil {
			fmt.Fprintln(os.Stderr, "还原失败:", err)
			os.Exit(1)
		}
		fmt.Printf("已还原:%d 用户 / %d 线路 / %d 上游,证书 %d 个(旧库保留为 .bak-*)\n", sum.Users, sum.Lines, sum.Upstreams, len(sum.Meta.Certs))
	case "set":
		fs := flag.NewFlagSet("set", flag.ExitOnError)
		dbPath := fs.String("db", "m-ui.db", "m-ui 数据库路径")
		fs.Parse(os.Args[2:])
		if fs.NArg() == 0 {
			fmt.Fprintln(os.Stderr, "用法: m-ui set -db <m-ui.db> key=value [key=value ...]   例: webPort=3053 nodeMode=true")
			os.Exit(2)
		}
		kv := map[string]string{}
		for _, a := range fs.Args() {
			k, v, ok := strings.Cut(a, "=")
			if !ok || k == "" {
				fmt.Fprintln(os.Stderr, "参数需为 key=value:", a)
				os.Exit(2)
			}
			kv[k] = v
		}
		if err := runner.SetSettings(*dbPath, kv); err != nil {
			fmt.Fprintln(os.Stderr, "写入失败:", err)
			os.Exit(1)
		}
		fmt.Printf("已写入 %d 项设置(运行中的 m-ui 需重启生效)\n", len(kv))
	case "passwd":
		fs := flag.NewFlagSet("passwd", flag.ExitOnError)
		dbPath := fs.String("db", "m-ui.db", "m-ui 数据库路径")
		user := fs.String("user", "admin", "管理员用户名(不存在则创建)")
		pw := fs.String("password", "", "新密码(留空则随机生成并打印)")
		fs.Parse(os.Args[2:])
		newPw, err := runner.ResetPassword(*dbPath, *user, *pw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "重置失败:", err)
			os.Exit(1)
		}
		_ = runner.SetSettings(*dbPath, map[string]string{"adminDefault": "false", "totpEnabled": "false", "totpSecret": ""})
		fmt.Printf("管理员 %s 密码已更新: %s(两步验证已一并关闭)\n", *user, newPw)
	case "selfsign":
		fs := flag.NewFlagSet("selfsign", flag.ExitOnError)
		hosts := fs.String("hosts", "", "域名或 IP,逗号分隔(必填)")
		cert := fs.String("cert", "cert/main.crt", "证书输出路径")
		key := fs.String("key", "cert/main.key", "私钥输出路径")
		days := fs.Int("days", 365, "有效天数")
		fs.Parse(os.Args[2:])
		if *hosts == "" {
			fmt.Fprintln(os.Stderr, "缺少 -hosts 参数")
			fs.Usage()
			os.Exit(2)
		}
		if err := certutil.GenerateSelfSigned(strings.Split(*hosts, ","), *cert, *key, *days); err != nil {
			fmt.Fprintln(os.Stderr, "自签失败:", err)
			os.Exit(1)
		}
		fmt.Println("自签证书已生成:", *cert, *key)
	case "render":
		fs := flag.NewFlagSet("render", flag.ExitOnError)
		dbPath := fs.String("db", "m-ui.db", "m-ui 数据库路径")
		out := fs.String("out", "", "配置输出文件(默认打印到标准输出)")
		validate := fs.Bool("validate", true, "用 sing-box 解析校验渲染结果")
		fs.Parse(os.Args[2:])
		if err := runRender(*dbPath, *out, *validate); err != nil {
			fmt.Fprintln(os.Stderr, "渲染失败:", err)
			os.Exit(1)
		}
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		dbPath := fs.String("db", "m-ui.db", "m-ui 数据库路径")
		fs.Parse(os.Args[2:])
		if err := runner.Run(*dbPath); err != nil {
			fmt.Fprintln(os.Stderr, "启动失败:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func runBackup(dbPath, out string) error {
	db, err := database.Open(dbPath)
	if err != nil {
		return err
	}
	defer database.Close(db)
	get := func(key string) string {
		var v string
		db.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
		return v
	}
	var certs []string
	for _, k := range []string{"certFile", "keyFile", "webCertFile", "webKeyFile", "subCertFile", "subKeyFile"} {
		certs = append(certs, get(k))
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	return backup.Create(db, certs, version, f)
}

func runRender(dbPath, out string, validate bool) error {
	db, err := database.Open(dbPath)
	if err != nil {
		return err
	}
	get := func(key string) string {
		var v string
		db.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
		return v
	}
	cert := render.NodeCert{
		ServerName: get("webDomain"),
		CertPath:   get("webCertFile"),
		KeyPath:    get("webKeyFile"),
	}
	raw, err := render.BuildConfig(db, cert)
	if err != nil {
		return err
	}
	if validate {
		if err := core.ParseConfig(raw); err != nil {
			return fmt.Errorf("sing-box 校验失败: %w", err)
		}
		fmt.Fprintln(os.Stderr, "✅ sing-box 解析校验通过")
	}
	if out == "" {
		fmt.Println(string(raw))
	} else if err := os.WriteFile(out, raw, 0o600); err != nil {
		return err
	} else {
		fmt.Fprintln(os.Stderr, "配置已写入:", out)
	}
	return nil
}
