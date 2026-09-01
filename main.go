package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/fangjunsheng555/m-ui/certutil"
	"github.com/fangjunsheng555/m-ui/core"
	"github.com/fangjunsheng555/m-ui/database"
	"github.com/fangjunsheng555/m-ui/importer"
	"github.com/fangjunsheng555/m-ui/render"
	"github.com/fangjunsheng555/m-ui/runner"
	"github.com/fangjunsheng555/m-ui/web"
)

func init() {
	web.Version = version
	runner.SetPanelStarter(func(r *runner.Runner) error {
		return web.NewServer(r).Start()
	})
}

var version = "0.1.0-p0"

func usage() {
	fmt.Println("m-ui", version)
	fmt.Println()
	fmt.Println("用法:")
	fmt.Println("  m-ui run                          启动面板(主/副角色由数据库设置 nodeMode 决定)")
	fmt.Println("  m-ui import -from <s-ui.db> [...] 从旧 s-ui 数据库迁移")
	fmt.Println("  m-ui version                      显示版本")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Println("m-ui", version)
	case "import":
		fs := flag.NewFlagSet("import", flag.ExitOnError)
		from := fs.String("from", "", "旧 s-ui 数据库路径(只读打开,绝不修改源文件)")
		to := fs.String("to", "m-ui.db", "生成的 m-ui 数据库路径")
		order := fs.String("order", "", "线路排序文件:每行一个线路名,可选")
		title := fs.String("title", "", "订阅 Profile-Title,可选")
		force := fs.Bool("force", false, "目标已存在时覆盖")
		fs.Parse(os.Args[2:])
		if *from == "" {
			fmt.Fprintln(os.Stderr, "缺少 -from 参数")
			fs.Usage()
			os.Exit(2)
		}
		if err := importer.Run(*from, *to, *order, *title, *force); err != nil {
			fmt.Fprintln(os.Stderr, "导入失败:", err)
			os.Exit(1)
		}
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
