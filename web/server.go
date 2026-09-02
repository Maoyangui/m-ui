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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fangjunsheng555/m-ui/certutil"
	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/logger"
	"github.com/fangjunsheng555/m-ui/notify"
	"github.com/fangjunsheng555/m-ui/ops"
	"github.com/fangjunsheng555/m-ui/runner"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

//go:embed assets/*
var assets embed.FS

const sessionCookie = "m-ui-session"

// Version 由 main 注入,状态接口与"关于"展示用。
var Version = "dev"

// session 记录登录人与过期时间;登录人用于操作审计。
type session struct {
	user string
	exp  time.Time
}

// Server 是面板 HTTP 服务。
type Server struct {
	run      *runner.Runner
	db       *gorm.DB
	httpSrv  *http.Server
	listener net.Listener

	mu         sync.Mutex
	sessions   map[string]session
	loginFails map[string][]int64 // ip → 最近失败时间
	ops        *ops.Runner
}

func NewServer(run *runner.Runner) *Server {
	return &Server{run: run, db: run.DB(), sessions: map[string]session{}, ops: ops.NewRunner()}
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
	p := s.setting("webPath")
	if p == "" {
		p = "/app/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func (s *Server) Start() error {
	base := s.basePath()
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
	mux.HandleFunc(api+"lines", s.auth(s.handleLines))
	mux.HandleFunc(api+"lines/", s.auth(s.handleLineItem))
	mux.HandleFunc(api+"lines/sort", s.auth(s.handleLineSort))
	mux.HandleFunc(api+"upstreams", s.auth(s.handleUpstreams))
	mux.HandleFunc(api+"upstreams/", s.auth(s.handleUpstreamItem))
	mux.HandleFunc(api+"users", s.auth(s.handleUsers))
	mux.HandleFunc(api+"users/", s.auth(s.handleUserItem))
	mux.HandleFunc(api+"settings", s.auth(s.handleSettings))
	mux.HandleFunc(api+"sublogs", s.auth(s.handleSubLogs))
	mux.HandleFunc(api+"password", s.auth(s.handlePassword))
	mux.HandleFunc(api+"reload", s.auth(s.handleReload))
	mux.HandleFunc(api+"stats", s.auth(s.handleStats))
	mux.HandleFunc(api+"onlines", s.auth(s.handleOnlines))
	mux.HandleFunc(api+"logs", s.auth(s.handleLogs))
	mux.HandleFunc(api+"audit", s.auth(s.handleAudit))
	mux.HandleFunc(api+"keygen", s.auth(s.handleKeygen))
	mux.HandleFunc(api+"plans", s.auth(s.handlePlans))
	mux.HandleFunc(api+"plans/", s.auth(s.handlePlanItem))
	mux.HandleFunc(api+"notify/test", s.auth(s.handleNotifyTest))
	mux.HandleFunc(api+"cert", s.auth(s.handleCert))
	mux.HandleFunc(api+"cert/", s.auth(s.handleCertSub))
	mux.HandleFunc(api+"backup", s.auth(s.handleBackup))
	mux.HandleFunc(api+"backup/", s.auth(s.handleBackupSub))
	mux.HandleFunc(api+"ops", s.auth(s.handleOps))
	mux.HandleFunc(api+"ops/", s.auth(s.handleOpsSub))
	mux.HandleFunc(api+"nodes", s.auth(s.handleNodes))
	mux.HandleFunc(api+"nodes/", s.auth(s.handleNodeItem))
	mux.HandleFunc(api+"agent/", s.handleAgent) // 内部按动作分别做令牌/会话鉴权

	// 根路径重定向到面板
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, base, http.StatusTemporaryRedirect)
			return
		}
		http.NotFound(w, r)
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
	s.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Warning("面板服务退出: ", err)
		}
	}()
	logger.Info("面板已启动 ", scheme, "://", addr, base)
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
	return true
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
		s.mu.Unlock()
	}
}

// auth 包装需要登录的处理函数。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
	var req struct{ Username, Password string }
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
		s.noteLoginFailure(clientIP(r))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
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
