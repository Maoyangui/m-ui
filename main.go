package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/fangjunsheng555/m-ui/importer"
)

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
	case "run":
		fmt.Println("m-ui run:数据面与面板在 P1/P2 实现,当前为 P0(骨架+导入器)。")
	default:
		usage()
		os.Exit(2)
	}
}
