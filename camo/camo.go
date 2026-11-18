// Package camo 页面伪装(用户 N2):有人直接打开订阅 / 面板端口的根目录、陌生路径,或者用浏览器连 AnyTLS / Trojan / Hysteria2
// 线路时,看到的是一个普通的网站(或者跳转到管理员自己的网站),而不是 404 或连上就断。
//
// 伪装站用内置模板(下载镜像、软件下载、视频站、个人站),每台服务器按自己的随机种子生成站名、配色、图片、内容,
// 连类名、静态文件目录、版式都按种子换 —— 所有 m-ui 长得一样(哪怕只是骨架一样)的话,伪装站本身就成了指纹。
// 内容(页面、样式表、按种子画的矢量图)在内存里生成一次,之后只是回应:不读磁盘、不连外网、不开新进程,没人来时不占资源。
// 响应头只带普通静态站会带的几项,不带面板的安全头,也不冒充别的服务器软件。
//
// 局限(帮助页里写着):只对主动探测有用,不改变流量特征。
package camo

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Templates 内置模板。auto(或空)按种子挑一个。
var Templates = []string{"mirror", "downloads", "video", "blog"}

// page 一页生成好的内容(页面、样式表、图片都是)
type page struct {
	body  []byte
	ctype string
	etag  string
}

// Site 一个生成好的伪装站(只读,可并发回应)。
type Site struct {
	template string
	name     string
	css      string // 样式表的路径
	icon     string // 站点图标的路径
	logo     string // 顶栏站名前面那一小块(HTML)
	pages    map[string]page
	notFound page
	modTime  time.Time
}

// New 按模板和种子生成伪装站。template 不认识或为空时按种子挑;now 决定页面上的日期(按周,见 newGen)。
func New(template string, seed uint64, now time.Time) *Site {
	g := newGen(seed, now)
	t := strings.ToLower(strings.TrimSpace(template))
	if !known(t) {
		t = Templates[seed%uint64(len(Templates))]
	}
	s := &Site{template: t, pages: map[string]page{}, modTime: g.modTime(), css: g.sk.css, icon: "/favicon.svg"}
	s.add(s.css, "text/css; charset=utf-8", g.css())
	s.add(s.icon, svgType, favicon(g, t == "blog"))
	switch t {
	case "mirror":
		genMirror(s, g)
	case "downloads":
		genDownloads(s, g)
	case "video":
		genVideo(s, g)
	default:
		genBlog(s, g)
	}
	s.add("/robots.txt", textType, []byte("User-agent: *\nDisallow:\n"))
	return s
}

func known(t string) bool {
	for _, x := range Templates {
		if x == t {
			return true
		}
	}
	return false
}

// Template 实际用的模板(auto 时是按种子挑出来的那个)。
func (s *Site) Template() string { return s.template }

// Name 站名(设置页预览用)。
func (s *Site) Name() string { return s.name }

// Home 首页的 HTML。
func (s *Site) Home() []byte { return s.pages["/"].body }

// Size 整个站在内存里占多少字节(页面、样式表、图片加起来)。
func (s *Site) Size() int {
	n := len(s.notFound.body)
	for _, p := range s.pages {
		n += len(p.body)
	}
	return n
}

var srcRe = regexp.MustCompile(` src="(/[^"]+)"`)

// Preview 设置页预览用的首页:样式表和图片直接内联进去(预览框是沙箱里的 srcdoc,去不了这个站取文件),
// 链接拿掉(预览框里点了不会跳到面板自己的地址上去)。
func (s *Site) Preview() []byte {
	h := string(s.Home())
	if css, ok := s.pages[s.css]; ok {
		h = strings.Replace(h, `<link rel="stylesheet" href="`+s.css+`">`, "<style>"+string(css.body)+"</style>", 1)
	}
	h = srcRe.ReplaceAllStringFunc(h, func(m string) string {
		p, ok := s.pages[srcRe.FindStringSubmatch(m)[1]]
		if !ok || p.ctype != svgType {
			return m
		}
		return ` src="data:image/svg+xml;base64,` + base64.StdEncoding.EncodeToString(p.body) + `"`
	})
	return []byte(strings.ReplaceAll(h, ` href="`, ` data-href="`))
}

func (s *Site) add(path, ctype string, body []byte) {
	sum := sha256.Sum256(body)
	s.pages[path] = page{body: body, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
}

func (s *Site) setNotFound(body []byte) {
	sum := sha256.Sum256(body)
	s.notFound = page{body: body, ctype: htmlType, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
}

// clearHeaders 进来之前别处加过的头(面板的安全头之类)一律清掉:伪装站不该带面板的特征
func clearHeaders(w http.ResponseWriter) {
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
}

// ServeHTTP 回应伪装站:GET / HEAD 按路径给页面(目录路径少了结尾斜杠就 301 补上),没有的给站内风格的 404,别的方法 405。
func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clearHeaders(w)
	h := w.Header()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		s.write(w, r, http.StatusMethodNotAllowed, s.notFound, false)
		return
	}
	p := r.URL.Path
	if p == "" {
		p = "/"
	}
	if pg, ok := s.pages[p]; ok {
		s.write(w, r, http.StatusOK, pg, true)
		return
	}
	if !strings.HasSuffix(p, "/") {
		if _, ok := s.pages[p+"/"]; ok {
			h.Set("Location", p+"/")
			h.Set("Content-Length", "0")
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
	}
	s.write(w, r, http.StatusNotFound, s.notFound, false)
}

func (s *Site) write(w http.ResponseWriter, r *http.Request, code int, pg page, cache bool) {
	h := w.Header()
	h.Set("Content-Type", pg.ctype)
	if cache {
		h.Set("Last-Modified", s.modTime.UTC().Format(http.TimeFormat))
		h.Set("ETag", pg.etag)
		h.Set("Cache-Control", "public, max-age=3600")
		if r.Header.Get("If-None-Match") == pg.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(pg.body)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = w.Write(pg.body)
	}
}

// ---- 跳转到管理员自己的网站 ----

// Redirect 伪装方式「跳转到我的网站」:所有请求一律 302 到管理员填的网址。只填了网站(没有路径)时把访问的路径和参数带过去,
// 填到具体页面时一律跳到那一页。不带正文、不带别的头,和常见的「这台只做跳转」的服务器一样。
type Redirect struct {
	u        url.URL
	keepPath bool
}

var (
	errRedirectScheme = errors.New("跳转的网址要以 http:// 或 https:// 开头,比如 https://www.example.com")
	errRedirectHost   = errors.New("跳转的网址里没有域名")
	errRedirectUser   = errors.New("跳转的网址里不能带用户名或密码")
)

// ParseRedirect 校验管理员填的跳转网址,返回处理器和规整后的写法(去掉首尾空白和 #片段;只写了域名的补上 https://)。
func ParseRedirect(raw string) (*Redirect, string, error) {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", errRedirectScheme
	}
	if u.Hostname() == "" {
		return nil, "", errRedirectHost
	}
	if u.User != nil {
		return nil, "", errRedirectUser
	}
	u.Fragment, u.RawFragment = "", ""
	rd := &Redirect{u: *u, keepPath: (u.Path == "" || u.Path == "/") && u.RawQuery == ""}
	return rd, u.String(), nil
}

// Target 跳到哪(规整后的写法)。
func (rd *Redirect) Target() string { return rd.u.String() }

func (rd *Redirect) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clearHeaders(w)
	loc := rd.u
	if rd.keepPath {
		loc.Path, loc.RawPath, loc.RawQuery = r.URL.Path, r.URL.RawPath, r.URL.RawQuery
		if !strings.HasPrefix(loc.Path, "/") {
			loc.Path, loc.RawPath = "/"+loc.Path, ""
		}
	}
	w.Header().Set("Location", loc.String())
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusFound)
}

// ---- 当前的伪装站:数据面里各入站、订阅 / 面板端口从这里取 ----

// Source 返回现在要用的伪装站(或跳转);没开页面伪装时返回 nil(调用方照旧回 404 / 断开)。
type Source func() http.Handler

var current atomic.Pointer[Source]

// SetSource 由 runner 在启动时设一次。
func SetSource(f Source) { current.Store(&f) }

// Handler 现在要用的伪装站;没开(或没设来源)时为 nil。
func Handler() http.Handler {
	if p := current.Load(); p != nil && *p != nil {
		return (*p)()
	}
	return nil
}

// ServeOr404 开着页面伪装就回应伪装站,没开回普通 404(订阅 / 面板 / 代理面板端口的根目录和陌生路径用)。
func ServeOr404(w http.ResponseWriter, r *http.Request) {
	if h := Handler(); h != nil {
		h.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

// ---- 认证没过的连接(AnyTLS / Trojan) ----

var httpMethods = [][]byte{[]byte("GET "), []byte("HEAD "), []byte("POST "), []byte("PUT "), []byte("DELETE "), []byte("OPTIONS "),
	[]byte("PATCH "), []byte("CONNECT "), []byte("TRACE "), []byte("PRI * ")}

// LooksHTTP 这几个字节像不像一个 HTTP 请求的开头(请求方法后跟空格)。
func LooksHTTP(b []byte) bool {
	for _, m := range httpMethods {
		if bytes.HasPrefix(b, m) {
			return true
		}
	}
	return false
}

// Sniff 看连接上最先到的几个字节像不像 HTTP 请求,读出的字节原样留给之后的读取。认证没过的连接靠它分辨:
// 像 HTTP 的多半是浏览器、扫描器;不像的多半是协议客户端填错了密码(日志里照旧记原来的原因)。最多等 3 秒。
func Sniff(conn net.Conn) (net.Conn, bool) {
	br := bufio.NewReaderSize(conn, 2048)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, _ := br.Peek(8)
	_ = conn.SetReadDeadline(time.Time{})
	return &peekedConn{Conn: conn, r: br}, LooksHTTP(b)
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// ServeConn 把 conn 当成一条 HTTP/1.1 连接回应伪装站,连接关掉(对方断开、空闲超时)才返回。
// 读写都有时限:探测者开着连接不发东西,最多占 15 秒。不像 HTTP 的数据按网站的样子回 400。
func ServeConn(conn net.Conn, h http.Handler) {
	l := &oneConn{c: &doneConn{Conn: conn, done: make(chan struct{})}}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	_ = srv.Serve(l)
}

// oneConn 只交出一条连接的监听器:第二次 Accept 等到这条连接关了才返回,Serve 随之返回。
type oneConn struct {
	c    *doneConn
	once sync.Once
}

func (l *oneConn) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.c })
	if c != nil {
		return c, nil
	}
	<-l.c.done
	return nil, net.ErrClosed
}
func (l *oneConn) Close() error   { return nil }
func (l *oneConn) Addr() net.Addr { return l.c.LocalAddr() }

type doneConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *doneConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

// PageCount 测试 / 预览工具用:站里一共几个文件。
func PageCount(s *Site) int { return len(s.pages) }
