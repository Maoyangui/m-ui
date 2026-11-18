package camo

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
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
			if !strings.HasPrefix(home, "<!doctype html>") || !strings.Contains(home, s.Name()) || !strings.Contains(home, `<link rel="stylesheet" href="`+s.css+`">`) {
				t.Fatalf("%q/%d: 首页不完整: %.300s", tmpl, seed, home)
			}
			for _, p := range []string{"/robots.txt", "/favicon.svg", s.css} {
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

// 各台服务器的站各不一样:站名、样式表路径、类名都按种子变(骨架一样的话伪装站本身就成了指纹)。
func TestSitesDiffer(t *testing.T) {
	names, css, cards := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for seed := uint64(1); seed <= 40; seed++ {
		g := newGen(seed*0x9e3779b97f4a7c15, testNow)
		s := New("mirror", seed*0x9e3779b97f4a7c15, testNow)
		names[s.Name()], css[s.css], cards[g.c("card")] = true, true, true
	}
	if len(names) < 10 || len(css) < 38 || len(cards) < 6 {
		t.Fatalf("40 个种子:站名 %d 种、样式表路径 %d 种、card 类名 %d 种:各台长得太像", len(names), len(css), len(cards))
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

// 回应:GET / HEAD 给页面(HEAD 不带正文),目录少了结尾斜杠 301,没有的路径站内 404,别的方法 405;
// 进来之前别处加过的头(面板的安全头)一律清掉;ETag 对得上回 304。
func TestServeHTTP(t *testing.T) {
	s := New("downloads", 99, testNow)
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
	if w := do(http.MethodGet, "/releases", nil); w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/releases/" {
		t.Fatalf("目录少了斜杠应 301 到 /releases/: %d %v", w.Code, w.Header())
	}
	if w := do(http.MethodGet, "/wp-login.php", nil); w.Code != 404 || !strings.Contains(w.Body.String(), "Page not found") || !strings.Contains(w.Body.String(), s.Name()) {
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
		v := New("video", seed*0x9e3779b97f4a7c15, testNow)
		for _, m := range regexp.MustCompile(`<title>([^<]+) - `).FindAllStringSubmatch(string(v.Home())+collectPages(v, "/watch/"), -1) {
			titles[m[1]] = true
		}
		b := New("blog", seed*0x9e3779b97f4a7c15, testNow)
		seen := map[string]bool{}
		for _, m := range paraRe.FindAllStringSubmatch(collectPages(b, "/posts/"), -1) {
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
