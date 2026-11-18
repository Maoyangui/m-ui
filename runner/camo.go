package runner

// 页面伪装(用户 N2,设计 .tmp/work/DESIGN_CAMO.md):伪装站的来源(读设置带 10 秒缓存,开关、换模板不用重载数据面)、
// 本机的随机种子(每台各自生成,不随主机同步:所有面板长得一样的话伪装站本身就成了指纹),
// 以及可选的 80 / 443 端口(设置 camoPorts,默认关):端口被线路、本机的面板 / 订阅 / 代理面板或别的程序占着就不开,写明原因;
// 签证书走 HTTP 验证时临时让出 80;443 按访问的域名在证书库里挑证书。

import (
	crand "crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Maoyangui/m-ui/camo"
	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/logger"
)

// camoState 伪装站的缓存和两个端口的状况。
type camoState struct {
	mu   sync.Mutex
	at   time.Time // 上次读设置的时间
	on   bool
	key  string
	h    http.Handler // 现在的伪装站(*camo.Site)或跳转(*camo.Redirect)
	busy atomic.Bool  // 正有一条连接在读设置、生成:同时到的别的连接先用旧的,不跟着一起读库

	seedMu  sync.Mutex // 种子第一次生成时只生成一个
	seedMem uint64     // 种子存不进库时先记在内存里(不然每 10 秒换一个样子)

	ports     map[int]*camoPort
	reserved  map[int]bool      // 正在重载的那份配置要用的 TCP 端口(重载完之前,已生效的配置里还没有它们)
	holdUntil map[int]time.Time // 面板 / 订阅 / 代理面板正要改到这个端口:这段时间里不去占
	hold80    int               // 正在走 HTTP 验证签证书:80 先让给它
	loop      bool              // 对账循环已经起来了(只有这时设置页存了设置才当场对账;测试里没有循环,不去占端口)
	stopped   bool
	certs     camoCerts

	media camoMedia // 图包里的照片(camomedia.go)
}

// CamoRedirectMode 设置 camoTemplate 的这个值表示「跳转到我的网站」(网址在 camoRedirect)。
const CamoRedirectMode = "redirect"

// camoPort 80 / 443 上的一个监听。srv 为 nil 时 why 写着没开的原因。
type camoPort struct {
	srv *http.Server
	ln  net.Listener // 让出端口时直接关它:srv.Close 赶在后台 Serve 起来之前调的话关不掉监听(见 closeCamoPort)
	why string
}

const camoCacheFor = 10 * time.Second

// camoHTTP / camoHTTPS 伪装站可选占用的两个端口(测试里换成随便哪两个空闲端口)。
var camoHTTP, camoHTTPS = 80, 443

func camoPortList() []int { return []int{camoHTTP, camoHTTPS} }

// camoSource 现在要用的伪装站(数据面里各入站、订阅 / 面板端口经 camo.Handler 取):没开时为 nil。
// 只有认证没过的连接、对不上的路径才会来取,真客户端不经过这里。
func (r *Runner) camoSource() http.Handler {
	c := &r.camo
	now := time.Now()
	c.mu.Lock()
	stale := c.at.IsZero() || now.Sub(c.at) >= camoCacheFor || now.Before(c.at)
	on, h := c.on, c.h
	c.mu.Unlock()
	if stale && c.busy.CompareAndSwap(false, true) {
		on, h = r.camoRefresh(now)
		c.busy.Store(false)
	}
	if !on {
		return nil
	}
	return h
}

// camoRefresh 读设置;模板、跳转网址、种子、所在的那一周或处理好的照片变了才重新生成(生成很快,内容只在内存里;没开时不生成;
// 照片在后台取、处理,见 camomedia.go)。读库、写种子都不在锁里。
func (r *Runner) camoRefresh(now time.Time) (bool, http.Handler) {
	on := settingOn(r.setting("camoEnabled"))
	tmpl, target, seed := "", "", uint64(0)
	var photos camo.Photos
	gen := 0
	if on {
		tmpl, seed = strings.ToLower(strings.TrimSpace(r.setting("camoTemplate"))), r.camoSeed()
		if tmpl == CamoRedirectMode {
			target = strings.TrimSpace(r.setting("camoRedirect"))
		}
		photos, gen = r.camo.media.photos(seed)
	}
	key := fmt.Sprintf("%s|%s|%x|%d|%d", tmpl, target, seed, camo.WeekOf(now).Unix(), gen)
	c := &r.camo
	c.mu.Lock()
	c.at, c.on = now, on
	if on && (c.h == nil || c.key != key) {
		c.h, c.key = camoBuild(tmpl, target, seed, now, photos), key
	}
	// 这一站还缺照片就交给取图的(没重新生成也交:上次没取全时,到了再试的时间要靠这里接着取;
	// 正在取、还没到再试的时间,它自己不动)
	var want []camo.PhotoReq
	if site, ok := c.h.(*camo.Site); ok && on {
		want = site.Wanted()
	}
	on, h := c.on, c.h
	c.mu.Unlock()
	r.camoMediaWant(seed, want)
	return on, h
}

// camoBuild 跳转(网址不对时 —— 比如命令行改坏了 —— 退回自动挑的伪装站,并记一笔)或者伪装站
func camoBuild(tmpl, target string, seed uint64, now time.Time, photos camo.Photos) http.Handler {
	if tmpl == CamoRedirectMode {
		rd, _, err := camo.ParseRedirect(target)
		if err == nil {
			return rd
		}
		logger.Warning("页面伪装:跳转的网址不对(", target, "),先用自动挑的伪装站: ", err)
		tmpl = ""
	}
	return camo.Build(tmpl, seed, now, photos)
}

// CamoApply 设置页存了页面伪装的设置:下一条连接就按新设置回应(不等 10 秒的缓存),80 / 443 当场对一次账(不等半分钟)。
func (r *Runner) CamoApply() {
	c := &r.camo
	c.mu.Lock()
	c.at = time.Time{}
	loop := c.loop
	c.mu.Unlock()
	if loop {
		r.camoPortsSync()
	}
}

func settingOn(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "true") }

// CamoPreview 设置页的预览:按给的模板(空 = 自动)和本机种子生成的那个站(开关关着也给,方便先看再开),
// 以及它的封面照片状况。预览用到的照片也算「用到」:没取过的在后台去取,设置页过一会儿再看就是照片了。
func (r *Runner) CamoPreview(template string) (*camo.Site, *CamoPhotoStatus) {
	seed := r.camoSeed()
	photos, _ := r.camo.media.photos(seed)
	site := camo.Build(strings.TrimSpace(template), seed, time.Now(), photos)
	r.camoMediaWant(seed, site.Wanted())
	return site, r.camoPhotoStatus(site)
}

// CamoReseed 设置页的「换一个样子」:本机换一个新种子,伪装站马上按它重新生成(只换这台,别的服务器各是各的)。
func (r *Runner) CamoReseed() error {
	c := &r.camo
	c.seedMu.Lock()
	v := newCamoSeed()
	err := r.setSetting("camoSeed", strconv.FormatUint(v, 16))
	if err == nil {
		c.seedMem = 0
	}
	c.seedMu.Unlock()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.at = time.Time{} // 下一条来取的连接重新读
	c.mu.Unlock()
	return nil
}

// camoSeed 本机的伪装站种子:第一次用到时随机生成并存进设置(camoSeed,本机设置,不随主机同步、不进容灾副本)。
func (r *Runner) camoSeed() uint64 {
	c := &r.camo
	c.seedMu.Lock()
	defer c.seedMu.Unlock()
	if v, err := strconv.ParseUint(strings.TrimSpace(r.setting("camoSeed")), 16, 64); err == nil && v != 0 {
		return v
	}
	if c.seedMem != 0 {
		return c.seedMem
	}
	v := newCamoSeed()
	if err := r.setSetting("camoSeed", strconv.FormatUint(v, 16)); err != nil {
		c.seedMem = v
		logger.Warning("页面伪装:本机种子没存上(重启后会换一个样子): ", err)
	}
	return v
}

func newCamoSeed() uint64 {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return binary.BigEndian.Uint64(b[:]) | 1
}

// ---- 80 / 443 ----

// CamoPortState 设置页上显示的端口状况。
type CamoPortState struct {
	Port int  `json:"port"`
	On   bool `json:"on"`
	// Why 没开的原因:line(线路在用)/ service(这台的面板、订阅或代理面板在用)/ acme(正在签证书,临时让出)/
	// nocert(443 没有可用的证书)/ busy(别的程序占着,Info 是原文)
	Why  string `json:"why,omitempty"`
	Info string `json:"detail,omitempty"`
}

// CamoPorts 80 / 443 现在的状况;没开 camoPorts(或还没对过账)时为空。
func (r *Runner) CamoPorts() []CamoPortState {
	c := &r.camo
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []CamoPortState
	for _, p := range camoPortList() {
		cp := c.ports[p]
		if cp == nil {
			continue
		}
		st := CamoPortState{Port: p, On: cp.srv != nil}
		if cp.srv == nil {
			why, info, _ := strings.Cut(cp.why, ":")
			st.Why, st.Info = why, strings.TrimSpace(info)
		}
		out = append(out, st)
	}
	return out
}

// CamoOn 伪装站现在是不是占着这个端口。
func (r *Runner) CamoOn(port int) bool {
	c := &r.camo
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := c.ports[port]
	return cp != nil && cp.srv != nil
}

// camoFirewallPorts 系统防火墙要给伪装站放行的端口(fwPorts 用):伪装站占着的;签证书时临时让出的 80 也算 ——
// 不然那一会儿防火墙规则被撤掉,HTTP 验证反而进不来。
func (r *Runner) camoFirewallPorts() []int {
	c := &r.camo
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []int
	for _, p := range camoPortList() {
		if cp := c.ports[p]; cp != nil && (cp.srv != nil || cp.why == "acme") {
			out = append(out, p)
		}
	}
	return out
}

// camoPortsLoop 每 30 秒对一次账:开着 camoPorts 就把空着的 80 / 443 占上,关了就放掉。
func (r *Runner) camoPortsLoop(stop <-chan struct{}) {
	r.camo.mu.Lock()
	r.camo.loop = true
	r.camo.mu.Unlock()
	r.camoPortsSync()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.camoPortsSync()
		case <-stop:
			r.camoPortsClose()
			return
		}
	}
}

// camoPortsSync 按设置开关 80 / 443。不占的端口:线路要用的(TCP 入站;只走 UDP 的 Hysteria2 / TUIC 不冲突)、
// 这台的面板 / 订阅 / 代理面板端口、正在走 HTTP 验证签证书时的 80。设置、证书清单都在拿锁之前读好。
func (r *Runner) camoPortsSync() {
	want := settingOn(r.setting("camoEnabled")) && settingOn(r.setting("camoPorts"))
	used := linePortsTCP(r.appliedConfig())
	svc := r.camoServicePorts()
	var certs []camoCertFile
	if want {
		certs = r.camoCertFiles()
	}
	now := time.Now()
	c := &r.camo
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	if c.ports == nil {
		c.ports = map[int]*camoPort{}
	}
	for _, port := range camoPortList() {
		cp := c.ports[port]
		if !want {
			if cp != nil {
				if closeCamoPort(cp) {
					logger.Info("页面伪装:", port, " 端口上的伪装站已关掉")
				}
				delete(c.ports, port)
			}
			continue
		}
		if cp == nil {
			cp = &camoPort{}
			c.ports[port] = cp
		}
		why := ""
		switch {
		case used[port] || c.reserved[port]:
			why = "line"
		case svc[port] || now.Before(c.holdUntil[port]):
			why = "service"
		case port == camoHTTP && c.hold80 > 0:
			why = "acme"
		case port == camoHTTPS && !c.certs.update(certs):
			why = "nocert"
		}
		if why != "" {
			if closeCamoPort(cp) {
				logger.Info("页面伪装:", port, " 端口让出来了(", why, ")")
			}
			cp.why = why
			continue
		}
		if cp.srv != nil {
			continue
		}
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			cp.why = "busy: " + err.Error()
			continue
		}
		if port == camoHTTPS {
			ln = tls.NewListener(ln, &tls.Config{GetCertificate: c.certs.get, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
		}
		srv := &http.Server{Handler: r.camoPortHandler(port), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 30 * time.Second,
			IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0)}
		cp.srv, cp.ln, cp.why = srv, ln, ""
		go func() { _ = srv.Serve(ln) }()
		logger.Info("页面伪装:", port, " 端口已放上伪装站")
	}
}

// camoPortsYield 重载数据面之前调:新配置的线路要用 80 / 443(TCP)时,先把伪装站在这两个端口上的监听放掉,线路才绑得上;
// 这份配置要用的端口记下来,重载完之前对账也不去占(已生效的配置里还没有它们)。
func (r *Runner) camoPortsYield(raw []byte) {
	used := linePortsTCP(raw)
	c := &r.camo
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reserved = used
	for port, cp := range c.ports {
		if used[port] && closeCamoPort(cp) {
			cp.why = "line"
			logger.Info("页面伪装:线路要用 ", port, " 端口,伪装站让出来")
		}
	}
}

// CamoReleasePort 面板 / 订阅 / 代理面板要改到 80 或 443:伪装站先让出来,两分钟内不再去占
// (保存成功后对账时按「面板在用」不再占;保存没成功,两分钟后照常放回去)。
func (r *Runner) CamoReleasePort(port int) {
	c := &r.camo
	c.mu.Lock()
	defer c.mu.Unlock()
	if port != camoHTTP && port != camoHTTPS {
		return
	}
	if c.holdUntil == nil {
		c.holdUntil = map[int]time.Time{}
	}
	c.holdUntil[port] = time.Now().Add(2 * time.Minute)
	if cp := c.ports[port]; cp != nil && closeCamoPort(cp) {
		cp.why = "service"
		logger.Info("页面伪装:", port, " 端口让给面板 / 订阅")
	}
}

// camoHold80 签证书走 HTTP 验证要临时占 80 端口:伪装站占着就先让出来,签完(成功、失败都算)调返回的函数放回去
// (下一轮对账时重新占上)。
func (r *Runner) camoHold80() func() {
	c := &r.camo
	c.mu.Lock()
	c.hold80++
	if cp := c.ports[camoHTTP]; cp != nil && closeCamoPort(cp) {
		cp.why = "acme"
		logger.Info("页面伪装:签证书要用 80 端口,伪装站先让出来")
	}
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.hold80--
			c.mu.Unlock()
		})
	}
}

func (r *Runner) camoPortsClose() {
	c := &r.camo
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	for _, cp := range c.ports {
		closeCamoPort(cp)
	}
}

// closeCamoPort 关掉这个端口上的监听,返回时端口已经放开;原来开着的返回 true。
// 监听要自己关:srv.Serve 在后台 goroutine 里起,srv.Close 赶在它登记监听之前调的话不管这个监听,端口要等那个 goroutine
// 跑起来才放 —— 刚占上就要让给线路(重载数据面)时,线路会绑不上(压力测试里遇到)。
func closeCamoPort(cp *camoPort) bool {
	if cp.srv == nil {
		return false
	}
	_ = cp.srv.Close()
	if cp.ln != nil {
		_ = cp.ln.Close()
	}
	cp.srv, cp.ln = nil, nil
	return true
}

// camoPortHandler 80 / 443 上的处理:开着伪装就给伪装站(关了以后监听也随之放掉,这里只是保险)。
// 伪装站在 443 上也开着时,80 上的请求和常见网站一样 301 到 https(跳转模式本来就跳走,不用管)。
func (r *Runner) camoPortHandler(port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := camo.Handler()
		if h == nil {
			http.NotFound(w, req)
			return
		}
		if _, site := h.(*camo.Site); site && port == camoHTTP && r.CamoOn(camoHTTPS) {
			if loc := httpsURL(req.Host, camoHTTPS, req.URL.RequestURI()); loc != "" {
				for k := range w.Header() {
					delete(w.Header(), k)
				}
				w.Header().Set("Location", loc)
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(http.StatusMovedPermanently)
				return
			}
		}
		h.ServeHTTP(w, req)
	})
}

// httpsURL 把这次请求的主机名(去掉端口)换成 https 的地址;没有主机名时返回空
func httpsURL(hostHeader string, port int, uri string) string {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return ""
	}
	hp := host
	if strings.Contains(host, ":") {
		hp = "[" + host + "]"
	}
	if port != 443 {
		hp = net.JoinHostPort(host, strconv.Itoa(port))
	}
	if !strings.HasPrefix(uri, "/") {
		uri = "/"
	}
	return "https://" + hp + uri
}

// camoServicePorts 这台的面板 / 订阅 / 代理面板端口(和 web 包 localServicePorts 同一口径:代理面板关掉或本机是副机时不算)。
func (r *Runner) camoServicePorts() map[int]bool {
	out := map[int]bool{r.settingIntOr("webPort", 2053): true, r.settingIntOr("subPort", 2056): true}
	if !strings.EqualFold(strings.TrimSpace(r.setting("resellerEnabled")), "false") && !settingOn(r.setting("nodeMode")) {
		out[r.settingIntOr("resellerPort", 2054)] = true
	}
	return out
}

// linePortsTCP 渲染好的 sing-box 配置里,各入站用的 TCP 端口(只走 UDP 的 hysteria2 / tuic / hysteria 不算)。
func linePortsTCP(raw []byte) map[int]bool {
	out := map[int]bool{}
	var cfg struct {
		Inbounds []struct {
			Type       string `json:"type"`
			ListenPort int    `json:"listen_port"`
		} `json:"inbounds"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &cfg) != nil {
		return out
	}
	for _, in := range cfg.Inbounds {
		switch in.Type {
		case "hysteria2", "hysteria", "tuic":
			continue
		}
		if in.ListenPort > 0 {
			out[in.ListenPort] = true
		}
	}
	return out
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ---- 443 的证书 ----

type camoCertFile struct{ cert, key string }

// camoCertFiles 443 上用的证书:线路默认证书在前(访问的域名哪张都没写时用它),证书库里别的证书按名字排在后面 ——
// 按访问的域名挑,直接打开线路的域名时看到的证书和线路一致。一张都没有返回 nil。
func (r *Runner) camoCertFiles() []camoCertFile {
	var out []camoCertFile
	seen := map[string]bool{}
	add := func(cert, key string) {
		cert, key = strings.TrimSpace(cert), strings.TrimSpace(key)
		if cert == "" || key == "" || seen[cert] || !fileExists(cert) || !fileExists(key) {
			return
		}
		seen[cert] = true
		out = append(out, camoCertFile{cert, key})
	}
	nc := r.nodeCert() // 线路默认证书;没设时和数据面一样回落到面板证书
	add(nc.CertPath, nc.KeyPath)
	named := namedCertsOf(r.db)
	names := make([]string, 0, len(named))
	for n := range named {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		add(named[n].CertPath, named[n].KeyPath)
	}
	return out
}

var errNoCamoCert = errors.New("no certificate for the camouflage site")

// camoCerts 443 上的证书:按访问的域名挑(certutil.SNI),证书文件续期后自动换上;证书清单变了(加了、删了证书)才重建。
type camoCerts struct {
	mu  sync.Mutex
	key string
	sni *certutil.SNI
}

// update 按现在的证书清单更新,返回有没有能用的证书;清单空了、新的一张都读不出来时继续用已经在用的。
func (c *camoCerts) update(files []camoCertFile) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f.cert)
		b.WriteByte(0)
		b.WriteString(f.key)
		b.WriteByte(0)
	}
	if key := b.String(); key != c.key || c.sni == nil {
		var list []*certutil.Reloader
		for _, f := range files {
			if rl, err := certutil.NewReloader(f.cert, f.key); err == nil {
				list = append(list, rl)
			}
		}
		if len(list) > 0 {
			c.sni, c.key = certutil.NewSNI(list[0], list[1:]...), key
		}
	}
	return c.sni != nil
}

func (c *camoCerts) get(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	s := c.sni
	c.mu.Unlock()
	if s == nil {
		return nil, errNoCamoCert
	}
	return s.GetCertificate(h)
}

// appliedConfig 数据面现在跑着的那份配置(拿锁读;重载过程中调的地方直接用 r.appliedRaw)。
func (r *Runner) appliedConfig() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.appliedRaw
}
