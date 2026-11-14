package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
	"gorm.io/gorm"
)

func newAttentionServer(t *testing.T) (*Server, *gorm.DB) {
	t.Helper()
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(run.DB()) })
	return NewServer(run), run.DB()
}

// setSetting 写一个设置(有就改、没有就加)
func setSetting(t *testing.T, s *Server, k, v string) {
	t.Helper()
	if err := s.run.SetSetting(k, v); err != nil {
		t.Fatal(err)
	}
}

// createAll 逐个建;gorm 的 default:true 会把 false 写成 true(建完还会把结构体里的值回填成 true),要停用的建完再改回来
func createUsers(t *testing.T, db *gorm.DB, list []model.User) {
	t.Helper()
	for _, u := range list {
		enabled := u.Enabled
		if err := db.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
		if !enabled {
			db.Model(&model.User{}).Where("id = ?", u.Id).Update("enabled", false)
		}
	}
}

const gib = int64(1) << 30

// 「需要处理」的用户计数:只算直属、启用的;快到期按 N 天(不含已过期),流量按已用 ≥ P%(只算限量的)。
func TestAttentionUserCounts(t *testing.T) {
	s, db := newAttentionServer(t)
	now := time.Now().Unix()
	rs := model.Reseller{Name: "dl", Enabled: true}
	db.Create(&rs)
	day := int64(86400)
	users := []model.User{
		{Name: "exp-2d", Enabled: true, Expiry: now + 2*day},                             // 算
		{Name: "exp-edge", Enabled: true, Expiry: now + 3*day - 60},                      // 算:还在 3 天内
		{Name: "exp-5d", Enabled: true, Expiry: now + 5*day},                             // 不算:超过 3 天
		{Name: "expired", Enabled: true, Expiry: now - day},                              // 不算:已过期的另有状态
		{Name: "never", Enabled: true},                                                   // 不算:不限期
		{Name: "off-exp", Enabled: false, Expiry: now + day},                             // 不算:停用的
		{Name: "rs-exp", Enabled: true, Expiry: now + day, ResellerId: rs.Id},            // 不算:代理名下的
		{Name: "q-80", Enabled: true, Volume: 100 * gib, Up: 30 * gib, Down: 50 * gib},   // 算:正好 80%
		{Name: "q-79", Enabled: true, Volume: 100 * gib, Up: 29 * gib, Down: 50 * gib},   // 不算
		{Name: "q-over", Enabled: true, Volume: 10 * gib, Up: 20 * gib},                  // 算:超量的也是快用完
		{Name: "q-unlim", Enabled: true, Up: 500 * gib},                                  // 不算:不限量
		{Name: "q-off", Enabled: false, Volume: 10 * gib, Up: 20 * gib},                  // 不算:停用的
		{Name: "q-rs", Enabled: true, Volume: 10 * gib, Up: 20 * gib, ResellerId: rs.Id}, // 不算:代理名下的
	}
	createUsers(t, db, users)
	v := s.computeAttention(time.Now())
	if v.ExpiringDays != 3 || v.QuotaPct != 80 {
		t.Fatalf("默认阈值应是 3 天 / 80%%: %+v", v)
	}
	if v.UsersExpiring != 2 {
		t.Fatalf("快到期应只算直属、启用、3 天内未过期的 2 个,得到 %d", v.UsersExpiring)
	}
	if v.UsersQuota != 2 {
		t.Fatalf("流量将尽应只算直属、启用、限量且 ≥80%% 的 2 个,得到 %d", v.UsersQuota)
	}

	// 阈值跟 Telegram 预警同一组设置;填 0 天时按 3 天
	setSetting(t, s, "tgExpiringDays", "6")
	setSetting(t, s, "tgQuotaPercent", "79")
	v = s.computeAttention(time.Now())
	if v.ExpiringDays != 6 || v.UsersExpiring != 3 || v.QuotaPct != 79 || v.UsersQuota != 3 {
		t.Fatalf("改阈值后:6 天内 3 个、≥79%% 3 个,得到 %+v", v)
	}
	setSetting(t, s, "tgExpiringDays", "0")
	if v = s.computeAttention(time.Now()); v.ExpiringDays != 3 {
		t.Fatalf("填 0 天时应按 3 天: %d", v.ExpiringDays)
	}
}

// 代理(只算启用的):额度用尽;7 天内要到期的;过去 7 天里刚到期的(更早到期的不再常驻,停用的、不限期的不算)。
func TestAttentionResellerCounts(t *testing.T) {
	s, db := newAttentionServer(t)
	now := time.Now().Unix()
	list := []model.Reseller{
		{Name: "a", Enabled: true, Depleted: true},
		{Name: "b", Enabled: true, Expiry: now + 3*86400},
		{Name: "c", Enabled: true, Expiry: now - 86400}, // 刚到期
		{Name: "d", Enabled: true, Expiry: now + 30*86400},
		{Name: "e", Enabled: false, Expiry: now + 86400},
		{Name: "f", Enabled: true},
		{Name: "g", Enabled: true, Expiry: now - 30*86400}, // 一个月前就到期了:不再常驻提示
		{Name: "h", Enabled: false, Depleted: true},        // 停用的:不算
	}
	for _, r := range list {
		enabled := r.Enabled
		db.Create(&r)
		if !enabled {
			db.Model(&model.Reseller{}).Where("id = ?", r.Id).Update("enabled", false)
		}
	}
	v := s.computeAttention(time.Now())
	if v.ResellersDepleted != 1 || v.ResellersExpiring != 1 || v.ResellersExpired != 1 {
		t.Fatalf("额度用尽 1 个、7 天内到期 1 个、刚到期 1 个,得到 %+v", v)
	}
}

// 定时备份:backupHour ≥ 0 才算开着;没填(默认关)不算
func TestAttentionBackupAuto(t *testing.T) {
	s, _ := newAttentionServer(t)
	if v := s.computeAttention(time.Now()); v.Backup.Auto {
		t.Fatal("没设 backupHour 时定时备份是关的")
	}
	setSetting(t, s, "backupHour", "4")
	if v := s.computeAttention(time.Now()); !v.Backup.Auto || v.Backup.LastErr != "" {
		t.Fatalf("backupHour=4 时应算开着,也没有失败: %+v", v.Backup)
	}
	setSetting(t, s, "backupHour", "-1")
	if v := s.computeAttention(time.Now()); v.Backup.Auto {
		t.Fatal("backupHour=-1 是关")
	}
}

// 60 秒缓存:期间库里变了也不重算;过了 60 秒重算
func TestAttentionCached(t *testing.T) {
	s, db := newAttentionServer(t)
	now := time.Now().Unix()
	db.Create(&model.User{Name: "x", Enabled: true, Expiry: now + 86400})
	if v := s.attention(); v.UsersExpiring != 1 {
		t.Fatalf("第一次应算出 1 个: %d", v.UsersExpiring)
	}
	db.Create(&model.User{Name: "y", Enabled: true, Expiry: now + 86400})
	if v := s.attention(); v.UsersExpiring != 1 {
		t.Fatalf("60 秒内应用缓存: %d", v.UsersExpiring)
	}
	s.attnMu.Lock()
	s.attnAt = time.Now().Add(-attentionTTL - time.Second)
	s.attnMu.Unlock()
	if v := s.attention(); v.UsersExpiring != 2 {
		t.Fatalf("缓存过期后应重算出 2 个: %d", v.UsersExpiring)
	}
}

func statusJSON(t *testing.T, s *Server, r *http.Request) map[string]json.RawMessage {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleStatus(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// 主机的 status 带 attention、不带副机的字段;副机的带 hubSeenAt / ledgerFails、不带 attention;
// 数据目录所在盘的总量与剩余两边都给(Windows 上不统计)
func TestStatusAttentionAndNodeFields(t *testing.T) {
	s, _ := newAttentionServer(t)
	m := statusJSON(t, s, httptest.NewRequest("GET", "http://x/app/api/status", nil))
	if _, ok := m["attention"]; !ok {
		t.Fatal("主机的 status 应带 attention")
	}
	if _, ok := m["hubSeenAt"]; ok {
		t.Fatal("主机不该有 hubSeenAt")
	}
	if _, ok := m["ledgerFails"]; ok {
		t.Fatal("主机不该有 ledgerFails")
	}
	if runtime.GOOS != "windows" {
		var d struct{ Total, Free int64 }
		if err := json.Unmarshal(m["disk"], &d); err != nil || d.Total <= 0 || d.Free < 0 || d.Free > d.Total {
			t.Fatalf("disk 应是 {total, free}: %s %v", m["disk"], err)
		}
	}

	setSetting(t, s, "nodeMode", "true")
	m = statusJSON(t, s, httptest.NewRequest("GET", "http://x/app/api/status", nil))
	if _, ok := m["attention"]; ok {
		t.Fatal("副机不该算 attention")
	}
	if _, ok := m["hubSeenAt"]; ok {
		t.Fatal("主机还没联系过本机时不给 hubSeenAt")
	}
	if string(m["ledgerFails"]) != "0" {
		t.Fatalf("副机应给 ledgerFails: %s", m["ledgerFails"])
	}
	at := time.Now().Unix() - 30
	s.hubSeenAt.Store(at)
	m = statusJSON(t, s, httptest.NewRequest("GET", "http://x/app/api/status", nil))
	var got int64
	if err := json.Unmarshal(m["hubSeenAt"], &got); err != nil || got != at {
		t.Fatalf("hubSeenAt 应是 %d: %s", at, m["hubSeenAt"])
	}
}

// 只有带对令牌的请求(主机)才刷新 hubSeenAt
func TestAgentAuthRecordsHubSeen(t *testing.T) {
	s, _ := newAttentionServer(t)
	setSetting(t, s, "nodeMode", "true")
	tok, err := s.nodeToken()
	if err != nil {
		t.Fatal(err)
	}
	h := s.agentAuth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r := httptest.NewRequest("POST", "http://x/app/api/agent/ping", nil)
	r.Header.Set("X-Agent-Token", "wrong")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusUnauthorized || s.hubSeenAt.Load() != 0 {
		t.Fatalf("令牌不对不该记: %d %d", w.Code, s.hubSeenAt.Load())
	}
	r = httptest.NewRequest("POST", "http://x/app/api/agent/ping", nil)
	r.Header.Set("X-Agent-Token", tok)
	w = httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusNoContent || time.Now().Unix()-s.hubSeenAt.Load() > 5 {
		t.Fatalf("主机的请求应记下时间: %d %d", w.Code, s.hubSeenAt.Load())
	}
}

// actor=human:不要定时任务批量写的记录(以 Job 结尾的执行者)
func TestAuditActorHuman(t *testing.T) {
	s, db := newAttentionServer(t)
	for i, a := range []string{"admin", "ResetJob", "telegram", "DepleteJob", "dl", "RuleJob", "api"} {
		db.Create(&model.Change{DateTime: int64(1000 + i), Actor: a, Key: "user", Action: "x", Obj: []byte(`"u"`)})
	}
	get := func(q string) []model.Change {
		w := httptest.NewRecorder()
		s.handleAudit(w, httptest.NewRequest("GET", "http://x/app/api/audit?"+q, nil))
		var rows []model.Change
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatal(err, w.Body.String())
		}
		return rows
	}
	if rows := get("limit=40"); len(rows) != 7 {
		t.Fatalf("不带 actor 应全给: %d", len(rows))
	}
	rows := get("limit=40&actor=human")
	if len(rows) != 4 {
		t.Fatalf("人工档应只剩 4 条: %+v", rows)
	}
	for _, r := range rows {
		if len(r.Actor) >= 3 && r.Actor[len(r.Actor)-3:] == "Job" {
			t.Fatalf("人工档里出现了定时任务: %s", r.Actor)
		}
	}
	if rows := get("limit=2&actor=human"); len(rows) != 2 || rows[0].Actor != "api" {
		t.Fatalf("limit 仍然生效且按新到旧: %+v", rows)
	}
}

// 代理作用域的 status:额度用尽、用户数上限、设备池最近被拒数
func TestResellerStatusAttentionFields(t *testing.T) {
	s, db := newAttentionServer(t)
	rs := model.Reseller{Name: "dl", Enabled: true, UserLimit: 50, Depleted: true}
	db.Create(&rs)
	r := httptest.NewRequest("GET", "http://x/dl/api/status", nil)
	r = r.WithContext(withScope(r, rs.Id))
	m := statusJSON(t, s, r)
	if string(m["depleted"]) != "true" || string(m["userLimit"]) != "50" || string(m["poolRejects"]) != "0" {
		t.Fatalf("代理 status 应带 depleted / userLimit / poolRejects: %s %s %s", m["depleted"], m["userLimit"], m["poolRejects"])
	}
	if _, ok := m["attention"]; ok {
		t.Fatal("代理作用域不该有主面板的 attention")
	}
}

// 在线设备数:IPv4 地址与 IPv6 /64 各自计数取较大的(和设备数限制同一口径,见 model.DeviceCount);没有 IP 的不列
func TestOnlineDevices(t *testing.T) {
	ips := map[string][]string{
		"a": {"203.0.113.5", "203.0.113.6", "203.0.113.7", "2001:db8:1:2::10", "2001:db8:1:2::20", "2001:db8:1:3::1"},
		"b": {},
		"c": {"198.51.100.1"},
	}
	got := onlineDevices([]string{"a", "b", "c", "d"}, ips)
	if got["a"] != 3 || got["c"] != 1 {
		t.Fatalf("a 是 3 个 v4、2 个 /64,应算 3 台;c 1 台: %v", got)
	}
	if _, ok := got["b"]; ok {
		t.Fatalf("没有 IP 的不该列: %v", got)
	}
	if _, ok := got["d"]; ok {
		t.Fatalf("不在名单里的不该列: %v", got)
	}
	// 代理作用域:handler 先把名单过滤成名下用户,再算设备数
	if got := onlineDevices([]string{"c"}, ips); len(got) != 1 || got["c"] != 1 {
		t.Fatalf("只算传进来的用户: %v", got)
	}
}

// 在线接口带 devices(没人在线时是空对象,不是 null)
func TestOnlinesIncludesDevices(t *testing.T) {
	s, _ := newAttentionServer(t)
	w := httptest.NewRecorder()
	s.handleOnlines(w, httptest.NewRequest("GET", "http://x/app/api/onlines", nil))
	var out struct {
		Devices map[string]int `json:"devices"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Devices == nil {
		t.Fatalf("onlines 应带 devices 对象: %d %s", w.Code, w.Body.String())
	}
}

// 副机:主机推来的配置被拒收(版本太低)或没应用成,记下原因;之后应用成功清掉。不回应的(主机已放弃)不算。
func TestApplyStateNotesFailures(t *testing.T) {
	var a applyState
	a.note(http.StatusConflict, []byte(`{"error":"副机版本 v0.7.1 低于主机要求的 v0.7.17,请先升级这台副机"}`))
	msg, at := a.get()
	if msg != "副机版本 v0.7.1 低于主机要求的 v0.7.17,请先升级这台副机" || time.Now().Unix()-at > 5 {
		t.Fatalf("应记下错误原文和时间: %q %d", msg, at)
	}
	a.note(0, nil)
	if msg, _ := a.get(); msg == "" {
		t.Fatal("没回应的请求不该清掉")
	}
	a.note(http.StatusServiceUnavailable, []byte("not json"))
	if msg, _ := a.get(); msg != http.StatusText(http.StatusServiceUnavailable) {
		t.Fatalf("正文不是 JSON 时用状态码说明: %q", msg)
	}
	a.note(http.StatusOK, []byte(`{"ok":"1"}`))
	if msg, at := a.get(); msg != "" || at != 0 {
		t.Fatalf("应用成功后应清空: %q %d", msg, at)
	}
	// 记录器:错误回应的正文能拿到,原样写给主机
	w := httptest.NewRecorder()
	rec := &applyRecorder{ResponseWriter: w}
	writeJSON(rec, http.StatusConflict, map[string]string{"error": "x"})
	if rec.code != http.StatusConflict || !strings.Contains(rec.body.String(), `"x"`) || !strings.Contains(w.Body.String(), `"x"`) {
		t.Fatalf("记录器应拿到状态码与正文: %d %q %q", rec.code, rec.body.String(), w.Body.String())
	}
}

// 副机 status:最近一次没应用成的原因;本机部署了几条线路(库里是主机下发的全部线路)。GET nodes 的修订号用最近应用的那份
func TestNodeStatusApplyAndLocalLines(t *testing.T) {
	s, db := newAttentionServer(t)
	setSetting(t, s, "nodeMode", "true")
	setSetting(t, s, "hubRevision", "abcdef0123456789")
	self := s.run.LocalNodeId()
	db.Create(&model.Line{Name: "all", Protocol: "socks", Port: 31001, Enabled: true})
	db.Create(&model.Line{Name: "mine", Protocol: "socks", Port: 31002, Enabled: true, NodeIds: []byte(fmt.Sprintf("[%d]", self))})
	db.Create(&model.Line{Name: "other", Protocol: "socks", Port: 31003, Enabled: true, NodeIds: []byte(fmt.Sprintf("[%d]", self+100))})
	off := model.Line{Name: "mine-off", Protocol: "socks", Port: 31004, Enabled: true, NodeIds: []byte(fmt.Sprintf("[%d]", self))}
	db.Create(&off)
	db.Model(&model.Line{}).Where("id = ?", off.Id).Update("enabled", false)
	s.applyRes.note(http.StatusConflict, []byte(`{"error":"too old"}`))
	m := statusJSON(t, s, httptest.NewRequest("GET", "http://x/app/api/status", nil))
	if string(m["applyErr"]) != `"too old"` || len(m["applyErrAt"]) == 0 {
		t.Fatalf("副机 status 应带 applyErr / applyErrAt: %s %s", m["applyErr"], m["applyErrAt"])
	}
	if string(m["linesLocal"]) != "3" || string(m["linesLocalEnabled"]) != "2" {
		t.Fatalf("本机部署的线路应是 3 条(启用 2 条): %s / %s", m["linesLocal"], m["linesLocalEnabled"])
	}
	w := httptest.NewRecorder()
	s.handleNodes(w, httptest.NewRequest("GET", "http://x/app/api/nodes", nil))
	var nodes struct{ Revision string }
	if err := json.Unmarshal(w.Body.Bytes(), &nodes); err != nil || nodes.Revision != "abcdef0123456789" {
		t.Fatalf("副机的 GET nodes 应给最近应用的修订号: %v %s", err, w.Body.String())
	}
}

// 主面板:在线名单里代理名下的人 → 代理名;流量排行标出代理名下的和已删除的;status 带大陆连通检测自己的状况
func TestOwnersTopAndReachState(t *testing.T) {
	s, db := newAttentionServer(t)
	rs := model.Reseller{Name: "华东店", Enabled: true}
	db.Create(&rs)
	db.Create(&model.User{Name: "direct", Enabled: true})
	db.Create(&model.User{Name: "sub", Enabled: true, ResellerId: rs.Id})
	if got := s.onlineOwners([]string{"direct", "sub", "nobody"}); len(got) != 1 || got["sub"] != "华东店" {
		t.Fatalf("只列代理名下的: %v", got)
	}
	now := time.Now().Unix()
	for i, n := range []string{"direct", "sub", "deleted"} {
		db.Create(&model.Stats{DateTime: now - 60, Resource: "user", Tag: n, Direction: true, Traffic: int64(1000 * (3 - i))})
	}
	w := httptest.NewRecorder()
	s.handleStatsTop(w, httptest.NewRequest("GET", "http://x/app/api/stats/top?hours=24&limit=10", nil))
	var rows []struct {
		Name  string
		Owner string
		Gone  bool
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 3 {
		t.Fatalf("排行: %v %s", err, w.Body.String())
	}
	if rows[0].Name != "direct" || rows[0].Owner != "" || rows[0].Gone || rows[1].Owner != "华东店" || !rows[2].Gone {
		t.Fatalf("代理名下的标所属代理、删掉的标 gone: %+v", rows)
	}
	m := statusJSON(t, s, httptest.NewRequest("GET", "http://x/app/api/status", nil))
	var rst struct {
		Auto    bool
		Minutes int
	}
	if err := json.Unmarshal(m["reachState"], &rst); err != nil || rst.Minutes <= 0 {
		t.Fatalf("主机 status 应带 reachState: %s %v", m["reachState"], err)
	}
}
