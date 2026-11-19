package camo

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

var seeds = []uint64{1, 7, 99, 0x5eed1a2b3c4d5e6f, 0x0badc0ffee123457, 0x9e3779b97f4a7c15}

// 每种模板都生成得出完整的站:站名、首页、样式表、站点图标、robots.txt、站内风格的 404;
// 同一种子同一周生成的一模一样(重启不变样);不认识的模板按种子挑一种内置的;整个站不超过 300 KB(不加重面板负担)。
func TestNewTemplates(t *testing.T) {
	for _, seed := range seeds {
		for _, tmpl := range append(append([]string{}, Templates...), "", "auto", "nope") {
			s := New(tmpl, seed, testNow)
			if s.Name() == "" || !known(s.Template()) {
				t.Fatalf("%q/%d: 站名或模板不对: %q %q", tmpl, seed, s.Name(), s.Template())
			}
			home := string(s.Home())
			if !strings.HasPrefix(strings.ToLower(home), "<!doctype html>") || !strings.Contains(home, s.Name()) || !strings.Contains(home, `<link rel="stylesheet" href="`+s.css+`">`) {
				t.Fatalf("%q/%d: 首页不完整: %.300s", tmpl, seed, home)
			}
			for _, p := range []string{"/robots.txt", s.icon, s.css} {
				if _, ok := s.pages[p]; !ok {
					t.Fatalf("%q/%d: 没有 %s", tmpl, seed, p)
				}
			}
			if s.Size() > 300<<10 {
				t.Fatalf("%q/%d: 整个站 %d 字节,太大了", tmpl, seed, s.Size())
			}
			again := New(tmpl, seed, testNow.Add(time.Hour))
			if again.Size() != s.Size() || !bytes.Equal(again.Home(), s.Home()) || len(again.pages) != len(s.pages) {
				t.Fatalf("%q/%d: 同一种子同一周应生成一样的站", tmpl, seed)
			}
		}
	}
	for _, tmpl := range Templates {
		if s := New(tmpl, 7, testNow); s.Template() != tmpl {
			t.Fatalf("指定了 %s,实际 %s", tmpl, s.Template())
		}
	}
}

// 各台服务器的站各不一样:站名、首页标题、样式表路径、站点图标、类名都按种子变(骨架一样的话伪装站本身就成了指纹)。
// 站名:四种站都要(原来只查镜像站、40 个里有 10 种就算过;词库小,实际 40 个里只有 31 种 —— 审查 F-H1)。
func TestSitesDiffer(t *testing.T) {
	for _, tmpl := range Templates {
		names, titles, css, icons, cards := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
		for seed := uint64(1); seed <= 40; seed++ {
			g := newGen(seed*0x9e3779b97f4a7c15, testNow)
			s := New(tmpl, seed*0x9e3779b97f4a7c15, testNow)
			names[s.Name()], css[s.css], cards[g.c("card")] = true, true, true
			titles[find(t, `<title>([^<]*)</title>`, string(s.Home()))] = true
			icons[string(s.pages[s.icon].body)] = true
		}
		if len(names) < 34 || len(titles) < 34 || len(css) < 38 || len(icons) < 36 || len(cards) < 6 {
			t.Fatalf("%s 40 个种子:站名 %d 种、首页标题 %d 种、样式表路径 %d 种、站点图标 %d 种、card 类名 %d 种:各台长得太像", tmpl, len(names), len(titles), len(css), len(icons), len(cards))
		}
	}
}

var linkRe = regexp.MustCompile(` (?:href|src)="([^"]+)"`)

// 站里每一页的站内链接、图片都打得开(镜像的子目录和文件、下载页、视频、文章都在);图片是合法的 SVG,
// 不带脚本、不引外部资源、没有算坏的数(NaN / Inf)。
func TestNoDeadLinksAndCleanImages(t *testing.T) {
	for _, seed := range seeds {
		for _, tmpl := range Templates {
			s := New(tmpl, seed, testNow)
			imgs := 0
			for p, pg := range s.pages {
				if pg.ctype == svgType {
					imgs++
					body := string(pg.body)
					if strings.Contains(body, "NaN") || strings.Contains(body, "Inf") || strings.Contains(body, "<script") || strings.Contains(body, "href=") {
						t.Fatalf("%s/%d %s: 图片里有不该有的东西: %.200s", tmpl, seed, p, body)
					}
					d := xml.NewDecoder(bytes.NewReader(pg.body))
					for {
						if _, err := d.Token(); err == io.EOF {
							break
						} else if err != nil {
							t.Fatalf("%s/%d %s: 不是合法的 SVG: %v", tmpl, seed, p, err)
						}
					}
				}
				if pg.ctype != htmlType {
					continue
				}
				for _, m := range linkRe.FindAllStringSubmatch(string(pg.body), -1) {
					if strings.HasPrefix(m[1], "#") { // 页内锚点(视频页「会员才能看」那一层)
						continue
					}
					if !strings.HasPrefix(m[1], "/") {
						t.Fatalf("%s/%d %s: 链接到站外 %s", tmpl, seed, p, m[1])
					}
					w := httptest.NewRecorder()
					s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, m[1], nil))
					if w.Code != 200 {
						t.Fatalf("%s/%d %s 里的 %s 打不开: %d", tmpl, seed, p, m[1], w.Code)
					}
				}
			}
			want := map[string]int{"mirror": 1, "downloads": 2, "video": 13, "blog": 10}[tmpl] // 软件下载:图标 + 站点图标(界面截图是 HTML)
			if imgs < want {
				t.Fatalf("%s/%d: 只有 %d 张图,至少要 %d 张(视频要有封面、文章要有题图)", tmpl, seed, imgs, want)
			}
		}
	}
}

// 类名都要是合法的 CSS 标识符(不能以数字、连字符加数字开头)。以前「前缀-词」那种写法的前缀约三成以数字开头,
// 那一套样式规则整个失效、页面变成裸 HTML(自审 G1)。
func TestSkinClassNames(t *testing.T) {
	re := regexp.MustCompile(`^(?:[A-Za-z_]|-[A-Za-z_])[A-Za-z0-9_-]*$`)
	for i := uint64(0); i < 2000; i++ {
		g := newGen(i*0x9e3779b97f4a7c15+1, testNow)
		for k, c := range g.sk.cls {
			if !re.MatchString(c) {
				t.Fatalf("第 %d 个种子的类名 %s = %q 不是合法的 CSS 标识符", i, k, c)
			}
		}
		if !re.MatchString(g.sk.kf) {
			t.Fatalf("关键帧名 %q 不合法", g.sk.kf)
		}
	}
}

// 回应:GET / HEAD 给页面(HEAD 不带正文),目录少了结尾斜杠 301,没有的路径站内 404,别的方法 405;
// 进来之前别处加过的头(面板的安全头)一律清掉;ETag 对得上回 304。
func TestServeHTTP(t *testing.T) {
	s := New("downloads", 99, testNow)
	rel := newGen(99, testNow).sk.rel // 各版本那一页的地址(按种子)
	do := func(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		w.Header().Set("X-Frame-Options", "DENY") // 面板的 secureHeaders 先加过
		w.Header().Set("Referrer-Policy", "no-referrer")
		s.ServeHTTP(w, req)
		return w
	}
	w := do(http.MethodGet, "/", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), s.Name()) {
		t.Fatalf("首页: %d %v", w.Code, w.Header())
	}
	if w.Header().Get("X-Frame-Options") != "" || w.Header().Get("Referrer-Policy") != "" {
		t.Fatalf("面板的头没清掉: %v", w.Header())
	}
	etag := w.Header().Get("ETag")
	if etag == "" || w.Header().Get("Last-Modified") == "" {
		t.Fatalf("静态站该有 ETag / Last-Modified: %v", w.Header())
	}
	if w := do(http.MethodGet, "/", map[string]string{"If-None-Match": etag}); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("ETag 对得上应 304: %d", w.Code)
	}
	if w := do(http.MethodHead, "/", nil); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD: %d %d %v", w.Code, w.Body.Len(), w.Header())
	}
	if w := do(http.MethodGet, strings.TrimSuffix(rel, "/"), nil); w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != rel {
		t.Fatalf("目录少了斜杠应 301 到 %s: %d %v", rel, w.Code, w.Header())
	}
	if w := do(http.MethodGet, "/wp-login.php", nil); w.Code != 404 || !strings.Contains(w.Body.String(), "<b>404</b>") || !strings.Contains(w.Body.String(), s.Name()) {
		t.Fatalf("没有的路径给站内风格的 404: %d %.120s", w.Code, w.Body.String())
	}
	if w := do(http.MethodPost, "/", nil); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST 应 405: %d %v", w.Code, w.Header())
	}
	if w := do(http.MethodGet, s.css, nil); w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("样式表: %d %v", w.Code, w.Header())
	}
}

// 设置页预览:样式表、图片都内联进去(沙箱里的 srcdoc 去不了这个站取文件),链接都拿掉。
func TestPreview(t *testing.T) {
	for _, tmpl := range Templates {
		s := New(tmpl, 7, testNow)
		p := string(s.Preview())
		if strings.Contains(p, ` href="`) || strings.Contains(p, `<link rel="stylesheet"`) || !strings.Contains(p, "<style>") {
			t.Fatalf("%s: 预览里还有外链或样式表没内联: %.300s", tmpl, p)
		}
		if strings.Contains(p, ` src="/`) {
			t.Fatalf("%s: 预览里还有没内联的图片", tmpl)
		}
		if tmpl == "video" && strings.Count(p, `src="data:image/svg+xml;base64,`) < 12 {
			t.Fatalf("视频站的预览应带着封面")
		}
	}
}

// 跳转:只写了域名的补 https://;只填网站时带上访问的路径和参数,填到具体页面时一律跳那一页;
// 只认 http / https、要有域名、不能带用户名密码;不带正文,面板的头清掉;怪路径(//evil.com)也还在原来的域名下。
func TestRedirect(t *testing.T) {
	for _, c := range []struct{ in, norm, err string }{
		{"www.example.com", "https://www.example.com", ""},
		{"  https://www.example.com/  ", "https://www.example.com/", ""},
		{"http://example.com/a/b?x=1#top", "http://example.com/a/b?x=1", ""},
		{"ftp://example.com", "", "http://"},
		{"https://", "", "域名"},
		{"https://user:pw@example.com", "", "用户名"},
		{"", "", "http://"},
	} {
		rd, norm, err := ParseRedirect(c.in)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("%q 应报错(%s),实际 %v", c.in, c.err, err)
			}
			continue
		}
		if err != nil || norm != c.norm || rd.Target() != c.norm {
			t.Fatalf("%q: 得到 %q %v,应为 %q", c.in, norm, err, c.norm)
		}
	}
	get := func(target, path string) *httptest.ResponseRecorder {
		rd, _, err := ParseRedirect(target)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		w.Header().Set("X-Frame-Options", "DENY")
		rd.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	for _, c := range []struct{ target, path, loc string }{
		{"https://www.example.com", "/a/b?x=1", "https://www.example.com/a/b?x=1"},
		{"https://www.example.com/", "/", "https://www.example.com/"},
		{"https://www.example.com/landing", "/a/b?x=1", "https://www.example.com/landing"},
		{"https://www.example.com", "//evil.com/x", "https://www.example.com//evil.com/x"},
	} {
		w := get(c.target, c.path)
		if w.Code != http.StatusFound || w.Header().Get("Location") != c.loc || w.Body.Len() != 0 || w.Header().Get("X-Frame-Options") != "" {
			t.Fatalf("%s + %s: %d %q(应 302 %q,不带正文、不带面板的头)", c.target, c.path, w.Code, w.Header().Get("Location"), c.loc)
		}
	}
}

// 来源:没设、设成 nil、来源给 nil 时 Handler 都是 nil,ServeOr404 回普通 404;给了伪装站就回应它。
func TestSourceAndServeOr404(t *testing.T) {
	t.Cleanup(func() { SetSource(nil) })
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		ServeOr404(w, httptest.NewRequest(http.MethodGet, "/", nil))
		return w
	}
	SetSource(nil)
	if Handler() != nil || get().Code != 404 {
		t.Fatal("没设来源时应是普通 404")
	}
	var site http.Handler
	SetSource(func() http.Handler { return site })
	if Handler() != nil || get().Code != 404 {
		t.Fatal("来源给 nil(没开)时应是普通 404")
	}
	s := New("blog", 3, testNow)
	site = s
	if w := get(); w.Code != 200 || !strings.Contains(w.Body.String(), s.Name()) {
		t.Fatalf("开着时应回应伪装站: %d", w.Code)
	}
}

// Sniff 分得清浏览器的请求和协议客户端的二进制数据,看过的字节原样留给之后的读取。
func TestSniff(t *testing.T) {
	for _, c := range []struct {
		data string
		http bool
	}{
		{"GET / HTTP/1.1\r\nHost: a\r\n\r\n", true}, {"HEAD /x HTTP/1.1\r\n\r\n", true}, {"OPTIONS * HTTP/1.1\r\n\r\n", true},
		{"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n", true}, {"\x16\x03\x01\x02\x00\x01\x00\x01\xfc\x03", false}, {strings.Repeat("\x8f", 40), false}, {"GETX / HTTP/1.1\r\n", false},
	} {
		cli, srv := net.Pipe()
		go func() { _, _ = cli.Write([]byte(c.data)); cli.Close() }()
		conn, isHTTP := Sniff(srv)
		got, _ := io.ReadAll(conn)
		srv.Close()
		if isHTTP != c.http || string(got) != c.data {
			t.Fatalf("%q: 判成 %v,读回 %q", c.data, isHTTP, got)
		}
	}
}

// ServeConn 在一条现成的连接上按 HTTP/1.1 回应(保持连接时同一条连接上能连着问),对方断开后返回;不像 HTTP 的回 400。
func TestServeConn(t *testing.T) {
	s := New("video", 5, testNow)
	cli, srv := net.Pipe()
	done := make(chan struct{})
	go func() { ServeConn(srv, s); close(done) }()
	br := bufio.NewReader(cli)
	for _, p := range []string{"/", "/nope"} {
		req, _ := http.NewRequest(http.MethodGet, "http://example.com"+p, nil)
		if err := req.Write(cli); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := 200
		if p != "/" {
			want = 404
		}
		if resp.StatusCode != want || !strings.Contains(string(body), s.Name()) {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
	cli.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("对方断开后 ServeConn 应返回")
	}

	cli, srv = net.Pipe()
	go ServeConn(srv, s)
	go func() { _, _ = cli.Write([]byte(strings.Repeat("\x8f\x01", 40) + "\r\n\r\n")) }()
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, _ := bufio.NewReader(cli).ReadString('\n')
	cli.Close()
	if !strings.HasPrefix(line, "HTTP/1.1 400") {
		t.Fatalf("不像 HTTP 的数据应按网站的样子回 400,实际 %q", line)
	}
}

var paraRe = regexp.MustCompile(`<p>([^<]{40,})</p>`)

// 文字按种子拼:不同服务器上的视频标题、文章段落大多不一样(写死的句子随便搜一句就能找出所有伪装站)。
func TestTextVaries(t *testing.T) {
	titles := map[string]bool{}
	paraSites := map[string]int{}
	for seed := uint64(1); seed <= 20; seed++ {
		sk := newGen(seed*0x9e3779b97f4a7c15, testNow).sk // 视频页、文章的地址前缀按种子
		v := New("video", seed*0x9e3779b97f4a7c15, testNow)
		for _, m := range regexp.MustCompile(`<h1>([^<]+)</h1>`).FindAllStringSubmatch(collectPages(v, sk.watch), -1) {
			titles[m[1]] = true
		}
		b := New("blog", seed*0x9e3779b97f4a7c15, testNow)
		seen := map[string]bool{}
		for _, m := range paraRe.FindAllStringSubmatch(collectPages(b, sk.posts), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				paraSites[m[1]]++
			}
		}
	}
	if len(titles) < 100 {
		t.Fatalf("20 个视频站只有 %d 个不同的标题", len(titles))
	}
	shared := 0
	for _, n := range paraSites {
		if n > 1 {
			shared++
		}
	}
	if len(paraSites) < 300 || shared*10 > len(paraSites) {
		t.Fatalf("20 个博客:%d 个不同的段落,其中 %d 个在不止一个站里出现", len(paraSites), shared)
	}
}

func collectPages(s *Site, prefix string) string {
	var b strings.Builder
	for p, pg := range s.pages {
		if strings.HasPrefix(p, prefix) {
			b.Write(pg.body)
		}
	}
	return b.String()
}

// 线路上交给伪装站的连接有上限(审查 F-G1):同一个来源最多 MaxConnsPerIP 条,总共最多 MaxConns 条,放掉一条才能再进一条;
// IPv6 同一个 /64 算一个来源;release 多调不多减,全放掉以后归零。
func TestAdmit(t *testing.T) {
	oldT, oldP := MaxConns, MaxConnsPerIP
	MaxConns, MaxConnsPerIP = 5, 2
	defer func() { MaxConns, MaxConnsPerIP = oldT, oldP }()
	var rel []func()
	take := func(ip string) bool {
		r, ok := Admit(ip)
		if ok {
			rel = append(rel, r)
		}
		return ok
	}
	if !take("203.0.113.1") || !take("203.0.113.1") || take("203.0.113.1") {
		t.Fatal("同一个来源第三条应放不下")
	}
	if !take("2001:db8:1:2::1") || !take("2001:db8:1:2:ffff::9") || take("2001:db8:1:2::3") {
		t.Fatal("IPv6 同一个 /64 应算一个来源")
	}
	if !take("2001:db8:1:3::1") {
		t.Fatal("别的 /64 是另一个来源")
	}
	if take("198.51.100.7") {
		t.Fatal("总数满了应放不下")
	}
	rel[0]()
	rel[0]() // 多调一次不多减
	if !take("198.51.100.7") || take("198.51.100.8") {
		t.Fatal("放掉一条只能再进一条")
	}
	for _, r := range rel {
		r()
	}
	admitted.mu.Lock()
	n, m := admitted.total, len(admitted.byIP)
	admitted.mu.Unlock()
	if n != 0 || m != 0 {
		t.Fatalf("全放掉以后应归零: 总数 %d,来源 %d 个", n, m)
	}
}

// 页面和接口的回应里没有 fmt 的报错记号。一句话有几种说法,有的用不上参数、有的序号写错,Sprintf 就会在页面上留下
// %!(EXTRA …)、%!f(BADINDEX)(审查 F-H1 改写文字时出过:镜像站流量那一行写成了 %[2].1f)。
// 空字符是表单页里放提示的记号(alertSlot),也不该留在回应里。提交接口各按空着提交回一次。
func TestNoFormatLeftovers(t *testing.T) {
	for i := uint64(1); i <= 150; i++ {
		for _, tmpl := range Templates {
			seed := i*0x9e3779b97f4a7c15 + uint64(len(tmpl))
			s := New(tmpl, seed, testNow)
			check := func(where string, body []byte) {
				if j := bytes.Index(body, []byte("%!")); j >= 0 {
					t.Fatalf("%s/%x %s: 有格式化残留: %q", tmpl, seed, where, body[max(0, j-60):min(len(body), j+60)])
				}
				if bytes.IndexByte(body, 0) >= 0 {
					t.Fatalf("%s/%x %s: 有空字符", tmpl, seed, where)
				}
			}
			for p, pg := range s.pages {
				if !strings.HasPrefix(pg.ctype, "text/css") { // 样式表里有 50%!important 这种写法,不查
					check(p, pg.body)
				}
			}
			check("404 页", s.notFound.body)
			for p, rt := range s.api {
				check(p, rt.body)
				if rt.post == nil {
					continue
				}
				rep := rt.post(map[string]string{})
				check(p+" 的提示", []byte(rep.msg))
				for _, f := range rep.fields {
					check(p+" 的字段提示", []byte(f))
				}
				if rt.form != nil {
					check(p+" 没开脚本时回的页面", rt.form(rep.msg, rep.ok))
				}
			}
		}
	}
}

var (
	bitTagRe   = regexp.MustCompile(`<[^>]+>`)
	bitAttrRe  = regexp.MustCompile(` (?:placeholder|aria-label|alt|title|style)="([^"]+)"`)
	bitSpaceRe = regexp.MustCompile(`\s+`)
	closeRunRe = regexp.MustCompile(`(?:</[a-z0-9]+>\s*)+`)
)

// genericMarkup 到处都有的标记(常见图标库里 <svg> 的写法):算源码片段之前先拿掉
var genericMarkup = []string{`viewBox="0 0 24 24" fill="none" stroke="currentColor"`, `fill="none" viewBox="0 0 24 24"`, `stroke-linecap="round" stroke-linejoin="round"`,
	`xmlns="http://www.w3.org/2000/svg"`}

// worthCounting 够长、值得算的文字:三个词以上,或者十四个字以上(「Sign in」「About」「Email」这类到处都有的短词不算)
func worthCounting(t string) bool { return len(strings.Fields(t)) >= 3 || len([]rune(t)) >= 14 }

// textBits 一页上给人看的文字(按标签切开)和几种属性(placeholder、aria-label、alt、title、内联样式)的值
func textBits(page string) map[string]bool {
	out := map[string]bool{}
	for _, m := range bitAttrRe.FindAllStringSubmatch(page, -1) {
		if v := html.UnescapeString(m[1]); worthCounting(v) {
			out["@"+v] = true
		}
	}
	for _, raw := range bitTagRe.Split(page, -1) {
		if v := strings.TrimSpace(bitSpaceRe.ReplaceAllString(html.UnescapeString(raw), " ")); worthCounting(v) {
			out[v] = true
		}
	}
	return out
}

// 各台的伪装站不共用固定的文字和标记(审查 F-H1)。
//
// 所有 m-ui 的伪装站出自同一份公开的代码。页面上有一句话(或者源码里有一段标记)各台一字不差的话,拿它去搜
// 整页源码的那类搜索引擎里搜一次,就能把所有开着页面伪装的服务器列出来 —— 伪装反而成了记号。原来视频站页脚、
// 404 页、镜像站首页的四句话、下载页的订阅标题等各台完全一样(60 个种子里 100% 相同),内联样式、功能图标的路径也是。
//
// 每种模板 60 个种子:
//   - 首页上任何一段文字(三个词以上或十四个字以上;连同 placeholder、aria-label、内联样式的值)最多一半的站有;
//   - 全站(连同 404 页)任何一段文字最多六成的站有(镜像里真实存在的文件名除外);
//   - 首页 <body> 的源码里任何 48 个字节的片段最多一半的站有(一串收尾的标签、常见图标库的固定写法不算)。
//
// 改文字、加模板时这条挂了:给那句话多写几种说法(text.go 的 say),或者把固定的标记改成按种子变,不要调高门槛。
// 本机看实际数值:go run ./.devtmp/camodump -share 60(同一套算法)。
func TestNoSharedBoilerplate(t *testing.T) {
	const n, k = 60, 48
	for _, tmpl := range Templates {
		home, all, grams := map[string]int{}, map[string]int{}, map[string]int{}
		for i := 0; i < n; i++ {
			s := New(tmpl, uint64(0x9e3779b97f4a7c15)*uint64(i+1)^uint64(len(tmpl))<<40^0x1234567, testNow)
			h := string(s.Home())
			for b := range textBits(h) {
				home[b]++
			}
			seen := map[string]bool{}
			for _, pg := range s.pages {
				if pg.ctype == htmlType {
					for b := range textBits(string(pg.body)) {
						seen[b] = true
					}
				}
			}
			for b := range textBits(string(s.notFound.body)) { // 陌生路径回的那一页:随便探一个不存在的地址拿到的都是它
				seen[b] = true
			}
			for b := range seen {
				all[b]++
			}
			body := h[strings.Index(h, "<body>"):]
			for _, g := range genericMarkup {
				body = strings.ReplaceAll(body, g, "\x01")
			}
			body = closeRunRe.ReplaceAllString(body, "\x02")
			one := map[string]bool{}
			for j := 0; j+k <= len(body); j++ {
				one[body[j:j+k]] = true
			}
			for g := range one {
				grams[g]++
			}
		}
		over := func(m map[string]int, limit int, allow ...string) []string {
			var out []string
		next:
			for b, c := range m {
				if c <= limit {
					continue
				}
				for _, a := range allow {
					if b == a {
						continue next
					}
				}
				out = append(out, fmt.Sprintf("%d%% %q", c*100/n, b))
			}
			sort.Strings(out)
			if len(out) > 12 {
				out = out[:12]
			}
			return out
		}
		if bad := over(home, n/2); len(bad) > 0 {
			t.Errorf("%s:首页上这些文字超过一半的站都有:\n  %s", tmpl, strings.Join(bad, "\n  "))
		}
		if bad := over(all, n*6/10, "SHA256SUMS.sig"); len(bad) > 0 {
			t.Errorf("%s:全站这些文字超过六成的站都有:\n  %s", tmpl, strings.Join(bad, "\n  "))
		}
		if bad := over(grams, n/2); len(bad) > 0 {
			t.Errorf("%s:首页源码里这些片段超过一半的站都有:\n  %s", tmpl, strings.Join(bad, "\n  "))
		}
	}
}
