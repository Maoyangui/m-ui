package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 审计记录在日志页「操作审计」和概览「最近操作」里按 assets/js/audit.js 译成界面上的话,认不出的原样显示库里的英文短词
// (D-62 之前有四十来个动作在中文界面上显示成 password、subpage、fence 这样,容灾命令行的 reset 还被译成了「重置流量」)。
// 这里扫一遍写审计的地方 —— web 的 s.audit / s.auditAs 和 audit 回调(handleAPIKeys / handleKeyLogs / handlePageLogo)、
// ha 的 s.audit / auditTo、jobs 和 rules 的 record、直接写的 model.Change{Key, Action} —— 每个对象、每个动作在中英两张表里都要有名字。
// 动作是「字面量 + 变量」拼出来的(比如 "server:"+op)只能查前缀:前缀自己有名字,或者至少有一个以它开头的整句名字;
// map[bool]string{…}[x] 这样拼的把每个取值都展开查;整个放在变量里的抄在下面的 dynamicAuditActions 里。
func TestAuditLabelsExist(t *testing.T) {
	dict := helpI18n(t)
	js, err := fs.ReadFile(assets, "assets/js/audit.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)const OBJ_NAV = \{(.*?)\};`).FindSubmatch(js)
	if m == nil {
		t.Fatal("audit.js 里找不到 OBJ_NAV")
	}
	nav := map[string]bool{}
	for _, k := range regexp.MustCompile(`(\w+): '`).FindAllSubmatch(m[1], -1) {
		nav[string(k[1])] = true
	}
	if len(nav) < 10 {
		t.Fatalf("OBJ_NAV 只解析出 %d 个对象,格式可能变了", len(nav))
	}

	recs := auditCalls(t, ".", "../ha", "../jobs", "../rules", "../runner", "../hub", "../ops", "../monitor", "../backup", "../importer")
	for _, d := range dynamicAuditActions {
		for _, a := range d.acts {
			recs = append(recs, auditRec{key: d.key, act: a, at: d.at})
		}
	}
	if len(recs) < 100 {
		t.Fatalf("只扫到 %d 处写审计,扫描方式可能失效了", len(recs))
	}

	userSpecial := regexp.MustCompile(`^(batch:plan:(renew|extend|bind)(:.*)?|plan:bind|plan:(renew|extend|replace|stack|prorate):.*)$`)
	batch := regexp.MustCompile(`^batch:(.+)$`)
	named := func(d map[string]string, key, a string, prefix bool) bool {
		dot := strings.ReplaceAll(a, ":", ".")
		if !prefix {
			if key == "user" && userSpecial.MatchString(a) {
				return true // audit.js 对用户的套餐动作单独拼句子
			}
			if d["audit.kv."+key+"."+dot] != "" || d["audit.v."+dot] != "" {
				return true
			}
			// 「前缀:后面」的通用拆法会把后面那段原样显示,字面量动作不算有名字;只有批量的后面那段本身是动作名
			if mm := batch.FindStringSubmatch(a); mm != nil {
				return d["audit.v."+strings.ReplaceAll(mm[1], ":", ".")] != ""
			}
			return false
		}
		if a == "batch:" {
			return true // 批量的动作名在别处是字面量,那里已经查过
		}
		for k := range d {
			if strings.HasPrefix(k, "audit.v."+dot) || strings.HasPrefix(k, "audit.kv."+key+"."+dot) {
				return true
			}
		}
		return strings.HasSuffix(a, ":") && d["audit.v."+strings.TrimSuffix(dot, ".")] != ""
	}

	var missing []string
	seen := map[string]bool{}
	for _, r := range recs {
		id := r.key + " " + r.act
		if r.prefix {
			id += "…"
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		t.Logf("%-10s %-28s %s", r.key, r.act+map[bool]string{true: "…"}[r.prefix], r.at)
		for _, lang := range []string{"zh", "en"} {
			d := dict[lang]
			if r.key != "" && !nav[r.key] && d["audit.o."+r.key] == "" {
				missing = append(missing, lang+" 对象 "+strconv.Quote(r.key)+"  ("+r.at+")")
			}
			if !named(d, r.key, r.act, r.prefix) {
				missing = append(missing, lang+" 动作 "+strconv.Quote(r.key+" "+r.act)+map[bool]string{true: "(前缀)"}[r.prefix]+"  ("+r.at+")")
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("这些审计对象 / 动作在日志页会原样显示英文短词(i18n.js 里补 audit.o.* / audit.v.*,同名动作在不同对象下意思不同的补 audit.kv.对象.动作):\n  %s",
			strings.Join(dedupe(missing), "\n  "))
	}
}

// dynamicAuditActions 动作名整个放在变量里、扫不出来的,手抄一份可能的取值(改了源码要跟着改)。
var dynamicAuditActions = []struct {
	key  string
	acts []string
	at   string
}{
	{"ha", []string{"demoted", "yielded"}, "ha/demote.go 变为副机"},
	{"ha", []string{"takeover", "became-master"}, "ha/promote.go 接任 / 设为主机后成为主机"},
	{"ha", []string{"takeover", "set-master", "abandon", "pause", "resume", "yield", "reset"}, "ha/cmd.go 命令行(status 不记)"},
}

type auditRec struct {
	key, act string
	prefix   bool // act 只是前缀(后面拼的是变量)
	at       string
}

// auditCalls 扫各包写审计的调用,取出对象和动作(只认字面量;拼接的取前缀;map 字面量取下标的展开)。
func auditCalls(t *testing.T, dirs ...string) []auditRec {
	t.Helper()
	// acts 返回表达式可能的取值;prefix 为真表示只知道前缀
	var acts func(e ast.Expr) (vals []string, prefix, ok bool)
	acts = func(e ast.Expr) ([]string, bool, bool) {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				s, err := strconv.Unquote(v.Value)
				return []string{s}, false, err == nil
			}
		case *ast.ParenExpr:
			return acts(v.X)
		case *ast.IndexExpr: // map[bool]string{true: "enable", false: "disable"}[on]
			if cl, ok := v.X.(*ast.CompositeLit); ok {
				if _, ok := cl.Type.(*ast.MapType); ok {
					var out []string
					for _, el := range cl.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							return nil, false, false
						}
						vs, p, ok := acts(kv.Value)
						if !ok || p {
							return nil, false, false
						}
						out = append(out, vs...)
					}
					return out, false, len(out) > 0
				}
			}
		case *ast.BinaryExpr:
			if v.Op != token.ADD {
				break
			}
			l, lp, ok := acts(v.X)
			if !ok || lp {
				return nil, false, false
			}
			r, rp, ok := acts(v.Y)
			if !ok {
				return l, true, true
			}
			var out []string
			for _, a := range l {
				for _, b := range r {
					out = append(out, a+b)
				}
			}
			return out, rp, true
		}
		return nil, false, false
	}
	lit := func(e ast.Expr) (string, bool) {
		if v, ok := e.(*ast.BasicLit); ok && v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			return s, err == nil
		}
		return "", false
	}
	var out []auditRec
	for _, dir := range dirs {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkgs {
			for _, f := range p.Files {
				ast.Inspect(f, func(n ast.Node) bool {
					add := func(key string, e ast.Expr) {
						vs, prefix, ok := acts(e)
						if !ok {
							return
						}
						for _, a := range vs {
							out = append(out, auditRec{key: key, act: a, prefix: prefix, at: fset.Position(e.Pos()).String()})
						}
					}
					switch v := n.(type) {
					case *ast.CallExpr:
						switch fn := v.Fun.(type) {
						case *ast.SelectorExpr:
							switch {
							case p.Name == "web" && (fn.Sel.Name == "audit" || fn.Sel.Name == "auditAs") && len(v.Args) == 4:
								if key, ok := lit(v.Args[1]); ok {
									add(key, v.Args[2])
								}
							case p.Name == "ha" && fn.Sel.Name == "audit" && len(v.Args) == 3:
								add("ha", v.Args[1])
							}
						case *ast.Ident:
							switch {
							case p.Name == "web" && fn.Name == "audit" && len(v.Args) >= 1: // 传进来的 audit 回调:对象由调用方定
								add("", v.Args[0])
							case p.Name == "ha" && fn.Name == "auditTo" && len(v.Args) == 5:
								add("ha", v.Args[3])
							case p.Name == "jobs" && fn.Name == "record" && len(v.Args) == 5:
								if key, ok := lit(v.Args[2]); ok {
									add(key, v.Args[3])
								}
							case p.Name == "rules" && fn.Name == "record" && len(v.Args) == 4:
								add("rule", v.Args[2])
							}
						}
					case *ast.CompositeLit: // model.Change{Key: "ha", Action: "cf-api-override"}
						name := ""
						switch ty := v.Type.(type) {
						case *ast.SelectorExpr:
							name = ty.Sel.Name
						case *ast.Ident:
							name = ty.Name
						}
						if name != "Change" {
							break
						}
						var key string
						var act ast.Expr
						for _, el := range v.Elts {
							if kv, ok := el.(*ast.KeyValueExpr); ok {
								if id, ok := kv.Key.(*ast.Ident); ok {
									switch id.Name {
									case "Key":
										key, _ = lit(kv.Value)
									case "Action":
										act = kv.Value
									}
								}
							}
						}
						if key != "" && act != nil {
							add(key, act)
						}
					}
					return true
				})
			}
		}
	}
	return out
}

func dedupe(xs []string) []string {
	out := xs[:0]
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}
