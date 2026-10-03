package web

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// 英文界面靠 assets/js/errmsg.js 把后端中文错误翻成英文(审计 MB08)。这里扫一遍会把错误送到面板的包:
// 每条带中文的错误文案(errors.New / fmt.Errorf / "error": … / 拼接的前缀)都要在表里有译文、占位个数一致,
// 表里也不能留源码里已经没有的条目;数据面重载的操作名(noteReload)要在 LABELS 里。
func TestErrorMessagesHaveEnglish(t *testing.T) {
	src, ops := backendMessages(t, ".", "../hop", "../upstream", "../ext", "../rules", "../creds", "../ops", "../selfupdate",
		"../acme", "../reach", "../render", "../runner", "../hub", "../certutil", "../backup", "../importer", "../jobs",
		"../monitor", "../stats", "../totp", "../database")
	js, err := os.ReadFile("assets/js/errmsg.js")
	if err != nil {
		t.Fatal(err)
	}
	en := jsTable(t, string(js), "/*BEGIN*/")
	labels := jsTable(t, string(js), "/*LABELS*/")

	verb := regexp.MustCompile(`%(?:\[\d+\])?[-+# 0-9.]*[dsvqwxf]`)
	var missing, stale, mismatch []string
	for k, at := range src {
		v, ok := en[k]
		if !ok {
			missing = append(missing, strconv.Quote(k)+"  ("+at+")")
		} else if len(verb.FindAllString(k, -1)) != len(verb.FindAllString(v, -1)) {
			mismatch = append(mismatch, strconv.Quote(k)+" → "+strconv.Quote(v))
		}
	}
	for k := range en {
		if _, ok := src[k]; !ok {
			stale = append(stale, strconv.Quote(k))
		}
	}
	for op := range ops {
		if _, ok := labels[op]; !ok {
			missing = append(missing, "LABELS "+strconv.Quote(op))
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing)+len(stale)+len(mismatch) > 0 {
		t.Fatalf("errmsg.js 与后端错误文案不同步\n缺译文:\n  %s\n源码里已没有:\n  %s\n占位个数不一致:\n  %s",
			strings.Join(missing, "\n  "), strings.Join(stale, "\n  "), strings.Join(mismatch, "\n  "))
	}
}

// jsTable 取 errmsg.js 里 marker 与 /*END*/ 之间的 JSON 对象。
func jsTable(t *testing.T, js, marker string) map[string]string {
	t.Helper()
	i := strings.Index(js, marker)
	j := strings.Index(js[i+1:], "/*END*/")
	if i < 0 || j < 0 {
		t.Fatalf("errmsg.js 里找不到 %s … /*END*/", marker)
	}
	m := map[string]string{}
	dec := json.NewDecoder(strings.NewReader(js[i+len(marker) : i+1+j]))
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("errmsg.js 的 %s 不是合法 JSON: %v", marker, err)
	}
	return m
}

// backendMessages 返回各包里带中文的错误文案(键 → 第一次出现的位置)与 noteReload 的操作名。
func backendMessages(t *testing.T, dirs ...string) (map[string]string, map[string]bool) {
	t.Helper()
	cjk := func(s string) bool {
		for _, r := range s {
			if unicode.Is(unicode.Han, r) {
				return true
			}
		}
		return false
	}
	var keyOf func(e ast.Expr) (string, bool)
	keyOf = func(e ast.Expr) (string, bool) {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				s, err := strconv.Unquote(v.Value)
				return s, err == nil
			}
		case *ast.BinaryExpr: // "前缀: " + err.Error() 记作 "前缀: %w"
			if v.Op == token.ADD {
				l, ok := keyOf(v.X)
				if !ok {
					return "", false
				}
				if r, ok := keyOf(v.Y); ok {
					return l + r, true
				}
				return l + "%w", true
			}
		case *ast.ParenExpr:
			return keyOf(v.X)
		}
		return "", false
	}
	msgs, ops := map[string]string{}, map[string]bool{}
	for _, dir := range dirs {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkgs {
			for _, f := range p.Files {
				ast.Inspect(f, func(n ast.Node) bool {
					add := func(e ast.Expr) {
						if k, ok := keyOf(e); ok && cjk(k) {
							if _, seen := msgs[k]; !seen {
								msgs[k] = fset.Position(e.Pos()).String()
							}
						}
					}
					switch v := n.(type) {
					case *ast.CallExpr:
						name := ""
						switch fn := v.Fun.(type) {
						case *ast.SelectorExpr:
							if x, ok := fn.X.(*ast.Ident); ok {
								name = x.Name + "." + fn.Sel.Name
							} else {
								name = "." + fn.Sel.Name
							}
						case *ast.Ident:
							name = fn.Name
						}
						switch {
						case name == "errors.New" || name == "fmt.Errorf" || name == "updateErr":
							if len(v.Args) > 0 {
								add(v.Args[0])
							}
						case name == "http.Error" && len(v.Args) > 1:
							add(v.Args[1])
						case strings.HasSuffix(name, ".noteReload") && len(v.Args) > 0:
							if k, ok := keyOf(v.Args[0]); ok {
								ops[k] = true
							}
						}
					case *ast.KeyValueExpr:
						if k, ok := keyOf(v.Key); ok && k == "error" {
							add(v.Value)
						}
					case *ast.CompositeLit:
						if id, ok := v.Type.(*ast.Ident); ok && id.Name == "keyErr" && len(v.Elts) > 0 {
							if kv, ok := v.Elts[0].(*ast.KeyValueExpr); ok {
								add(kv.Value)
							} else {
								add(v.Elts[0])
							}
						}
					}
					return true
				})
			}
		}
	}
	return msgs, ops
}
