// Package web 提供 m-ui 管理面板:会话认证、REST API 与内嵌前端。
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
	"github.com/Maoyangui/m-ui/ops"
	"github.com/Maoyangui/m-ui/runner"
	"github.com/Maoyangui/m-ui/totp"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

//go:embed assets/*
var assets embed.FS

const sessionCookie = "m-ui-session"

// innerBase 路由注册用的固定前缀;对外的面板路径(设置 webPath)在请求进入时改写到它,
// 因此改路径保存即生效,无需重启。各处理函数解析路径一律用 innerBase,拼对外地址才用 basePath()。
const innerBase = "/app/"

// Version 由 main 注入,状态接口与"关于"展示用。
var Version = "dev"

// session 记录登录人与过期时间;登录人用于操作审计。
// reseller 非 0 表示这是代理面板的会话,所有查询都被限制在该代理名下;
// pending 表示代理还没设过密码,进面板后必须先设置。
type session struct {
	user     string
	reseller uint
	pending  bool
	exp      time.Time
}

// Server 是面板 HTTP 服务。
type Server struct {
	run      *runner.Runner
	db       *gorm.DB
	httpSrv  *http.Server
	listener net.Listener

	rSrv      *http.Server // 代理面板(独立端口/路径)
	rListener net.Listener

	mu         sync.Mutex
	sessions   map[string]session
	loginFails map[string][]int64 // ip → 最近失败时间
	ops        *ops.Runner

	totpPending    string          // 两步验证:已生成、待认证器验证一次后才生效的密钥
	totpPendingRS  map[uint]string // 同上,按代理
	lastTotpStep   int64           // 最近一次登录成功用掉的时间步,同一验证码不能再用(防重放)
	lastTotpStepRS map[uint]int64  // 同上,按代理
}

func NewServer(run *runner.Runner) *Server {
	return &Server{run: run, db: run.DB(), sessions: map[string]session{},
		totpPendingRS: map[uint]string{}, lastTotpStepRS: map[uint]int64{}, ops: ops.NewRunner()}
}

// actor 返回当前请求的登录用户名(审计用);未登录为空。
func (s *Server) actor(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[c.Value].user
}

func (s *Server) setting(key string) string {
	var v string
	s.db.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v
}

func (s *Server) settingInt(key string, def int) int {
	v, err := strconv.Atoi(s.setting(key))
	if err != nil {
		return def
	}
	return v
}

// basePath 面板路径,始终以 / 开头结尾。
func (s *Server) basePath() string {
	return normalizePath(s.setting("webPath"), "/app/")
}

// normalizePath 把用户填的路径规整成 /xxx/ 形式:去掉首尾空白与引号、
// 补上首尾斜杠、压掉重复斜杠。填 app、/app、app/、"//app//" 都得到 /app/;填 / 就是 /。
func normalizePath(p, def string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "\"'")
	p = strings.TrimSpace(p)
	if p == "" {
		p = def
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// redirectTo 跳到 target,带上原查询串(少个斜杠的地址跳转时别把 ?lang= 之类丢了)。
func redirectTo(w http.ResponseWriter, r *http.Request, target string) {
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

func (s *Server) Start() error {
	base := innerBase
	logger.SetEnabled(s.setting("logEnabled") != "false")
	mux := http.NewServeMux()

	// 静态前端
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		return err
	}
	mux.Handle(base, assetCache(sub, http.StripPrefix(strings.TrimSuffix(base, "/"), http.FileServer(http.FS(sub)))))

	// API
	api := base + "api/"
	mux.HandleFunc(api+"login", s.handleLogin)
	mux.HandleFunc(api+"logout", s.handleLogout)
	mux.HandleFunc(api+"status", s.auth(s.handleStatus))
	mux.HandleFunc(api+"lines", s.auth(s.masterOnly(s.handleLines)))
	mux.HandleFunc(api+"lines/", s.auth(s.masterOnly(s.handleLineItem)))
	mux.HandleFunc(api+"lines/sort", s.auth(s.masterOnly(s.handleLineSort)))
	mux.HandleFunc(api+"upstreams", s.auth(s.masterOnly(s.handleUpstreams)))
	mux.HandleFunc(api+"upstreams/", s.auth(s.masterOnly(s.handleUpstreamItem)))
	mux.HandleFunc(api+"users", s.auth(s.masterOnly(s.handleUsers)))
	mux.HandleFunc(api+"users/", s.auth(s.masterOnly(s.handleUserItem)))
	mux.HandleFunc(api+"users/import", s.auth(s.masterOnly(s.handleUsersImport)))
	mux.HandleFunc(api+"settings", s.auth(s.handleSettings))
	mux.HandleFunc(api+"sublogs", s.auth(s.handleSubLogs))
	mux.HandleFunc(api+"password", s.auth(s.handlePassword))
	mux.HandleFunc(api+"reload", s.auth(s.handleReload))
	mux.HandleFunc(api+"update", s.auth(s.handleUpdate)) // 副机也是一份完整安装,自己更新自己
	mux.HandleFunc(api+"update/ack", s.auth(s.handleUpdateAck))
	mux.HandleFunc(api+"stats", s.auth(s.handleStats))
	mux.HandleFunc(api+"stats/top", s.auth(s.handleStatsTop))
	mux.HandleFunc(api+"onlines", s.auth(s.handleOnlines))
	mux.HandleFunc(api+"logs", s.auth(s.handleLogs))
	mux.HandleFunc(api+"audit", s.auth(s.handleAudit))
	mux.HandleFunc(api+"keygen", s.auth(s.handleKeygen))
	mux.HandleFunc(api+"resellers", s.auth(s.masterOnly(s.handleResellers)))
	mux.HandleFunc(api+"resellers/", s.auth(s.masterOnly(s.handleResellerItem)))
	mux.HandleFunc(api+"plans", s.auth(s.masterOnly(s.handlePlans)))
	mux.HandleFunc(api+"plans/", s.auth(s.masterOnly(s.handlePlanItem)))
	mux.HandleFunc(api+"notify/test", s.auth(s.handleNotifyTest))
	mux.HandleFunc(api+"cert", s.auth(s.handleCert))
	mux.HandleFunc(api+"cert/", s.auth(s.handleCertSub))
	mux.HandleFunc(api+"backup", s.auth(s.handleBackup))
	mux.HandleFunc(api+"backup/", s.auth(s.handleBackupSub))
	mux.HandleFunc(api+"ops", s.auth(s.handleOps))
	mux.HandleFunc(api+"ops/", s.auth(s.handleOpsSub))
	mux.HandleFunc(api+"conns/recent", s.auth(s.handleRecentConns))
	mux.HandleFunc(api+"exts", s.auth(s.masterOnly(s.handleExts)))
	mux.HandleFunc(api+"exts/", s.auth(s.masterOnly(s.handleExtItem)))
	mux.HandleFunc(api+"nodes", s.auth(s.handleNodes))
	mux.HandleFunc(api+"nodes/", s.auth(s.handleNodeItem))
	mux.HandleFunc(api+"agent/", s.handleAgent) // 内部按动作分别做令牌/会话鉴权
	mux.HandleFunc(base+"logo.svg", brand.ServeLogo)
	mux.HandleFunc(base+"support", s.handleSupport)
	mux.HandleFunc(base+"support/qr", s.handleSupportQR)
	mux.HandleFunc(api+"admin/", s.auth(s.handleAdmin))
	mux.HandleFunc(api+"v1/", s.handlePublicAPI) // 外部 API:Bearer 令牌鉴权,与会话无关

	// 根路径重定向到面板
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && s.basePath() != "/" {
			http.Redirect(w, r, s.basePath(), http.StatusTemporaryRedirect)
			return
		}
		http.NotFound(w, r)
	})
	// 对外前缀 → 内部前缀改写;对外前缀不是 innerBase 时,直接访问内部前缀视为不存在
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		pub := s.basePath()
		// 少一个结尾斜杠也要能打开(/app → /app/);路径就是 / 时没有这一步
		if pub != "/" && r.URL.Path == strings.TrimSuffix(pub, "/") {
			redirectTo(w, r, pub)
			return
		}
		if pub == "/" { // 面板挂在根上:整段前缀就是 /,直接改写到内部前缀
			r.URL.Path = innerBase + strings.TrimPrefix(r.URL.Path, "/")
			r.URL.RawPath = ""
			mux.ServeHTTP(w, r)
			return
		}
		if pub != innerBase && r.URL.Path != "/" {
			switch {
			case strings.HasPrefix(r.URL.Path, pub):
				r.URL.Path = innerBase + strings.TrimPrefix(r.URL.Path, pub)
				r.URL.RawPath = ""
			case strings.HasPrefix(r.URL.Path, innerBase):
				http.NotFound(w, r)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})

	listen := s.setting("webListen")
	if listen == "" {
		listen = "0.0.0.0"
	}
	port := s.settingInt("webPort", 2053)
	addr := net.JoinHostPort(listen, strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	scheme := "http"
	certFile, keyFile := s.setting("webCertFile"), s.setting("webKeyFile")
	if certFile != "" && keyFile != "" {
		rl, err := certutil.NewReloader(certFile, keyFile)
		if err != nil {
			ln.Close()
			return fmt.Errorf("加载面板证书: %w", err)
		}
		ln = tls.NewListener(ln, rl.TLSConfig()) // 证书文件更新后自动换用,续期无需重启
		scheme = "https"
	}

	s.listener = ln
	s.httpSrv = &http.Server{Handler: outer, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Warning("面板服务退出: ", err)
		}
	}()
	logger.Info("面板已启动 ", scheme, "://", addr, s.basePath())
	s.StartUpdateWatch()
	if err := s.StartReseller(); err != nil {
		logger.Warning("代理面板启动失败: ", err)
	}
	go s.reapSessions()
	return nil
}

// assetCache 给内嵌前端加上按内容哈希生成的 ETag,并要求浏览器每次校验(no-cache)。
// 内嵌文件没有修改时间,浏览器会按启发式长期缓存 ES 模块,升级后就会看到旧界面;
// 这里用整套资源的哈希做 ETag:未升级时全部 304,升级后立即拿到新文件。
func assetCache(fsys fs.FS, next http.Handler) http.Handler {
	h := sha256.New()
	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, _ := fs.ReadFile(fsys, p)
		h.Write([]byte(p))
		h.Write(b)
		return nil
	})
	etag := `"` + hex.EncodeToString(h.Sum(nil))[:16] + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Stop() error {
	_ = s.StopReseller()
	if s.httpSrv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.httpSrv.Shutdown(ctx)
}

// ---- 会话 ----

func (s *Server) newSession(user string) string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	maxAge := time.Duration(s.settingInt("sessionMaxAge", 0)) * time.Minute
	if maxAge <= 0 {
		maxAge = 7 * 24 * time.Hour
	}
	s.mu.Lock()
	s.sessions[token] = session{user: user, exp: time.Now().Add(maxAge)}
	s.mu.Unlock()
	return token
}

// validSession 判断这是不是一个有效的**管理员**会话。
// 代理会话与管理员会话共用一张表,而 Cookie 不区分端口:代理只要把自己的令牌
// 换个 Cookie 名塞给主面板,就会被当成管理员——所以这里必须把代理会话挡在外面。
func (s *Server) validSession(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(sess.exp) {
		delete(s.sessions, token)
		return false
	}
	return sess.reseller == 0
}

func (s *Server) reapSessions() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		s.mu.Lock()
		for token, sess := range s.sessions {
			if now.After(sess.exp) {
				delete(s.sessions, token)
			}
		}
		for ip, fails := range s.loginFails { // 扫描器会留下大量 IP,过期的清掉
			if len(fails) == 0 || now.Unix()-fails[len(fails)-1] > 600 {
				delete(s.loginFails, ip)
			}
		}
		s.mu.Unlock()
	}
}

// sameOrigin 拦截跨站请求(CSRF):写操作只接受同源发起。
// JSON 接口不校验 Content-Type,恶意页面可用 text/plain 表单带 Cookie 伪造 POST;
// 现代浏览器对跨站请求都会带 Sec-Fetch-Site / Origin,据此拒绝即可。
// secureHeaders 面板与代理面板的基本响应头:禁止被 iframe 套壳(点击劫持)、
// 禁止 MIME 嗅探、不把面板地址带到外站。
func secureHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

func sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err != nil || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}

// masterOnly 副机上拒绝改动线路 / 上游 / 用户 / 套餐 / 外部节点:它们由主机下发,本机改了几秒后就会被覆盖。
// 只读请求与副机本地动作(测上游、踢线、二维码)放行。
func (s *Server) masterOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && s.role() == "node" {
			p := strings.TrimSuffix(r.URL.Path, "/")
			if !strings.HasSuffix(p, "/test") && !strings.HasSuffix(p, "/parse") && !strings.HasSuffix(p, "/kick") {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "本机是副服务器:线路、上游、用户由主机统一下发,请到主机面板修改"})
				return
			}
		}
		next(w, r)
	}
}

// auth 包装需要登录的处理函数。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "跨站请求被拒绝"})
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.validSession(c.Value) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "跨站请求被拒绝"})
		return
	}
	if s.loginBlocked(peerIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "失败次数过多,请 5 分钟后再试"})
		return
	}
	var req struct{ Username, Password, Code string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}

	var admin model.Admin
	if err := s.db.Where("username = ?", req.Username).First(&admin).Error; err != nil {
		// 统一延迟与措辞,避免用户名枚举
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}
	if !checkPassword(req.Password, admin.Password) {
		time.Sleep(300 * time.Millisecond)
		logger.Warning("面板登录失败,来源 ", clientIP(r))
		s.noteLoginFailure(peerIP(r))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}
	// 两步验证:密码正确后才要求验证码,不向猜密码的人暴露是否开启
	if s.setting("totpEnabled") == "true" {
		if strings.TrimSpace(req.Code) == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "请输入两步验证码", "totp": true, "needCode": true})
			return
		}
		ok, step := totp.Verify(s.setting("totpSecret"), req.Code, time.Now())
		s.mu.Lock()
		replay := ok && step <= s.lastTotpStep
		if ok && !replay {
			s.lastTotpStep = step
		}
		s.mu.Unlock()
		if !ok || replay {
			time.Sleep(300 * time.Millisecond)
			logger.Warning("面板两步验证失败,来源 ", clientIP(r))
			s.noteLoginFailure(peerIP(r))
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "两步验证码错误", "totp": true})
			return
		}
	}
	s.run.Notifier().Event("tgOnLogin", "🔐 <b>面板登录</b>:"+notify.Esc(admin.Username)+"\nIP:"+notify.Esc(clientIP(r))+"\n"+time.Now().Format("2006-01-02 15:04:05"))

	token := s.newSession(admin.Username)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: s.setting("webCertFile") != "",
	})
	stamp := time.Now().Format("2006-01-02 15:04:05") + " " + clientIP(r)
	s.db.Model(&model.Admin{}).Where("id = ?", admin.Id).Update("last_logins", stamp)
	logger.Info("面板登录成功: ", admin.Username, " 来自 ", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"username": admin.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// loginBlocked 该 IP 是否处于冷却期:10 分钟内失败 10 次就先挡 5 分钟,
// 否则空密码首登、弱口令都能被慢速爆破(每次失败只 sleep 300ms)。
func (s *Server) loginBlocked(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loginFails == nil {
		s.loginFails = map[string][]int64{}
	}
	now := time.Now().Unix()
	var recent []int64
	for _, ts := range s.loginFails[ip] {
		if now-ts < 600 {
			recent = append(recent, ts)
		}
	}
	s.loginFails[ip] = recent
	if len(recent) < 10 {
		return false
	}
	return now-recent[len(recent)-1] < 300
}

// noteLoginFailure 记录某 IP 的失败次数,10 分钟内达到 5 次告警一次。
func (s *Server) noteLoginFailure(ip string) {
	s.mu.Lock()
	if s.loginFails == nil {
		s.loginFails = map[string][]int64{}
	}
	now := time.Now().Unix()
	recent := s.loginFails[ip][:0]
	for _, ts := range s.loginFails[ip] {
		if now-ts < 600 {
			recent = append(recent, ts)
		}
	}
	recent = append(recent, now)
	s.loginFails[ip] = recent
	n := len(recent)
	s.mu.Unlock()
	if n == 5 {
		s.run.Notifier().Event("tgOnLogin", "⚠️ <b>面板登录连续失败</b>\nIP:"+notify.Esc(ip)+" 10 分钟内失败 5 次")
	}
}

// checkPassword 兼容 bcrypt 哈希与(旧库可能残留的)明文。
func checkPassword(plain, stored string) bool {
	if strings.HasPrefix(stored, "$2") {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(plain)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(plain), []byte(stored)) == 1
}

func hashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// peerIP 取 TCP 对端地址。登录限流必须用它:X-Forwarded-For 是客户端能随便写的,
// 按它计数等于没有限流(每次换一个就绕过),还能伪造成别人的 IP 把人锁在门外。
func peerIP(r *http.Request) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func badRequest(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}
