package web

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/totp"

	"github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

// 代理面板:另起一个监听(默认 2054,路径 /dl),复用同一套前端与接口,
// 区别只在会话带作用域——所有查询都被限制在该代理名下,管理类接口一概不注册。

const resellerCookie = "m-ui-dl"

// scopeKey 把当前会话的代理 ID 放进请求上下文;主面板的请求里没有,即为 0(不受限)。
type scopeKeyType struct{}

var scopeKey scopeKeyType

// scope 返回当前请求的代理 ID:0 = 主面板管理员(不受限)。
func scope(r *http.Request) uint {
	id, _ := r.Context().Value(scopeKey).(uint)
	return id
}

// resellerPath 代理面板对外路径,默认 /dl/。
func (s *Server) resellerPath() string {
	return normalizePath(s.setting("resellerPath"), "/dl/")
}

// StartReseller 启动代理面板;设置 resellerEnabled=false 或副机模式下不启动。
func (s *Server) StartReseller() error {
	if strings.EqualFold(s.setting("resellerEnabled"), "false") || s.run.IsNode() {
		return nil
	}
	base := innerBase
	mux := http.NewServeMux()
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		return err
	}
	mux.Handle(base, assetCache(sub, http.StripPrefix(strings.TrimSuffix(base, "/"), http.FileServer(http.FS(sub)))))

	api := base + "api/"
	mux.HandleFunc(api+"login", s.handleResellerLogin)
	mux.HandleFunc(api+"logout", s.handleResellerLogout)
	mux.HandleFunc(api+"status", s.rauth(s.handleStatus))
	mux.HandleFunc(api+"settings", s.rauth(s.handleSettings))
	mux.HandleFunc(api+"lines", s.rauth(s.handleLines))
	mux.HandleFunc(api+"users", s.rauth(s.handleUsers))
	mux.HandleFunc(api+"users/", s.rauth(s.handleUserItem))
	mux.HandleFunc(api+"plans", s.rauth(s.handlePlans))
	mux.HandleFunc(api+"plans/", s.rauth(s.handlePlanItem))
	mux.HandleFunc(api+"stats", s.rauth(s.handleStats))
	mux.HandleFunc(api+"onlines", s.rauth(s.handleOnlines))
	mux.HandleFunc(api+"self", s.rauth(s.handleResellerSelf))
	mux.HandleFunc(api+"self/", s.rauth(s.handleResellerSelfSub))
	mux.HandleFunc(api+"v1/", s.handleResellerPublicAPI) // 代理自己的外部 API:令牌鉴权,作用域限定为该代理
	mux.HandleFunc(base+"logo.svg", brand.ServeLogo)
	mux.HandleFunc(base+"support", s.handleSupport)
	mux.HandleFunc(base+"support/qr", s.handleSupportQR)

	path := s.resellerPath()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		if r.URL.Path == "/" && path != "/" {
			http.Redirect(w, r, path, http.StatusTemporaryRedirect)
			return
		}
		if path != "/" && r.URL.Path == strings.TrimSuffix(path, "/") { // /dl 少个斜杠也能打开
			redirectTo(w, r, path)
			return
		}
		// 对外路径改写到固定前缀,改路径保存即生效
		if strings.HasPrefix(r.URL.Path, path) {
			r.URL.Path = innerBase + strings.TrimPrefix(r.URL.Path, path)
			mux.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})

	listen := s.setting("resellerListen")
	if listen == "" {
		listen = "0.0.0.0"
	}
	port := s.settingInt("resellerPort", 2054)
	if port == s.settingInt("webPort", 2053) || port == s.settingInt("subPort", 2056) {
		return fmt.Errorf("代理面板端口 %d 与面板/订阅端口冲突,请在设置里改", port)
	}
	addr := net.JoinHostPort(listen, fmt.Sprint(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	scheme := "http"
	// 证书:留空则跟面板用同一张
	certFile, keyFile := s.setting("resellerCertFile"), s.setting("resellerKeyFile")
	if certFile == "" && keyFile == "" {
		certFile, keyFile = s.setting("webCertFile"), s.setting("webKeyFile")
	}
	if certFile != "" && keyFile != "" {
		rl, err := certutil.NewReloader(certFile, keyFile)
		if err != nil {
			ln.Close()
			return fmt.Errorf("加载代理面板证书: %w", err)
		}
		ln = tls.NewListener(ln, rl.TLSConfig())
		scheme = "https"
	}
	s.rListener = ln
	s.rSrv = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.rSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Warning("代理面板退出: ", err)
		}
	}()
	logger.Info("代理面板已启动 ", scheme, "://", addr, path)
	return nil
}

func (s *Server) StopReseller() error {
	if s.rSrv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.rSrv.Shutdown(ctx)
	s.rSrv = nil
	return err
}

// rauth 代理面板鉴权:必须是代理会话;还没设密码的会话只放行状态与设密码。
func (s *Server) rauth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "跨站请求被拒绝"})
			return
		}
		c, err := r.Cookie(resellerCookie)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
			return
		}
		s.mu.Lock()
		sess, ok := s.sessions[c.Value]
		if ok && time.Now().After(sess.exp) {
			delete(s.sessions, c.Value)
			ok = false
		}
		s.mu.Unlock()
		if !ok || sess.reseller == 0 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
			return
		}
		var rs model.Reseller
		if s.db.First(&rs, sess.reseller).Error != nil || !rs.Enabled {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "代理已停用"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 代理面板没有上传接口
		}
		if sess.pending && !strings.HasSuffix(r.URL.Path, "/status") && !strings.HasSuffix(r.URL.Path, "/self/password") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "请先设置密码"})
			return
		}
		next(w, r.WithContext(withScope(r, sess.reseller)))
	}
}

func (s *Server) handleResellerLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var body struct{ Username, Password, Code string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	ip, peer := clientIP(r), peerIP(r)
	if s.loginBlocked(peer) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "失败次数过多,请 5 分钟后再试"})
		return
	}
	var rs model.Reseller
	if s.db.Where("name = ?", strings.TrimSpace(body.Username)).First(&rs).Error != nil || !rs.Enabled {
		time.Sleep(300 * time.Millisecond)
		s.noteLoginFailure(peer)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}
	first := rs.Password == "" // 新建的代理没有密码:凭用户名登录,进去必须先设密码
	if first && (rs.ClaimBefore == 0 || time.Now().Unix() > rs.ClaimBefore) {
		// 认领窗口已过:不能再空密码进,免得账号一直敞着
		time.Sleep(300 * time.Millisecond)
		s.noteLoginFailure(peer)
		logger.Warning("代理 ", rs.Name, " 的首登窗口已过期,需主面板重置密码")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"}) // 措辞保持一致,不暴露账号是否存在
		return
	}
	if !first {
		if bcrypt.CompareHashAndPassword([]byte(rs.Password), []byte(body.Password)) != nil {
			time.Sleep(300 * time.Millisecond)
			s.noteLoginFailure(peer)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
			return
		}
		if rs.TotpEnabled {
			if body.Code == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "需要两步验证码", "totp": true})
				return
			}
			ok, step := totp.Verify(rs.TotpSecret, body.Code, time.Now())
			s.mu.Lock()
			replay := ok && step <= s.lastTotpStepRS[rs.Id] // 同一验证码不能再用
			if ok && !replay {
				s.lastTotpStepRS[rs.Id] = step
			}
			s.mu.Unlock()
			if !ok || replay {
				time.Sleep(300 * time.Millisecond)
				s.noteLoginFailure(peer)
				writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "两步验证码错误", "totp": true})
				return
			}
		}
	}
	token := s.newResellerSession(rs, first)
	http.SetCookie(w, &http.Cookie{
		Name: resellerCookie, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: s.setting("resellerCertFile") != "" || s.setting("webCertFile") != "",
	})
	s.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).
		Update("last_logins", time.Now().In(s.panelLocation()).Format("2006-01-02 15:04:05")+" "+ip) // 按面板时区显示,不用服务器本地时间
	logger.Info("代理登录成功: ", rs.Name, " 来自 ", ip)
	writeJSON(w, http.StatusOK, map[string]interface{}{"username": rs.Name, "mustSetPassword": first})
}

func (s *Server) handleResellerLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(resellerCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: resellerCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

func (s *Server) newResellerSession(rs model.Reseller, pending bool) string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	maxAge := time.Duration(s.settingInt("sessionMaxAge", 0)) * time.Minute
	if maxAge <= 0 {
		maxAge = 7 * 24 * time.Hour
	}
	s.mu.Lock()
	s.sessions[token] = session{user: rs.Name, reseller: rs.Id, pending: pending, exp: time.Now().Add(maxAge)}
	s.mu.Unlock()
	return token
}

// setSessionPending 设完密码后清掉"必须改密码"标记。
func (s *Server) setSessionPending(r *http.Request, pending bool) {
	c, err := r.Cookie(resellerCookie)
	if err != nil {
		return
	}
	s.mu.Lock()
	if sess, ok := s.sessions[c.Value]; ok {
		sess.pending = pending
		s.sessions[c.Value] = sess
	}
	s.mu.Unlock()
}

// current 当前登录的代理。
func (s *Server) current(r *http.Request) (model.Reseller, error) {
	var rs model.Reseller
	id := scope(r)
	if id == 0 {
		return rs, errors.New("不是代理会话")
	}
	return rs, s.db.First(&rs, id).Error
}

// handleResellerSelf GET 自己的额度与落地页设置;PUT 改落地页设置。
func (s *Server) handleResellerSelf(w http.ResponseWriter, r *http.Request) {
	rs, err := s.current(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.resellerRow(rs))
	case http.MethodPut:
		var p struct {
			PageEnabled  bool   `json:"pageEnabled"`
			ShareOn      bool   `json:"shareOn"`
			ProfileTitle string `json:"profileTitle"`
			PageTitle    string `json:"pageTitle"`
			PageNotice   string `json:"pageNotice"`
			PageSupport  string `json:"pageSupport"`
			PageBuyURL   string `json:"pageBuyURL"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		buy := strings.TrimSpace(p.PageBuyURL)
		if buy != "" && !strings.HasPrefix(strings.ToLower(buy), "http://") && !strings.HasPrefix(strings.ToLower(buy), "https://") {
			badRequest(w, errors.New("选购链接必须以 http:// 或 https:// 开头"))
			return
		}
		s.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Updates(map[string]interface{}{
			"page_enabled": p.PageEnabled, "share_on": p.ShareOn, "profile_title": strings.TrimSpace(p.ProfileTitle),
			"page_title": p.PageTitle, "page_notice": p.PageNotice, "page_support": p.PageSupport, "page_buy_url": buy,
		})
		s.auditAs(rs.Name, "reseller", "subpage", rs.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// handleResellerSelfSub 处理 /self/password 与 /self/totp。
func (s *Server) handleResellerSelfSub(w http.ResponseWriter, r *http.Request) {
	rs, err := s.current(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/password"):
		s.handleResellerPassword(w, r, rs)
	case strings.HasSuffix(r.URL.Path, "/api/rotate"):
		// 重新生成令牌:旧令牌立即失效
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return
		}
		tok := randomAPIToken()
		s.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("api_token", tok)
		s.auditAs(rs.Name, "reseller", "api-rotate", rs.Name)
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": rs.ApiEnabled, "token": tok})
	case strings.HasSuffix(r.URL.Path, "/api"):
		// 代理自己的外部 API:开关 + 令牌(首次开启自动生成)
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": rs.ApiEnabled, "token": rs.ApiToken})
		case http.MethodPut:
			var p struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				badRequest(w, err)
				return
			}
			tok := rs.ApiToken
			if p.Enabled && tok == "" {
				tok = randomAPIToken()
			}
			s.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Updates(map[string]interface{}{"api_enabled": p.Enabled, "api_token": tok})
			s.auditAs(rs.Name, "reseller", "api-"+map[bool]string{true: "on", false: "off"}[p.Enabled], rs.Name)
			writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": p.Enabled, "token": tok})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		}
	case strings.HasSuffix(r.URL.Path, "/totp/qr"):
		s.mu.Lock()
		secret := s.totpPendingRS[rs.Id]
		s.mu.Unlock()
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		png, err := qrcode.Encode(s.totpURL(rs.Name, secret), qrcode.Medium, 240)
		if err != nil {
			badRequest(w, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(png)
	case strings.HasSuffix(r.URL.Path, "/totp"):
		s.handleResellerTotp(w, r, rs)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleResellerPassword(w http.ResponseWriter, r *http.Request, rs model.Reseller) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var body struct{ Old, New string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	if len(body.New) < 8 {
		badRequest(w, errors.New("新密码至少 8 位"))
		return
	}
	if rs.Password != "" && bcrypt.CompareHashAndPassword([]byte(rs.Password), []byte(body.Old)) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "原密码错误"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.New), bcrypt.DefaultCost)
	if err != nil {
		badRequest(w, err)
		return
	}
	s.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).
		Updates(map[string]interface{}{"password": string(hash), "claim_before": 0})
	s.setSessionPending(r, false)
	s.auditAs(rs.Name, "reseller", "password", rs.Name)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// handleResellerTotp GET 生成待验证密钥;POST 用验证码开启;DELETE 关闭。
func (s *Server) handleResellerTotp(w http.ResponseWriter, r *http.Request, rs model.Reseller) {
	switch r.Method {
	case http.MethodGet:
		secret := totp.GenerateSecret()
		s.mu.Lock()
		s.totpPendingRS[rs.Id] = secret
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "url": s.totpURL(rs.Name, secret)})
	case http.MethodPost:
		var body struct{ Code, Secret string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, err)
			return
		}
		s.mu.Lock()
		secret := s.totpPendingRS[rs.Id]
		s.mu.Unlock()
		if strings.TrimSpace(body.Secret) != "" {
			n, err := totp.Normalize(body.Secret) // 手动输入的密钥
			if err != nil {
				badRequest(w, err)
				return
			}
			secret = n
		}
		if ok, _ := totp.Verify(secret, body.Code, time.Now()); secret == "" || !ok {
			badRequest(w, errors.New("验证码不正确"))
			return
		}
		s.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).
			Updates(map[string]interface{}{"totp_secret": secret, "totp_enabled": true})
		s.auditAs(rs.Name, "reseller", "totp-on", rs.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	case http.MethodDelete:
		s.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).
			Updates(map[string]interface{}{"totp_secret": "", "totp_enabled": false})
		s.auditAs(rs.Name, "reseller", "totp-off", rs.Name)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}
