package web

import (
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/totp"

	"golang.org/x/crypto/bcrypt"
)

// 代理关两步验证与管理员一致:要当前密码和一次有效验证码;已开启时不能直接换密钥。
func TestResellerTotpDisableNeedsPasswordAndCode(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db, sessions: map[string]session{}, totpPendingRS: map[uint]string{}, lastTotpStepRS: map[uint]int64{}}
	hash, _ := bcrypt.GenerateFromPassword([]byte("pass-1234"), bcrypt.MinCost)
	secret := totp.GenerateSecret()
	rs := model.Reseller{Name: "dl", Enabled: true, Password: string(hash), TotpSecret: secret, TotpEnabled: true}
	db.Create(&rs)

	call := func(method, body string) int {
		r := httptest.NewRequest(method, "http://x/app/api/self/totp", strings.NewReader(body))
		w := httptest.NewRecorder()
		var cur model.Reseller
		db.First(&cur, rs.Id)
		s.handleResellerTotp(w, r, cur)
		return w.Code
	}
	enabled := func() bool {
		var cur model.Reseller
		db.First(&cur, rs.Id)
		return cur.TotpEnabled && cur.TotpSecret == secret
	}
	code, _ := totp.Code(secret, time.Now())
	other, _ := totp.Code("KRSXG5CTMVRXEZLU", time.Now())
	for _, body := range []string{``, `{}`, `{"password":"pass-1234"}`, `{"password":"wrong","code":"` + code + `"}`, `{"password":"pass-1234","code":"000000x"}`} {
		if c := call("DELETE", body); c == 200 || !enabled() {
			t.Fatalf("缺密码或验证码(%s)不该能关掉 2FA,得 %d", body, c)
		}
	}
	if c := call("POST", `{"secret":"KRSXG5CTMVRXEZLU","code":"`+other+`"}`); c != 409 || !enabled() {
		t.Fatalf("已开启时不能换密钥,得 %d", c)
	}
	if c := call("DELETE", `{"password":"pass-1234","code":"`+code+`"}`); c != 200 || enabled() {
		t.Fatalf("密码与验证码都对应能关掉,得 %d", c)
	}
}

// 代理登录同样拒绝跨站请求(登录 CSRF:把受害者的浏览器登进别人的代理账号)。
func TestResellerLoginRejectsCrossSite(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db, sessions: map[string]session{}, loginFails: map[string][]int64{}}
	db.Create(&model.Reseller{Name: "dl", Enabled: true, ClaimBefore: time.Now().Unix() + 3600})
	r := httptest.NewRequest("POST", "http://x/app/api/login", strings.NewReader(`{"username":"dl","password":""}`))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	s.handleResellerLogin(w, r)
	if w.Code != 403 || len(w.Result().Cookies()) != 0 {
		t.Fatalf("跨站登录应被拒且不发 Cookie,得 %d", w.Code)
	}
}

// 主面板重置代理密码时一并关掉外部 API、收回他的全部密钥:盗号者手里的任何一把都随之作废
// (以前只清默认那一把,别的几把在代理重新打开外部 API 后又能用了)。
func TestResellerPasswordResetRevokesAPIToken(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db, sessions: map[string]session{}, totpPendingRS: map[uint]string{}}
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "old-token"}
	db.Create(&rs)
	db.Create(&model.ApiKey{ResellerId: rs.Id, Name: "机器人", Token: "extra-token", Perms: permUsersRead, Enabled: true})
	call := func(tok string) int {
		r := httptest.NewRequest("GET", "http://rs.example:2054/dl/api/v1/ping", nil)
		r.RemoteAddr = "203.0.113.5:4000"
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		s.handleResellerPublicAPI(w, r)
		return w.Code
	}
	if call("old-token") != 200 || call("extra-token") != 200 {
		t.Fatal("前提:两把密钥都可用")
	}
	r := httptest.NewRequest("POST", "/app/api/resellers/"+strconv.FormatUint(uint64(rs.Id), 10)+"/passwd", nil)
	w := httptest.NewRecorder()
	if !s.dispatchResellerSubroute(w, r) || w.Code != 200 {
		t.Fatalf("重置失败: %d %s", w.Code, w.Body.String())
	}
	var got model.Reseller
	db.First(&got, rs.Id)
	var left int64
	db.Model(&model.ApiKey{}).Where("reseller_id = ?", rs.Id).Count(&left)
	if got.ApiEnabled || got.ApiToken != "" || left != 0 {
		t.Fatalf("重置后外部 API 应关掉、密钥一把不留: %+v,还剩 %d 行", got, left)
	}
	// 代理重新设好密码、再打开外部 API:发一把新的默认密钥,以前的两把仍然无效
	if err := s.ensureFirstKey(rs.Id); err != nil {
		t.Fatal(err)
	}
	db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("api_enabled", true)
	db.First(&got, rs.Id)
	if got.ApiToken == "" || got.ApiToken == "old-token" || call(got.ApiToken) != 200 {
		t.Fatalf("重新打开后应有一把新的默认密钥: %q", got.ApiToken)
	}
	if call("old-token") != 401 || call("extra-token") != 401 {
		t.Fatal("重置前的密钥在重新打开外部 API 后不该又能用")
	}
}
