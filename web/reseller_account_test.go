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

// 主面板重置代理密码时一并关掉外部 API、清空令牌:盗号者手里的旧令牌随之作废。
func TestResellerPasswordResetRevokesAPIToken(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db, sessions: map[string]session{}, totpPendingRS: map[uint]string{}}
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "old-token"}
	db.Create(&rs)
	if _, ok := s.resellerByAPIToken("old-token"); !ok {
		t.Fatal("前提:旧令牌可用")
	}
	r := httptest.NewRequest("POST", "/app/api/resellers/"+strconv.FormatUint(uint64(rs.Id), 10)+"/passwd", nil)
	w := httptest.NewRecorder()
	if !s.dispatchResellerSubroute(w, r) || w.Code != 200 {
		t.Fatalf("重置失败: %d %s", w.Code, w.Body.String())
	}
	var got model.Reseller
	db.First(&got, rs.Id)
	if _, ok := s.resellerByAPIToken("old-token"); ok || got.ApiEnabled || got.ApiToken != "" {
		t.Fatalf("重置后旧 API 令牌应失效: %+v", got)
	}
}
