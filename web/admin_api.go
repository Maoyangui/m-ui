package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/logger"
	"github.com/fangjunsheng555/m-ui/totp"

	"github.com/skip2/go-qrcode"
)

// ---- 管理员:账号信息、两步验证(TOTP)、外部 API ----
//
// 相关设置项(不随主副机同步):
//   totpEnabled / totpSecret   两步验证开关与密钥(base32)
//   apiEnabled  / apiToken     外部 API 开关与 Bearer 令牌

const totpIssuer = "m-ui"

// handleAdmin 分发 /api/admin/*。
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	idx := strings.LastIndex(path, "/admin/")
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	action := path[idx+len("/admin/"):]
	post := func(fn func()) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return
		}
		fn()
	}
	switch action {
	case "info":
		s.adminInfo(w, r)
	case "totp/setup":
		post(func() { s.totpSetup(w, r) })
	case "totp/qr":
		s.totpQR(w, r)
	case "totp/cancel":
		post(func() {
			s.mu.Lock()
			s.totpPending = ""
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
		})
	case "totp/enable":
		post(func() { s.totpEnable(w, r) })
	case "totp/disable":
		post(func() { s.totpDisable(w, r) })
	case "api":
		post(func() { s.apiToggle(w, r) })
	case "api/rotate":
		post(func() {
			tok := s.newAPIToken()
			s.audit(r, "admin", "api:rotate", nil)
			writeJSON(w, http.StatusOK, map[string]string{"token": tok})
		})
	default:
		http.NotFound(w, r)
	}
}

// apiBase 外部 API 的绝对地址前缀(按当前访问的协议与主机拼出,复制即用)。
func (s *Server) apiBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + s.basePath() + "api/v1"
}

func (s *Server) adminInfo(w http.ResponseWriter, r *http.Request) {
	var admin model.Admin
	s.db.First(&admin)
	s.mu.Lock()
	pending := s.totpPending
	s.mu.Unlock()
	out := map[string]interface{}{
		"username":    admin.Username,
		"lastLogins":  admin.LastLogins,
		"totpEnabled": s.setting("totpEnabled") == "true",
		"apiEnabled":  s.setting("apiEnabled") == "true",
		"apiToken":    s.setting("apiToken"),
		"apiBase":     s.apiBase(r),
	}
	if pending != "" {
		out["totpPending"] = map[string]string{"secret": pending, "url": s.totpURL(admin.Username, pending)}
	}
	writeJSON(w, http.StatusOK, out)
}

// totpURL 账户标签带上域名/主机,认证器里多个面板不会混在一起。
func (s *Server) totpURL(username, secret string) string {
	host := s.setting("webDomain")
	if host == "" {
		host = s.run.PublicHost()
	}
	account := username
	if host != "" {
		account += "@" + host
	}
	return totp.URL(totpIssuer, account, secret)
}

// totpSetup 生成(或校验手工输入的)密钥并暂存,尚未生效;须用认证器给出一次正确验证码后才开启。
func (s *Server) totpSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Secret string `json:"secret"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	secret := totp.GenerateSecret()
	if strings.TrimSpace(req.Secret) != "" {
		n, err := totp.Normalize(req.Secret)
		if err != nil {
			badRequest(w, err)
			return
		}
		secret = n
	}
	s.mu.Lock()
	s.totpPending = secret
	s.mu.Unlock()
	var admin model.Admin
	s.db.First(&admin)
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "url": s.totpURL(admin.Username, secret)})
}

func (s *Server) totpQR(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	pending := s.totpPending
	s.mu.Unlock()
	if pending == "" {
		http.NotFound(w, r)
		return
	}
	var admin model.Admin
	s.db.First(&admin)
	png, err := qrcode.Encode(s.totpURL(admin.Username, pending), qrcode.Medium, 220)
	if err != nil {
		badRequest(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

func (s *Server) totpEnable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	pending := s.totpPending
	s.mu.Unlock()
	if pending == "" {
		badRequest(w, errors.New("请先生成或输入密钥"))
		return
	}
	ok, step := totp.Verify(pending, req.Code, time.Now())
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "验证码不正确,请确认认证器已添加此密钥且手机时间准确"})
		return
	}
	s.run.SetSetting("totpSecret", pending)
	s.run.SetSetting("totpEnabled", "true")
	s.mu.Lock()
	s.totpPending = ""
	s.lastTotpStep = step
	s.mu.Unlock()
	s.audit(r, "admin", "totp:enable", nil)
	logger.Info("已开启面板两步验证")
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

func (s *Server) totpDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var admin model.Admin
	if err := s.db.First(&admin).Error; err != nil {
		badRequest(w, err)
		return
	}
	if !checkPassword(req.Password, admin.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "密码错误"})
		return
	}
	if ok, _ := totp.Verify(s.setting("totpSecret"), req.Code, time.Now()); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "验证码错误"})
		return
	}
	s.run.SetSetting("totpEnabled", "false")
	s.run.SetSetting("totpSecret", "")
	s.audit(r, "admin", "totp:disable", nil)
	logger.Warning("已关闭面板两步验证")
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// ---- 外部 API 开关 ----

func (s *Server) newAPIToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	s.run.SetSetting("apiToken", tok)
	return tok
}

func (s *Server) apiToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	tok := s.setting("apiToken")
	if req.Enabled && tok == "" {
		tok = s.newAPIToken()
	}
	if req.Enabled {
		s.run.SetSetting("apiEnabled", "true")
	} else {
		s.run.SetSetting("apiEnabled", "false")
	}
	s.audit(r, "admin", "api:"+map[bool]string{true: "enable", false: "disable"}[req.Enabled], nil)
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": req.Enabled, "token": tok})
}
