package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// keyCall 用某个来源地址、某把令牌调外部 API(主面板入口或代理入口)。
func (e *pcEnv) keyCall(reseller bool, remote, token, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	url, h := "http://panel.example:2053/app/api/v1/"+path, e.s.handlePublicAPI
	if reseller {
		url, h = "http://rs.example:2054/dl/api/v1/"+path, e.s.handleResellerPublicAPI
	}
	r := httptest.NewRequest(method, url, strings.NewReader(body))
	r.RemoteAddr = remote
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// manage 调附加密钥 / 白名单的管理接口(rid 0 = 主面板,>0 = 那个代理自己)。
func (e *pcEnv) manage(rid uint, method, sub, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://panel.example:2053/app/api/admin/api/"+sub, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.200:5000"
	w := httptest.NewRecorder()
	e.s.handleAPIKeys(w, r, rid, sub, func(string, interface{}) {})
	return w
}

func (e *pcEnv) mkKey(rid uint, name string, perms ...string) model.ApiKey {
	e.t.Helper()
	b, _ := json.Marshal(map[string]interface{}{"name": name, "perms": perms})
	w := e.manage(rid, "POST", "keys", string(b))
	if w.Code != 200 {
		e.t.Fatalf("建密钥 %s: %d %s", name, w.Code, w.Body.String())
	}
	var k model.ApiKey
	_ = json.Unmarshal(w.Body.Bytes(), &k)
	if k.Token == "" || k.Id == 0 {
		e.t.Fatalf("建密钥没返回令牌: %s", w.Body.String())
	}
	return k
}

// 多把密钥按权限放行:只读的查得了用户、建不了号、续不了费、看不了面板信息;没权限是 403 并写明缺哪项;
// 默认密钥(原来那一把)仍然全部权限。停用的密钥、总开关关着都是 401。
func TestAPIKeyPermissions(t *testing.T) {
	e := newPCEnv(t)
	e.mkUser(model.User{Name: "alice"}, 1)
	ro := e.mkKey(0, "TG 机器人", permUsersRead, permPanelRead)
	shop := e.mkKey(0, "商城", permUsersRead, permUsersCreate, permBilling, permPlansRead)
	src := "203.0.113.10:4000"

	for _, c := range []struct {
		tok, method, path, body string
		want                    int
		perm                    string
	}{
		{ro.Token, "GET", "users", "", 200, ""},
		{ro.Token, "GET", "users/alice", "", 200, ""},
		{ro.Token, "GET", "users/alice/traffic?days=7", "", 200, ""},
		{ro.Token, "GET", "overview", "", 200, ""},
		{ro.Token, "GET", "lines", "", 200, ""},
		{ro.Token, "GET", "onlines", "", 200, ""},
		{ro.Token, "GET", "servers", "", 200, ""},
		{ro.Token, "GET", "ping", "", 200, ""},
		{ro.Token, "POST", "users", `{"name":"bob"}`, 403, permUsersCreate},
		{ro.Token, "POST", "users/alice/renew", `{"days":30}`, 403, permBilling},
		{ro.Token, "POST", "users/alice/disable", "", 403, permUsersUpdate},
		{ro.Token, "DELETE", "users/alice", "", 403, permUsersDelete},
		{ro.Token, "GET", "plans", "", 403, permPlansRead},
		{shop.Token, "GET", "plans", "", 200, ""},
		{shop.Token, "POST", "users", `{"name":"carol"}`, 200, ""},
		{shop.Token, "GET", "overview", "", 403, permPanelRead},
		{shop.Token, "PATCH", "users/alice", `{"remark":"x"}`, 403, permUsersUpdate},
		{pcMaster, "GET", "overview", "", 200, ""},
		{pcMaster, "GET", "plans", "", 200, ""},
	} {
		w := e.keyCall(false, src, c.tok, c.method, c.path, c.body)
		if w.Code != c.want {
			t.Fatalf("%s %s(%s):%d,应为 %d: %s", c.method, c.path, c.tok[:6], w.Code, c.want, w.Body.String())
		}
		if c.perm != "" && !strings.Contains(w.Body.String(), `"permission":"`+c.perm+`"`) {
			t.Fatalf("%s %s:403 里应写明缺 %s: %s", c.method, c.path, c.perm, w.Body.String())
		}
	}
	// 最近使用:记了时间和来源
	var k model.ApiKey
	e.db.First(&k, ro.Id)
	if k.LastUsedAt == 0 || k.LastUsedIP != "203.0.113.10" {
		t.Fatalf("最近使用没记上: %+v", k)
	}
	// 停用这把:401,而且不算猜令牌(不计入失败限流)
	if w := e.manage(0, "PUT", "keys/"+keyID(ro.Id), `{"enabled":false}`); w.Code != 200 {
		t.Fatalf("停用: %d %s", w.Code, w.Body.String())
	}
	for i := 0; i < 12; i++ {
		if w := e.keyCall(false, "203.0.113.11:4000", ro.Token, "GET", "users", ""); w.Code != 401 {
			t.Fatalf("停用的密钥应 401: %d", w.Code)
		}
	}
	if e.s.apiAuthBlocked("203.0.113.11") {
		t.Fatal("对得上某把密钥(只是停用了)的失败不该计入限流")
	}
	// 总开关关着:附加密钥也不能用
	e.db.Model(&model.Setting{}).Where("key = ?", "apiEnabled").Update("value", "false")
	if w := e.keyCall(false, src, shop.Token, "GET", "plans", ""); w.Code != 401 {
		t.Fatalf("总开关关着时附加密钥应 401: %d", w.Code)
	}
}

// 代理的附加密钥只在代理入口有效、只看自己名下;服务器列表不给代理看;主面板入口不认它,反过来也一样。
func TestAPIKeyResellerScope(t *testing.T) {
	e := newPCEnv(t)
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-default-token-0123456789"}
	e.db.Create(&rs)
	e.db.Create(&model.ResellerLine{ResellerId: rs.Id, LineId: 1})
	e.mkUser(model.User{Name: "mine", ResellerId: rs.Id}, 1)
	e.mkUser(model.User{Name: "other"}, 1)
	k := e.mkKey(rs.Id, "代理的机器人", permUsersRead, permPanelRead)
	adminKey := e.mkKey(0, "主面板只读", permUsersRead)
	src := "203.0.113.20:4000"

	w := e.keyCall(true, src, k.Token, "GET", "users", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mine"`) || strings.Contains(w.Body.String(), `"other"`) {
		t.Fatalf("代理密钥只该看到自己名下: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(true, src, k.Token, "GET", "users/other", ""); w.Code != 404 {
		t.Fatalf("别人的用户应 404: %d", w.Code)
	}
	if w := e.keyCall(true, src, k.Token, "GET", "servers", ""); w.Code != 403 {
		t.Fatalf("代理的密钥看不了服务器: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(true, src, k.Token, "GET", "overview", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("代理的概况只算自己名下: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(false, src, k.Token, "GET", "users", ""); w.Code != 401 {
		t.Fatalf("主面板入口不认代理的密钥: %d", w.Code)
	}
	if w := e.keyCall(true, src, adminKey.Token, "GET", "users", ""); w.Code != 401 {
		t.Fatalf("代理入口不认主面板的密钥: %d", w.Code)
	}
	// 代理不能动主面板的密钥(按 id 也不行)
	if w := e.manage(rs.Id, "DELETE", "keys/"+keyID(adminKey.Id), ""); w.Code != 404 {
		t.Fatalf("代理删主面板的密钥应 404: %d", w.Code)
	}
	// 代理关了外部 API / 被停用:它的附加密钥一起失效
	e.db.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("api_enabled", false)
	if w := e.keyCall(true, src, k.Token, "GET", "users", ""); w.Code != 401 {
		t.Fatalf("代理关了外部 API,附加密钥应 401: %d", w.Code)
	}
}

// 来源 IP 白名单:默认关;打开后名单外 403(不计入失败限流),名单里(单个 IP、网段、IPv6)放行;
// 挂在受信任的反向代理后面按转发头认真实来源;主面板和代理各一份。
func TestAPIIPAllowlist(t *testing.T) {
	e := newPCEnv(t)
	if w := e.keyCall(false, "203.0.113.30:1", pcMaster, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("默认关:谁都能调: %d", w.Code)
	}
	if w := e.manage(0, "PUT", "ip", `{"enabled":true,"list":""}`); w.Code != 400 {
		t.Fatalf("空名单不能打开: %d", w.Code)
	}
	if w := e.manage(0, "PUT", "ip", `{"enabled":true,"list":"not-an-ip"}`); w.Code != 400 {
		t.Fatalf("写坏的名单应拒收: %d", w.Code)
	}
	w := e.manage(0, "PUT", "ip", `{"enabled":true,"list":"198.51.100.7\n192.0.2.0/24，2001:db8::/32 198.51.100.7"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"list":"198.51.100.7, 192.0.2.0/24, 2001:db8::/32"`) {
		t.Fatalf("名单应规整去重: %d %s", w.Code, w.Body.String())
	}
	for _, c := range []struct {
		remote string
		want   int
	}{{"198.51.100.7:1", 200}, {"192.0.2.99:1", 200}, {"[2001:db8::5]:1", 200}, {"203.0.113.30:1", 403}, {"[2001:db9::1]:1", 403}} {
		if w := e.keyCall(false, c.remote, pcMaster, "GET", "ping", ""); w.Code != c.want {
			t.Fatalf("来源 %s:%d,应为 %d", c.remote, w.Code, c.want)
		}
	}
	for i := 0; i < 12; i++ {
		e.keyCall(false, "203.0.113.31:1", pcMaster, "GET", "ping", "")
	}
	if e.s.apiAuthBlocked("203.0.113.31") {
		t.Fatal("白名单挡下的不该计入失败限流")
	}
	// 反向代理后面:不信任时按对端(本机),信任后按 X-Forwarded-For
	if w := e.keyCall(false, "127.0.0.1:9", pcMaster, "GET", "ping", "", "X-Forwarded-For", "198.51.100.7"); w.Code != 403 {
		t.Fatalf("没信任反代时转发头不算数: %d", w.Code)
	}
	e.db.Create(&model.Setting{Key: "trustedProxies", Value: "127.0.0.1"})
	if w := e.keyCall(false, "127.0.0.1:9", pcMaster, "GET", "ping", "", "X-Forwarded-For", "198.51.100.7"); w.Code != 200 {
		t.Fatalf("信任反代后按真实来源放行: %d %s", w.Code, w.Body.String())
	}
	// 代理的白名单单独一份:主面板开了不影响代理
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-default-token-abcdefghij"}
	e.db.Create(&rs)
	if w := e.keyCall(true, "203.0.113.40:1", rs.ApiToken, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("代理没开白名单: %d", w.Code)
	}
	if w := e.manage(rs.Id, "PUT", "ip", `{"enabled":true,"list":"203.0.113.41"}`); w.Code != 200 {
		t.Fatalf("代理设白名单: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(true, "203.0.113.40:1", rs.ApiToken, "GET", "ping", ""); w.Code != 403 {
		t.Fatalf("代理白名单外应 403: %d", w.Code)
	}
	if w := e.keyCall(true, "203.0.113.41:1", rs.ApiToken, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("代理白名单内放行: %d", w.Code)
	}
}

// 管理:名字必填不重名、至少一项权限、不认识的权限拒收;重新生成后旧令牌立即失效;删除后 401。
func TestAPIKeyManage(t *testing.T) {
	e := newPCEnv(t)
	for _, body := range []string{`{"name":"","perms":["users.read"]}`, `{"name":"x","perms":[]}`, `{"name":"x","perms":["users.read","root"]}`} {
		if w := e.manage(0, "POST", "keys", body); w.Code != 400 {
			t.Fatalf("%s 应拒收: %d", body, w.Code)
		}
	}
	k := e.mkKey(0, "商城", permUsersRead)
	if w := e.manage(0, "POST", "keys", `{"name":"商城","perms":["users.read"]}`); w.Code != 400 {
		t.Fatalf("重名应拒收: %d", w.Code)
	}
	w := e.manage(0, "POST", "keys/"+keyID(k.Id)+"/rotate", "")
	var nk model.ApiKey
	_ = json.Unmarshal(w.Body.Bytes(), &nk)
	if w.Code != 200 || nk.Token == "" || nk.Token == k.Token {
		t.Fatalf("重新生成: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(false, "203.0.113.50:1", k.Token, "GET", "users", ""); w.Code != 401 {
		t.Fatalf("旧令牌应立即失效: %d", w.Code)
	}
	if w := e.keyCall(false, "203.0.113.50:1", nk.Token, "GET", "users", ""); w.Code != 200 {
		t.Fatalf("新令牌应能用: %d", w.Code)
	}
	if w := e.manage(0, "GET", "keys", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"permList":["users.read"]`) {
		t.Fatalf("列表: %d %s", w.Code, w.Body.String())
	}
	if w := e.manage(0, "DELETE", "keys/"+keyID(k.Id), ""); w.Code != 200 {
		t.Fatalf("删除: %d", w.Code)
	}
	if w := e.keyCall(false, "203.0.113.50:1", nk.Token, "GET", "users", ""); w.Code != 401 {
		t.Fatalf("删了以后应 401: %d", w.Code)
	}
}

func keyID(id uint) string {
	b, _ := json.Marshal(id)
	return string(b)
}

// 接口与权限的对应是一张表:每个路由都要落到确定的权限上(漏了就等于只读密钥也能写)。
func TestAPIPermForEveryRoute(t *testing.T) {
	for _, c := range []struct {
		m, p, want string
	}{
		{http.MethodGet, "ping", ""}, {http.MethodGet, "plans", permPlansRead}, {http.MethodGet, "overview", permPanelRead},
		{http.MethodGet, "servers", permPanelRead}, {http.MethodGet, "lines", permPanelRead}, {http.MethodGet, "onlines", permPanelRead},
		{http.MethodGet, "users", permUsersRead}, {http.MethodPost, "users", permUsersCreate},
		{http.MethodGet, "users/a", permUsersRead}, {http.MethodPatch, "users/a", permUsersUpdate}, {http.MethodPut, "users/a", permUsersUpdate}, {http.MethodDelete, "users/a", permUsersDelete},
		{http.MethodPost, "users/a/enable", permUsersUpdate}, {http.MethodPost, "users/a/disable", permUsersUpdate}, {http.MethodPost, "users/a/reset", permUsersUpdate},
		{http.MethodPost, "users/a/kick", permUsersUpdate}, {http.MethodPost, "users/a/rotate", permUsersUpdate},
		{http.MethodPost, "users/a/plan", permBilling}, {http.MethodPost, "users/a/renew", permBilling}, {http.MethodPost, "users/a/change", permBilling},
		{http.MethodGet, "users/a/plan-ops", permUsersRead}, {http.MethodGet, "users/a/sub", permUsersRead}, {http.MethodGet, "users/a/traffic", permUsersRead},
		{http.MethodPost, "users/a/whatever", permUsersUpdate},
	} {
		if got := apiPermFor(c.m, strings.Split(c.p, "/")); got != c.want {
			t.Errorf("%s %s → %q,应为 %q", c.m, c.p, got, c.want)
		}
	}
}
