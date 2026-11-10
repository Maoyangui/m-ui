package web

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"golang.org/x/net/html"
)

// 帮助说明页(assets/help/)放的是写给新手的使用说明,界面上的名字必须和面板一字不差:
// 正文里每个界面名都写成 <b data-k="i18n 键">文字</b>,这里逐个对照 i18n.js 里同语言的原文;
// 只在待合并功能里才有的键要放在 data-pending 范围内(键还不存在时跳过,合并后自动参与核对)。
// data-lit 标的是界面里写死、不走 i18n 的文字(如快速添加的预设名),要在指定的前端文件里原样出现。
func TestHelpPageUINamesMatchI18n(t *testing.T) {
	dicts := helpI18n(t)
	for _, lang := range []string{"zh", "en"} {
		doc := helpDoc(t, lang)
		checked, pending := 0, 0
		helpWalk(doc, nil, func(n *html.Node, anc []*html.Node) {
			if key, ok := attrOf(n, "data-k"); ok {
				text := nodeText(n)
				want, found := dicts[lang][key]
				switch {
				case !found && helpPending(n, anc):
					pending++
				case !found:
					t.Errorf("%s.html:界面名「%s」对应的键 %s 在 i18n.js 里没有", lang, text, key)
				case text != want:
					t.Errorf("%s.html:键 %s 写成了「%s」,界面上是「%s」", lang, key, text, want)
				default:
					checked++
				}
			}
			if src, ok := attrOf(n, "data-lit"); ok {
				b, err := fs.ReadFile(assets, "assets/"+src)
				if err != nil {
					t.Errorf("%s.html:data-lit 指向的 %s 读不到:%v", lang, src, err)
				} else if !bytes.Contains(b, []byte(nodeText(n))) {
					t.Errorf("%s.html:「%s」不在 %s 里", lang, nodeText(n), src)
				}
			}
		})
		if checked < 300 {
			t.Errorf("%s.html 只核对到 %d 个界面名,解析可能出了问题", lang, checked)
		}
		t.Logf("%s.html:核对 %d 个界面名,%d 个属于待合并功能、键还不存在", lang, checked, pending)
	}
}

// 页内链接都有落点;管理员视图和代理视图(?view=reseller)里看得见的链接都不能指向这个视图里藏起来的内容;
// 中英两份的章节 id、顺序与视图归属完全一致(切换语言时停在同一节);不加载任何外部资源。
func TestHelpPageStructure(t *testing.T) {
	var outline [2][]string
	for i, lang := range []string{"zh", "en"} {
		doc := helpDoc(t, lang)
		ids := map[string][]*html.Node{} // id → 祖先链(含自己)
		type link struct {
			href  string
			chain []*html.Node
		}
		var links []link
		helpWalk(doc, nil, func(n *html.Node, anc []*html.Node) {
			chain := append(append([]*html.Node{}, anc...), n)
			if id, ok := attrOf(n, "id"); ok {
				if _, dup := ids[id]; dup {
					t.Errorf("%s.html:id %s 重复", lang, id)
				}
				ids[id] = chain
			}
			if href, ok := attrOf(n, "href"); ok && strings.HasPrefix(href, "#") && href != "#top" {
				links = append(links, link{href, chain})
			}
			if cls, _ := attrOf(n, "class"); n.Data == "section" && (cls == "chap" || cls == "sec") {
				id, _ := attrOf(n, "id")
				if id == "" {
					t.Errorf("%s.html:有章节没有 id", lang)
				}
				scope, _ := attrOf(n, "data-scope")
				outline[i] = append(outline[i], id+":"+scope)
			}
			for _, a := range []string{"src", "href"} {
				v, _ := attrOf(n, a)
				if (n.Data == "script" || n.Data == "link" || n.Data == "img") && helpExternal.MatchString(v) {
					t.Errorf("%s.html 加载了外部资源:%s", lang, v)
				}
			}
		})
		for _, l := range links {
			target, ok := ids[strings.TrimPrefix(l.href, "#")]
			if !ok {
				t.Errorf("%s.html:链接 %s 没有落点", lang, l.href)
				continue
			}
			for _, view := range []string{"admin", "reseller"} {
				if helpVisible(l.chain, view) && !helpVisible(target, view) {
					t.Errorf("%s.html:%s 视图里链接 %s 指向看不见的内容", lang, view, l.href)
				}
			}
		}
	}
	if strings.Join(outline[0], ",") != strings.Join(outline[1], ",") {
		t.Errorf("中英两份帮助的章节不一致\nzh: %v\nen: %v", outline[0], outline[1])
	}
	if len(outline[0]) < 60 {
		t.Errorf("章节太少(%d),解析可能出了问题", len(outline[0]))
	}
	for _, f := range []string{"help/help.css", "help/help.js"} {
		b, err := fs.ReadFile(assets, "assets/"+f)
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`(?i)(url\(\s*['"]?(https?:)?//|@import|fetch\(|XMLHttpRequest)`).Match(b) {
			t.Errorf("%s 不能请求外部资源或网络", f)
		}
	}
}

// 面板右上角的"帮助说明"按页面定位到章节(app.js 的 HELP_ANCHOR / HELP_ANCHOR_RESELLER):
// 每个落点在两份帮助里都要有,代理面板的落点还要在代理视图里看得见。
func TestHelpAnchorsFromPanel(t *testing.T) {
	js, err := fs.ReadFile(assets, "assets/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, view string }{{"HELP_ANCHOR", "admin"}, {"HELP_ANCHOR_RESELLER", "reseller"}} {
		m := regexp.MustCompile(`const ` + c.name + ` = \{([^}]*)\}`).FindSubmatch(js)
		if m == nil {
			t.Fatalf("app.js 里找不到 %s", c.name)
		}
		pairs := regexp.MustCompile(`(\w+): '([\w-]+)'`).FindAllSubmatch(m[1], -1)
		if len(pairs) < 4 {
			t.Fatalf("%s 解析出的页面太少", c.name)
		}
		for _, lang := range []string{"zh", "en"} {
			doc := helpDoc(t, lang)
			chains := map[string][]*html.Node{}
			helpWalk(doc, nil, func(n *html.Node, anc []*html.Node) {
				if id, ok := attrOf(n, "id"); ok {
					chains[id] = append(append([]*html.Node{}, anc...), n)
				}
			})
			for _, p := range pairs {
				chain, ok := chains[string(p[2])]
				if !ok {
					t.Errorf("%s.html 没有 %s 页对应的章节 #%s", lang, p[1], p[2])
				} else if !helpVisible(chain, c.view) {
					t.Errorf("%s.html:%s 页的落点 #%s 在%s视图里看不见", lang, p[1], p[2], c.view)
				}
			}
		}
	}
	if !strings.Contains(string(js), "'help.btn'") {
		t.Error("app.js 没用 help.btn 文案")
	}
}

// 帮助页是普通的内嵌静态文件:和面板其它前端文件一样经 assetCache 提供(带内容哈希 ETag、要求每次校验),
// 帮助页内容一变 ETag 就变,升级后浏览器不会看到旧说明。
func TestHelpPageServedWithAssets(t *testing.T) {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		t.Fatal(err)
	}
	serve := func(fsys fs.FS, path, inm string) *httptest.ResponseRecorder {
		h := assetCache(fsys, http.StripPrefix("/app", http.FileServer(http.FS(fsys))))
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if inm != "" {
			r.Header.Set("If-None-Match", inm)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	var etag string
	for _, p := range []string{"/app/help/zh.html", "/app/help/en.html", "/app/help/help.css", "/app/help/help.js", "/app/help/"} {
		w := serve(sub, p, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s 应 200,得 %d", p, w.Code)
		}
		if etag = w.Header().Get("ETag"); etag == "" || w.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s 缺 ETag 或 no-cache", p)
		}
		if w2 := serve(sub, p, etag); w2.Code != http.StatusNotModified {
			t.Fatalf("%s 带 If-None-Match 应 304,得 %d", p, w2.Code)
		}
	}
	if ct := serve(sub, "/app/help/zh.html", "").Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("帮助页的 Content-Type 不对:%s", ct)
	}
	// 只改帮助页的一个字,整套资源的 ETag 就要变
	copyFS := fstest.MapFS{}
	if err := fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(sub, p)
		copyFS[p] = &fstest.MapFile{Data: b}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	copyFS["help/zh.html"].Data = append(append([]byte{}, copyFS["help/zh.html"].Data...), ' ')
	if got := serve(copyFS, "/app/help/zh.html", "").Header().Get("ETag"); got == etag {
		t.Fatal("帮助页改了,ETag 却没变")
	}
}

// ---- 小工具 ----

var helpExternal = regexp.MustCompile(`^(?i)(https?:)?//`)

func helpDoc(t *testing.T, lang string) *html.Node {
	t.Helper()
	b, err := fs.ReadFile(assets, "assets/help/"+lang+".html")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// helpWalk 深度优先走一遍元素,回调里带上祖先链(不含自己)。
func helpWalk(n *html.Node, anc []*html.Node, fn func(n *html.Node, anc []*html.Node)) {
	if n.Type == html.ElementNode {
		fn(n, anc)
		anc = append(anc, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		helpWalk(c, anc, fn)
	}
}

func attrOf(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func helpPending(n *html.Node, anc []*html.Node) bool {
	for _, x := range append(append([]*html.Node{}, anc...), n) {
		if _, ok := attrOf(x, "data-pending"); ok {
			return true
		}
	}
	return false
}

// helpVisible 这条祖先链(含元素自己)在某个视图里看不看得见:管理员视图藏 data-scope="reseller",代理视图藏 data-scope="admin"。
func helpVisible(chain []*html.Node, view string) bool {
	hidden := map[string]string{"admin": "reseller", "reseller": "admin"}[view]
	for _, x := range chain {
		if s, ok := attrOf(x, "data-scope"); ok {
			for _, f := range strings.Fields(s) {
				if f == hidden {
					return false
				}
			}
		}
	}
	return true
}

// helpI18n 从 i18n.js 取出 zh / en 两个字典:字典里只有字符串字面量的"键: 值"对,按出现顺序两两配对即可
// (单双引号都有,转义只有 \' \" \\ \n 这几种)。
func helpI18n(t *testing.T) map[string]map[string]string {
	t.Helper()
	b, err := fs.ReadFile(assets, "assets/js/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	out := map[string]map[string]string{}
	for _, name := range []string{"zh", "en"} {
		head := "const " + name + " = {"
		i := strings.Index(src, head)
		if i < 0 {
			t.Fatalf("i18n.js 里找不到 %s", head)
		}
		j := strings.Index(src[i:], "\n};")
		if j < 0 {
			t.Fatalf("i18n.js 的 %s 字典没有结尾", name)
		}
		body := src[i+len(head) : i+j]
		var lits []string
		for k := 0; k < len(body); k++ {
			q := body[k]
			if q != '\'' && q != '"' {
				continue
			}
			var s strings.Builder
			for k++; k < len(body) && body[k] != q; k++ {
				if body[k] == '\\' && k+1 < len(body) {
					k++
					if body[k] == 'n' {
						s.WriteByte('\n')
					} else {
						s.WriteByte(body[k])
					}
					continue
				}
				s.WriteByte(body[k])
			}
			lits = append(lits, s.String())
		}
		if len(lits)%2 != 0 || len(lits) < 2000 {
			t.Fatalf("i18n.js 的 %s 字典解析出 %d 个字符串,格式可能变了", name, len(lits))
		}
		m := map[string]string{}
		for k := 0; k < len(lits); k += 2 {
			m[lits[k]] = lits[k+1]
		}
		out[name] = m
	}
	return out
}
