package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 「选中全部 N 个筛选结果」一次能送上万个 id:超过 SQLite 一条语句 32766 个变量时,以前 Find 报错、结果为空,
// 批量延期 / 删除 / 套用套餐"处理了 0 个"照样 200。现在按 500 一批取。
func TestUsersBatchManyIds(t *testing.T) {
	s, db := newAttentionServer(t)
	now := time.Now().Unix()
	createUsers(t, db, []model.User{
		{Name: "a", Enabled: true, Expiry: now + 86400, SubToken: "t1t1t1t1t1t1t1t1t1t1t1t1"},
		{Name: "b", Enabled: true, Expiry: now + 86400, SubToken: "t2t2t2t2t2t2t2t2t2t2t2t2"},
		{Name: "c", Enabled: true, SubToken: "t3t3t3t3t3t3t3t3t3t3t3t3"}, // 不限期:延期不动它
	})
	ids := make([]string, 0, 33000)
	for i := 1; i <= 33000; i++ {
		ids = append(ids, fmt.Sprint(i))
	}
	post := func(body string) map[string]int64 {
		w := httptest.NewRecorder()
		s.handleUsersBatch(w, httptest.NewRequest("POST", "http://x/app/api/users/batch", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var res map[string]int64
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		return res
	}
	if res := post(`{"ids":[` + strings.Join(ids, ",") + `],"action":"extend","days":30}`); res["affected"] != 2 {
		t.Fatalf("33000 个 id 里有 2 个限期的用户,应延期 2 个: %v", res)
	}
	if res := post(`{"ids":[` + strings.Join(ids, ",") + `],"action":"disable"}`); res["affected"] != 3 {
		t.Fatalf("应停用 3 个: %v", res)
	}
	if c := idChunks(make([]uint, 1001)); len(c) != 3 || len(c[0]) != 500 || len(c[2]) != 1 {
		t.Fatalf("分批: %d", len(c))
	}
	if idChunks(nil) != nil {
		t.Fatal("没有 id 时不该有批")
	}
}

// 「需要处理」标题的人数:同时快到期又流量将尽的人只算一次;同时额度用尽又快到期的代理只算一次。
func TestAttentionDedupCounts(t *testing.T) {
	s, db := newAttentionServer(t)
	now := time.Now().Unix()
	createUsers(t, db, []model.User{
		{Name: "both", Enabled: true, Expiry: now + 86400, Volume: 10 * gib, Up: 9 * gib},
		{Name: "exp", Enabled: true, Expiry: now + 86400},
		{Name: "quota", Enabled: true, Volume: 10 * gib, Up: 9 * gib},
		{Name: "fine", Enabled: true, Expiry: now + 30*86400},
	})
	for _, r := range []model.Reseller{
		{Name: "r-both", Enabled: true, Depleted: true, Expiry: now + 86400},
		{Name: "r-dep", Enabled: true, Depleted: true},
		{Name: "r-gone", Enabled: true, Expiry: now - 86400},
		{Name: "r-fine", Enabled: true, Expiry: now + 30*86400},
	} {
		db.Create(&r)
	}
	v := s.computeAttention(time.Now())
	if v.UsersExpiring != 2 || v.UsersQuota != 2 || v.UsersAttn != 3 {
		t.Fatalf("用户分项 2 / 2,去重后 3: %+v", v)
	}
	if v.ResellersDepleted != 2 || v.ResellersExpiring != 1 || v.ResellersExpired != 1 || v.ResellersAttn != 3 {
		t.Fatalf("代理分项 2 / 1 / 1,去重后 3: %+v", v)
	}
}

// 「人工」档按定时任务的名单排除:名字以 job 结尾的管理员 / 代理(大小写都有)不能被藏掉。
func TestAuditHumanKeepsJobSuffixedPeople(t *testing.T) {
	s, db := newAttentionServer(t)
	for i, a := range []string{"admin", model.ActorResetJob, "bigjob", model.ActorDepleteJob, "MyJob", model.ActorRuleJob, "api:dljob"} {
		db.Create(&model.Change{DateTime: int64(1000 + i), Actor: a, Key: "user", Action: "x", Obj: []byte(`"u"`)})
	}
	w := httptest.NewRecorder()
	s.handleAudit(w, httptest.NewRequest("GET", "http://x/app/api/audit?limit=40&actor=human", nil))
	var rows []model.Change
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Actor)
	}
	if strings.Join(got, ",") != "api:dljob,MyJob,bigjob,admin" {
		t.Fatalf("人工档: %v", got)
	}
}

// 副机面板的在线名单同样给 owners(代理名下用户 → 代理名):以前只在主面板给,副机上代理名下的在线用户链到空的用户页。
func TestOnlinesOwnersOnNode(t *testing.T) {
	s, _ := newAttentionServer(t)
	setSetting(t, s, "nodeMode", "true")
	w := httptest.NewRecorder()
	s.handleOnlines(w, httptest.NewRequest("GET", "http://x/app/api/onlines", nil))
	var out map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["owners"]; !ok {
		t.Fatalf("副机的 onlines 也应带 owners: %s", w.Body.String())
	}
}

// 副机重启后主机一直没来联系:hubSeenAt 只在内存里没有了,配过对的副机给本次启动时间 hubQuietSince,概况据此报「主机多久没联系」。
func TestStatusHubQuietSince(t *testing.T) {
	s, _ := newAttentionServer(t)
	setSetting(t, s, "nodeMode", "true")
	m := statusJSON(t, s, httptest.NewRequest("GET", "http://x/app/api/status", nil))
	if _, ok := m["hubQuietSince"]; ok {
		t.Fatal("没配过对(没有 hubRevision)不给")
	}
	setSetting(t, s, "hubRevision", "abc123")
	m = statusJSON(t, s, httptest.NewRequest("GET", "http://x/app/api/status", nil))
	var since int64
	if err := json.Unmarshal(m["hubQuietSince"], &since); err != nil || since != s.startedAt || since == 0 {
		t.Fatalf("hubQuietSince 应是启动时间 %d: %s", s.startedAt, m["hubQuietSince"])
	}
	s.hubSeenAt.Store(time.Now().Unix())
	m = statusJSON(t, s, httptest.NewRequest("GET", "http://x/app/api/status", nil))
	if _, ok := m["hubQuietSince"]; ok {
		t.Fatal("主机联系过之后用 hubSeenAt,不再给 hubQuietSince")
	}
}

// GET nodes 带服务器时间:前端按它判断离线防抖,浏览器时钟偏了也不误判。
func TestNodesIncludesServerNow(t *testing.T) {
	s, _ := newAttentionServer(t)
	w := httptest.NewRecorder()
	s.handleNodes(w, httptest.NewRequest("GET", "http://x/app/api/nodes", nil))
	var out struct {
		Now int64 `json:"now"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Now < time.Now().Unix()-5 {
		t.Fatalf("GET nodes 应带 now: %s", w.Body.String())
	}
}
