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

// manage 调密钥的管理接口(rid 0 = 主面板,>0 = 那个代理自己)。
func (e *pcEnv) manage(rid uint, method, sub, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://panel.example:2053/app/api/admin/api/"+sub, strings.NewReader(body))
	r.RemoteAddr = "198.51.100.200:5000"
	w := httptest.NewRecorder()
	e.s.handleAPIKeys(w, r, rid, sub, func(string, interface{}) {})
	return w
}

// keys 这个面板的密钥列表。
func (e *pcEnv) keys(rid uint) []apiKeyView {
	e.t.Helper()
	w := e.manage(rid, "GET", "keys", "")
	var out struct {
		Keys []apiKeyView `json:"keys"`
		You  string       `json:"you"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.You != "198.51.100.200" {
		e.t.Fatalf("密钥列表: %d %s", w.Code, w.Body.String())
	}
	return out.Keys
}

// setKey 改一把密钥。
func (e *pcEnv) setKey(rid, id uint, body string) *httptest.ResponseRecorder {
	return e.manage(rid, "PUT", "keys/"+keyID(id), body)
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

// 来源 IP 按密钥限:默认不限;限了以后名单外 403(不计入失败限流),名单里(单个 IP、网段、IPv6)放行;
// 只限了这一把,别的照常;挂在受信任的反向代理后面按转发头认真实来源;代理的密钥同理。
func TestAPIKeyIPList(t *testing.T) {
	e := newPCEnv(t)
	bot := e.mkKey(0, "机器人", permUsersRead)
	shop := e.mkKey(0, "商城", permUsersRead)
	if w := e.keyCall(false, "203.0.113.30:1", bot.Token, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("默认不限来源: %d", w.Code)
	}
	if w := e.setKey(0, bot.Id, `{"ipAllow":true,"ipList":""}`); w.Code != 400 {
		t.Fatalf("空名单不能限来源: %d", w.Code)
	}
	if w := e.setKey(0, bot.Id, `{"ipAllow":true,"ipList":"not-an-ip"}`); w.Code != 400 {
		t.Fatalf("写坏的名单应拒收: %d", w.Code)
	}
	const list = `"ipList":"198.51.100.7, 192.0.2.0/24, 2001:db8::/32"`
	w := e.setKey(0, bot.Id, `{"ipAllow":true,"ipList":"198.51.100.7\n192.0.2.0/24，2001:db8::/32 198.51.100.7"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), list) {
		t.Fatalf("名单应规整去重: %d %s", w.Code, w.Body.String())
	}
	for _, c := range []struct {
		remote string
		want   int
	}{{"198.51.100.7:1", 200}, {"192.0.2.99:1", 200}, {"[2001:db8::5]:1", 200}, {"203.0.113.30:1", 403}, {"[2001:db9::1]:1", 403}} {
		if w := e.keyCall(false, c.remote, bot.Token, "GET", "ping", ""); w.Code != c.want {
			t.Fatalf("来源 %s:%d,应为 %d", c.remote, w.Code, c.want)
		}
	}
	// 只限了这一把:另一把和默认密钥照常
	for _, tok := range []string{shop.Token, pcMaster} {
		if w := e.keyCall(false, "203.0.113.30:1", tok, "GET", "ping", ""); w.Code != 200 {
			t.Fatalf("别的密钥不该跟着被限: %d %s", w.Code, w.Body.String())
		}
	}
	for i := 0; i < 12; i++ {
		e.keyCall(false, "203.0.113.31:1", bot.Token, "GET", "ping", "")
	}
	if e.s.apiAuthBlocked("203.0.113.31") {
		t.Fatal("密钥对、只是来源被挡的,不该计入失败限流")
	}
	// 反向代理后面:不信任时按对端(本机),信任后按 X-Forwarded-For
	if w := e.keyCall(false, "127.0.0.1:9", bot.Token, "GET", "ping", "", "X-Forwarded-For", "198.51.100.7"); w.Code != 403 {
		t.Fatalf("没信任反代时转发头不算数: %d", w.Code)
	}
	e.db.Create(&model.Setting{Key: "trustedProxies", Value: "127.0.0.1"})
	if w := e.keyCall(false, "127.0.0.1:9", bot.Token, "GET", "ping", "", "X-Forwarded-For", "198.51.100.7"); w.Code != 200 {
		t.Fatalf("信任反代后按真实来源放行: %d %s", w.Code, w.Body.String())
	}
	// 关掉限制、名单留着:又谁都能调;再打开不用重填
	w = e.setKey(0, bot.Id, `{"ipAllow":false}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ipAllow":false`) || !strings.Contains(w.Body.String(), list) {
		t.Fatalf("关掉限制时名单应留着: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(false, "203.0.113.30:1", bot.Token, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("关掉限制后应放行: %d", w.Code)
	}
	if w := e.setKey(0, bot.Id, `{"ipAllow":true}`); w.Code != 200 || !strings.Contains(w.Body.String(), list) {
		t.Fatalf("再打开用的是留着的名单: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(false, "203.0.113.30:1", bot.Token, "GET", "ping", ""); w.Code != 403 {
		t.Fatalf("再打开后名单外应 403: %d", w.Code)
	}
	// 建的时候就限
	w = e.manage(0, "POST", "keys", `{"name":"只许一处","perms":["users.read"],"ipAllow":true,"ipList":"198.51.100.9"}`)
	var one model.ApiKey
	if _ = json.Unmarshal(w.Body.Bytes(), &one); w.Code != 200 || one.Token == "" || !one.IpAllow {
		t.Fatalf("建的时候就限来源: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(false, "203.0.113.30:1", one.Token, "GET", "ping", ""); w.Code != 403 {
		t.Fatalf("新建的这把名单外应 403: %d", w.Code)
	}
	if w := e.keyCall(false, "198.51.100.9:1", one.Token, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("新建的这把名单内放行: %d", w.Code)
	}
	if w := e.manage(0, "POST", "keys", `{"name":"空名单","perms":["users.read"],"ipAllow":true}`); w.Code != 400 {
		t.Fatalf("建的时候限来源却没给名单应拒收: %d", w.Code)
	}
	// 代理的密钥同理,只限它自己这一把
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-default-token-abcdefghij"}
	e.db.Create(&rs)
	rk := e.mkKey(rs.Id, "代理机器人", permUsersRead)
	if w := e.setKey(rs.Id, rk.Id, `{"ipAllow":true,"ipList":"203.0.113.41"}`); w.Code != 200 {
		t.Fatalf("代理限来源: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(true, "203.0.113.40:1", rk.Token, "GET", "ping", ""); w.Code != 403 {
		t.Fatalf("代理这把名单外应 403: %d", w.Code)
	}
	if w := e.keyCall(true, "203.0.113.41:1", rk.Token, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("代理这把名单内放行: %d", w.Code)
	}
	if w := e.keyCall(true, "203.0.113.40:1", rs.ApiToken, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("代理的默认密钥没限: %d", w.Code)
	}
}

// 升级前就有的那一把在列表里是第一行,和别的一样能改:改名、收权限、停用、重新生成、删除;
// 它的令牌仍在原处(设置 apiToken),旧版里换了那边,这边跟着认,配置还是它那一行的。
func TestAPIDefaultKey(t *testing.T) {
	e := newPCEnv(t)
	e.mkUser(model.User{Name: "alice"}, 1)
	src := "203.0.113.60:1"
	ks := e.keys(0)
	if len(ks) != 1 || !ks[0].IsDefault || ks[0].Token != pcMaster || ks[0].Name != "" || len(ks[0].PermList) != len(apiPerms) || !ks[0].Enabled || ks[0].IpAllow {
		t.Fatalf("默认密钥应是列表里的一行:全部权限、启用、不限来源: %+v", ks)
	}
	def := ks[0]
	if again := e.keys(0); len(again) != 1 || again[0].Id != def.Id {
		t.Fatalf("反复读列表不该多出行: %+v", again)
	}
	other := e.mkKey(0, "机器人", permUsersRead)
	if ks = e.keys(0); len(ks) != 2 || !ks[0].IsDefault || ks[1].Id != other.Id || ks[1].IsDefault {
		t.Fatalf("默认密钥排第一,其余在后: %+v", ks)
	}
	// 行里存的是占位串,不是真正的令牌;占位串不能拿来调
	var row model.ApiKey
	e.db.First(&row, def.Id)
	if row.Token == "" || row.Token == pcMaster {
		t.Fatalf("默认密钥那一行不该存真正的令牌: %q", row.Token)
	}
	if w := e.keyCall(false, src, row.Token, "GET", "ping", ""); w.Code != 401 {
		t.Fatalf("占位串不能当令牌用: %d", w.Code)
	}
	// 起名 + 只留查看:建号 403;令牌没变
	w := e.setKey(0, def.Id, `{"name":"商城","perms":["users.read","plans.read"]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"token":"`+pcMaster+`"`) || !strings.Contains(w.Body.String(), `"isDefault":true`) {
		t.Fatalf("改默认密钥: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(false, src, pcMaster, "POST", "users", `{"name":"bob"}`); w.Code != 403 || !strings.Contains(w.Body.String(), `"permission":"`+permUsersCreate+`"`) {
		t.Fatalf("收了权限的默认密钥建号应 403: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(false, src, pcMaster, "GET", "users", ""); w.Code != 200 {
		t.Fatalf("留着的权限照常: %d", w.Code)
	}
	if sc, ok, _ := e.s.adminAPIAuth(authReq(pcMaster)); !ok || sc.actor != "api:商城" {
		t.Fatalf("起了名的默认密钥,审计里带名字: %+v", sc)
	}
	e.db.First(&row, def.Id)
	if row.LastUsedAt == 0 || row.LastUsedIP != "203.0.113.60" {
		t.Fatalf("默认密钥也记最近使用: %+v", row)
	}
	// 名字不能和别的重;可以清空(界面上就叫「默认密钥」),别的密钥不行
	if w := e.setKey(0, def.Id, `{"name":"机器人"}`); w.Code != 400 {
		t.Fatalf("重名应拒收: %d", w.Code)
	}
	if w := e.setKey(0, other.Id, `{"name":""}`); w.Code != 400 {
		t.Fatalf("别的密钥名字不能空: %d", w.Code)
	}
	if w := e.setKey(0, def.Id, `{"name":"","perms":["users.read","plans.read"]}`); w.Code != 200 {
		t.Fatalf("默认密钥可以不起名: %d %s", w.Code, w.Body.String())
	}
	if sc, ok, _ := e.s.adminAPIAuth(authReq(pcMaster)); !ok || sc.actor != "api" {
		t.Fatalf("没起名的默认密钥,审计里和以前一样记 api: %+v", sc)
	}
	// 停用:401,不计入限流;别的密钥不受影响
	if w := e.setKey(0, def.Id, `{"enabled":false}`); w.Code != 200 {
		t.Fatalf("停用默认密钥: %d %s", w.Code, w.Body.String())
	}
	for i := 0; i < 12; i++ {
		if w := e.keyCall(false, "203.0.113.61:1", pcMaster, "GET", "ping", ""); w.Code != 401 {
			t.Fatalf("停用的默认密钥应 401: %d", w.Code)
		}
	}
	if e.s.apiAuthBlocked("203.0.113.61") {
		t.Fatal("停用的默认密钥失败不该计入限流")
	}
	if w := e.keyCall(false, src, other.Token, "GET", "users", ""); w.Code != 200 {
		t.Fatalf("别的密钥不受影响: %d", w.Code)
	}
	if w := e.setKey(0, def.Id, `{"enabled":true}`); w.Code != 200 {
		t.Fatalf("启用: %d", w.Code)
	}
	// 重新生成:设置里的令牌换了,旧的 401,新的能用,配置不变
	w = e.manage(0, "POST", "keys/"+keyID(def.Id)+"/rotate", "")
	var nk apiKeyView
	if _ = json.Unmarshal(w.Body.Bytes(), &nk); w.Code != 200 || nk.Token == "" || nk.Token == pcMaster || e.s.setting("apiToken") != nk.Token {
		t.Fatalf("重新生成默认密钥: %d %s", w.Code, w.Body.String())
	}
	if w := e.keyCall(false, src, pcMaster, "GET", "ping", ""); w.Code != 401 {
		t.Fatalf("旧令牌应立即失效: %d", w.Code)
	}
	if w := e.keyCall(false, src, nk.Token, "POST", "users", `{"name":"bob"}`); w.Code != 403 {
		t.Fatalf("新令牌沿用原来的权限: %d", w.Code)
	}
	// 回退到旧版期间在旧界面重新生成(只动了设置):这边跟着认新的,旧的不再认
	e.db.Model(&model.Setting{}).Where("key = ?", "apiToken").Update("value", "rotated-by-old-version-0123")
	if w := e.keyCall(false, src, nk.Token, "GET", "ping", ""); w.Code != 401 {
		t.Fatalf("旧版里换掉的令牌不该还能用: %d", w.Code)
	}
	if w := e.keyCall(false, src, "rotated-by-old-version-0123", "GET", "users", ""); w.Code != 200 {
		t.Fatalf("旧版里换的新令牌应能用: %d", w.Code)
	}
	if ks = e.keys(0); len(ks) != 2 || ks[0].Token != "rotated-by-old-version-0123" || ks[0].Id != def.Id {
		t.Fatalf("列表里默认密钥显示的是现在的令牌,还是同一行: %+v", ks)
	}
	// 删除:设置里的令牌清空,立即失效,列表里没有它了;别的密钥还在
	if w := e.manage(0, "DELETE", "keys/"+keyID(def.Id), ""); w.Code != 200 {
		t.Fatalf("删默认密钥: %d %s", w.Code, w.Body.String())
	}
	if e.s.setting("apiToken") != "" {
		t.Fatal("删了默认密钥,设置里的令牌应清空(回退到旧版也不该再认)")
	}
	if w := e.keyCall(false, src, "rotated-by-old-version-0123", "GET", "ping", ""); w.Code != 401 {
		t.Fatalf("删了以后应 401: %d", w.Code)
	}
	if ks = e.keys(0); len(ks) != 1 || ks[0].Id != other.Id {
		t.Fatalf("列表里只剩别的密钥: %+v", ks)
	}
	if w := e.setKey(0, def.Id, `{"name":"x"}`); w.Code != 404 {
		t.Fatalf("删了的密钥再改应 404: %d", w.Code)
	}
}

func authReq(tok string) *http.Request {
	r := httptest.NewRequest("GET", "http://panel.example:2053/app/api/v1/ping", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

// 升级:旧版「整个面板一份」的来源 IP 白名单,默认密钥第一次用到时照它限(升级前后放行的来源一样);
// 之后各改各的,旧设置留着(回退到旧版照旧生效)但不再管新版;新建的密钥不受它影响。代理同理。
func TestAPIDefaultKeyAdoptsLegacyIPList(t *testing.T) {
	e := newPCEnv(t)
	e.db.Create(&model.Setting{Key: "apiIpAllow", Value: "true"})
	e.db.Create(&model.Setting{Key: "apiIpList", Value: "198.51.100.7"})
	if w := e.keyCall(false, "203.0.113.70:1", pcMaster, "GET", "ping", ""); w.Code != 403 {
		t.Fatalf("升级后第一次调用就该按旧白名单挡: %d", w.Code)
	}
	if w := e.keyCall(false, "198.51.100.7:1", pcMaster, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("旧白名单里的来源照常: %d", w.Code)
	}
	ks := e.keys(0)
	if len(ks) != 1 || !ks[0].IpAllow || ks[0].IpList != "198.51.100.7" {
		t.Fatalf("默认密钥应带着旧白名单: %+v", ks)
	}
	nk := e.mkKey(0, "新的", permUsersRead)
	if w := e.keyCall(false, "203.0.113.70:1", nk.Token, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("新建的密钥不受旧白名单影响: %d", w.Code)
	}
	if w := e.setKey(0, ks[0].Id, `{"ipAllow":false}`); w.Code != 200 {
		t.Fatalf("放开默认密钥: %d", w.Code)
	}
	if w := e.keyCall(false, "203.0.113.70:1", pcMaster, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("放开后不该再被旧设置挡: %d", w.Code)
	}
	if e.s.setting("apiIpAllow") != "true" || e.s.setting("apiIpList") != "198.51.100.7" {
		t.Fatal("旧版的白名单设置应留着(回退用)")
	}
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-default-token-abcdefghij", ApiIpAllow: true, ApiIpList: "203.0.113.41"}
	e.db.Create(&rs)
	if w := e.keyCall(true, "203.0.113.40:1", rs.ApiToken, "GET", "ping", ""); w.Code != 403 {
		t.Fatalf("代理的默认密钥按代理的旧白名单挡: %d", w.Code)
	}
	if w := e.keyCall(true, "203.0.113.41:1", rs.ApiToken, "GET", "ping", ""); w.Code != 200 {
		t.Fatalf("代理旧白名单里的来源照常: %d", w.Code)
	}
	if rk := e.keys(rs.Id); len(rk) != 1 || !rk[0].IsDefault || rk[0].Token != rs.ApiToken || rk[0].IpList != "203.0.113.41" {
		t.Fatalf("代理的默认密钥: %+v", rk)
	}
}

// 打开外部 API:一把密钥都没有时生成默认密钥(全部权限、不限来源);已经有密钥就不动;
// 以前那把默认密钥留下的配置不带到新的一把上。
func TestAPIFirstKeyOnEnable(t *testing.T) {
	e := newPCEnv(t)
	e.db.Where("key IN ?", []string{"apiEnabled", "apiToken"}).Delete(&model.Setting{})
	if ks := e.keys(0); len(ks) != 0 {
		t.Fatalf("没开过外部 API 的面板不该有密钥: %+v", ks)
	}
	toggle := func(on bool) {
		t.Helper()
		body := `{"enabled":false}`
		if on {
			body = `{"enabled":true}`
		}
		w := httptest.NewRecorder()
		e.s.apiToggle(w, httptest.NewRequest("POST", "http://panel.example:2053/app/api/admin/api", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("开关外部 API: %d %s", w.Code, w.Body.String())
		}
	}
	src := "203.0.113.80:1"
	toggle(true)
	ks := e.keys(0)
	if len(ks) != 1 || !ks[0].IsDefault || len(ks[0].Token) != 64 || ks[0].Token != e.s.setting("apiToken") || len(ks[0].PermList) != len(apiPerms) || ks[0].IpAllow {
		t.Fatalf("第一次打开应生成默认密钥: %+v", ks)
	}
	first := ks[0]
	if w := e.keyCall(false, src, first.Token, "GET", "users", ""); w.Code != 200 {
		t.Fatalf("开了就能用: %d %s", w.Code, w.Body.String())
	}
	toggle(false)
	if w := e.keyCall(false, src, first.Token, "GET", "users", ""); w.Code != 401 {
		t.Fatalf("关了应 401: %d", w.Code)
	}
	toggle(true)
	if ks = e.keys(0); len(ks) != 1 || ks[0].Token != first.Token || ks[0].Id != first.Id {
		t.Fatalf("关了再开不换密钥: %+v", ks)
	}
	// 删了默认那把、还留着别的:再开不会又冒出一把
	other := e.mkKey(0, "机器人", permUsersRead)
	if w := e.manage(0, "DELETE", "keys/"+keyID(first.Id), ""); w.Code != 200 {
		t.Fatalf("删默认密钥: %d", w.Code)
	}
	toggle(false)
	toggle(true)
	if ks = e.keys(0); len(ks) != 1 || ks[0].Id != other.Id {
		t.Fatalf("还有别的密钥时不该再生成默认密钥: %+v", ks)
	}
	// 全删光再开:重新生成一把
	if w := e.manage(0, "DELETE", "keys/"+keyID(other.Id), ""); w.Code != 200 {
		t.Fatalf("删密钥: %d", w.Code)
	}
	toggle(false)
	toggle(true)
	if ks = e.keys(0); len(ks) != 1 || !ks[0].IsDefault || ks[0].Token == first.Token {
		t.Fatalf("一把都没有时再开应重新生成: %+v", ks)
	}
	// 默认密钥改过名、限过来源,后来令牌被清掉(旧版里重置):再生成的是干净的一把,不带以前的配置
	if w := e.setKey(0, ks[0].Id, `{"name":"老的","ipAllow":true,"ipList":"198.51.100.7","perms":["users.read"]}`); w.Code != 200 {
		t.Fatalf("改默认密钥: %d %s", w.Code, w.Body.String())
	}
	e.db.Model(&model.Setting{}).Where("key = ?", "apiToken").Update("value", "")
	if ks = e.keys(0); len(ks) != 0 {
		t.Fatalf("令牌没了的默认密钥不该还在列表里: %+v", ks)
	}
	toggle(true)
	if ks = e.keys(0); len(ks) != 1 || ks[0].Name != "" || ks[0].IpAllow || ks[0].IpList != "" || len(ks[0].PermList) != len(apiPerms) {
		t.Fatalf("重新生成的默认密钥不该带着以前的配置: %+v", ks)
	}
	var rows int64
	e.db.Model(&model.ApiKey{}).Count(&rows)
	if rows != 1 {
		t.Fatalf("以前那一行应清掉,只剩新的一行: %d", rows)
	}
}

// 删代理:他的密钥记录一起删。
func TestAPIKeysGoWithReseller(t *testing.T) {
	e := newPCEnv(t)
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "rs-default-token-abcdefghij"}
	e.db.Create(&rs)
	e.mkKey(rs.Id, "代理机器人", permUsersRead)
	keep := e.mkKey(0, "主面板的", permUsersRead)
	if ks := e.keys(rs.Id); len(ks) != 2 {
		t.Fatalf("前提:代理有默认密钥和一把别的: %+v", ks)
	}
	w := httptest.NewRecorder()
	e.s.handleResellerItem(w, httptest.NewRequest("DELETE", "http://x/app/api/resellers/"+keyID(rs.Id), nil))
	if w.Code != 200 {
		t.Fatalf("删代理: %d %s", w.Code, w.Body.String())
	}
	var left []model.ApiKey
	e.db.Find(&left)
	for _, k := range left {
		if k.ResellerId != 0 {
			t.Fatalf("代理的密钥应一起删掉: %+v", k)
		}
	}
	if w := e.keyCall(false, "203.0.113.90:1", keep.Token, "GET", "users", ""); w.Code != 200 {
		t.Fatalf("主面板的密钥不受影响: %d", w.Code)
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
