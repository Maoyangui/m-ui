// Package camo 页面伪装(用户 N2):有人直接打开订阅 / 面板端口的根目录、陌生路径,或者用浏览器连 AnyTLS / Trojan / Hysteria2
// 线路时,看到的是一个普通的网站(或者跳转到管理员自己的网站),而不是 404 或连上就断。
//
// 伪装站用内置模板(下载镜像、软件下载、视频站、个人站),每台服务器按自己的随机种子生成站名、配色、图片、内容,
// 连类名、静态文件目录、版式、脚本里的接口路径都按种子换 —— 所有 m-ui 长得一样(哪怕只是骨架一样)的话,伪装站本身就成了指纹。
// 页面上给人看的每一句话、源码里的标记写法也都按种子变:各台有一句话(或一段标记)一字不差,拿它去搜就能列出全部伪装站
// (审查 F-H1;门槛见 TestNoSharedBoilerplate)。做不到的:页面结构、样式表和脚本的形状各台一样,对着这份代码逐台比仍可能认出来。
// 内容(页面、样式表、脚本、按种子画的矢量图,有图包照片时是处理好的照片)在内存里生成一次,之后只是回应:
// 回应时不读磁盘、不连外网、不开新进程,没人来时不占资源。照片由 runner 按需取下来、处理好再交进来(见 media.go)。
// 视频站有登录 / 注册(要邀请码)/ 找回密码,几种站各有几个像真站的接口(api.go):只回固定内容,提交的东西不存不记。
// 响应头只带普通站会带的几项,不带面板的安全头,也不冒充别的服务器软件。
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
	"net/netip"
	"net/url"
	"regexp"
	"sort"
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
	desc     string // <meta name="description">
	css      string // 样式表的路径
	js       string // 脚本的路径(没有脚本的站为空)
	icon     string // 站点图标的路径
	logo     string // 顶栏站名前面那一小块(HTML)
	head     string // <head> 里多加的(个人站的 RSS 链接)
	cta      string // 顶栏最右边的按钮(视频站的「登录」)
	foot     string // 页脚右边那一句
	pages    map[string]page
	notFound page
	modTime  time.Time

	photos Photos            // 处理好的照片(可以为 nil)
	used   map[string]bool   // 这一站用到的照片(不管处理好没有)
	wanted []PhotoReq        // 用到了、还没处理好的(这些位置先画插图)
	small  map[string][]byte // 图片地址 → 设置页预览里内联的小图

	api      map[string]*apiRoute // 假接口(api.go)
	apiPre   string               // 接口前缀:以它开头、又没对上的路径回 JSON 的 404
	errStyle int                  // 接口报错的写法(种子定)
	apiText  apiTexts             // 接口那几句通用的报错(种子定)
	cache    string               // 页面的 Cache-Control(种子定)
}

// New 按模板和种子生成伪装站(只用画的插图)。template 不认识或为空时按种子挑;now 决定页面上的日期(按周,见 newGen)。
func New(template string, seed uint64, now time.Time) *Site { return Build(template, seed, now, nil) }

// Build 同 New,photos 里有处理好的照片的封面位就用照片(没有的照旧画插图,并记进 Wanted)。
func Build(template string, seed uint64, now time.Time, photos Photos) *Site {
	g := newGen(seed, now)
	t := strings.ToLower(strings.TrimSpace(template))
	if !known(t) {
		t = Templates[seed%uint64(len(Templates))]
	}
	s := &Site{template: t, pages: map[string]page{}, modTime: g.modTime(), css: g.sk.css, icon: g.sk.icon,
		photos: photos, used: map[string]bool{}, small: map[string][]byte{}, api: map[string]*apiRoute{}, errStyle: g.sk.errStyle,
		apiText: newAPITexts(g), cache: g.sk.cache}
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
	s.add("/robots.txt", textType, []byte(g.sk.robots))
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

// Size 整个站在内存里占多少字节(页面、样式表、图片加起来,不算预览用的小图)。
func (s *Site) Size() int {
	n := len(s.notFound.body)
	for _, p := range s.pages {
		n += len(p.body)
	}
	return n
}

// Wanted 这一站用到、还没处理好的照片(每个尺寸一项;按出现顺序,可能有重复的名字)。
func (s *Site) Wanted() []PhotoReq { return s.wanted }

// PhotoNames 这一站用到的照片(去重、排好序;不管处理好没有)。
func (s *Site) PhotoNames() []string {
	out := make([]string, 0, len(s.used))
	for n := range s.used {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

var (
	srcRe    = regexp.MustCompile(` src="(/[^"]+)"`)
	scriptRe = regexp.MustCompile(`<script\b[^>]*></script>`)
)

// Preview 设置页预览用的首页:样式表和图片直接内联进去(预览框是沙箱里的 srcdoc,去不了这个站取文件;
// 照片换成小图,免得预览动辄几百 KB),脚本和链接拿掉(预览框里点了不会跳到面板自己的地址上去)。
func (s *Site) Preview() []byte {
	h := string(s.Home())
	if css, ok := s.pages[s.css]; ok {
		h = strings.Replace(h, `<link rel="stylesheet" href="`+s.css+`">`, "<style>"+string(css.body)+"</style>", 1)
	}
	h = srcRe.ReplaceAllStringFunc(h, func(m string) string {
		path := srcRe.FindStringSubmatch(m)[1]
		if b, ok := s.small[path]; ok {
			return ` src="data:image/jpeg;base64,` + base64.StdEncoding.EncodeToString(b) + `"`
		}
		p, ok := s.pages[path]
		if !ok || p.ctype != svgType {
			return m
		}
		return ` src="data:image/svg+xml;base64,` + base64.StdEncoding.EncodeToString(p.body) + `"`
	})
	h = scriptRe.ReplaceAllString(h, "")
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

// ServeHTTP 回应伪装站:先看是不是接口(api.go);GET / HEAD 按路径给页面(目录路径少了结尾斜杠就 301 补上),
// 没有的给站内风格的 404,别的方法 405。
func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clearHeaders(w)
	h := w.Header()
	p := r.URL.Path
	if p == "" {
		p = "/"
	}
	if s.serveAPI(w, r, p) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		s.write(w, r, http.StatusMethodNotAllowed, s.notFound, false)
		return
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
		h.Set("Cache-Control", s.cache)
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

// ---- 线路上交给伪装站的连接:数量上限 ----

// MaxConns / MaxConnsPerIP 线路上认证没过、交给伪装站回应的连接,同时最多这么多条 / 同一个来源最多这么多条。
//
// 这样的连接交给伪装站以后空闲 15 秒才断,一直发请求还能一直占着;没开页面伪装时是立刻断开。不设上限的话,
// 一个来源就能在线路端口上挂几万条连接(每条几十 KB),把内存耗尽 —— 面板和全部线路在同一个进程里
// (审查 F-G1,原观察 O11:实测每条约 41 KB)。超了的照没开页面伪装时那样立刻断开(见 Admit)。
// 512 条约 20 MB;同一个来源 16 条够浏览器开页面用(浏览器对一个站最多同时开 6 条)。
var (
	MaxConns      = 512
	MaxConnsPerIP = 16
)

var admitted = struct {
	mu    sync.Mutex
	total int
	byIP  map[string]int
}{byIP: map[string]int{}}

// Admit 线路上一条认证没过的连接要交给伪装站之前先问一声:放得下,返回 release(这条连接回应完了调,多调无妨);
// 放不下 ok 为 false,调用方照没开页面伪装时那样断开。ip 是来源地址(不带端口),IPv6 按 /64 算同一个来源。
func Admit(ip string) (release func(), ok bool) {
	key := sourceKey(ip)
	a := &admitted
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.total >= MaxConns || a.byIP[key] >= MaxConnsPerIP {
		return func() {}, false
	}
	a.total++
	a.byIP[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.total--
			if a.byIP[key]--; a.byIP[key] <= 0 {
				delete(a.byIP, key)
			}
			a.mu.Unlock()
		})
	}, true
}

// sourceKey 来源地址归成计数用的键:IPv4 一个地址一个,IPv6 一个 /64 一个(一台机器手里通常就有一整个 /64)
func sourceKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap().WithZone("")
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
}
