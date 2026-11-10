package ha

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"

	"gorm.io/gorm"
)

// 纪元规则的判定表(设计 4.2)。
func TestDecide(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	fresh, stale := now.Add(-30*time.Second), now.Add(-LeaseExpire-time.Second)
	cases := []struct {
		name     string
		v        NodeView
		e        uint64
		m        uint
		adopt    bool
		code     string
		wantAdpt bool
	}{
		{"都没开过容灾", NodeView{}, 0, 0, false, "", false},
		{"低纪元", NodeView{Epoch: 2, MasterId: 5, LastContact: fresh}, 1, 1, true, CodeStale, false},
		{"不带纪元的老主机", NodeView{Epoch: 1, MasterId: 1, LastContact: fresh}, 0, 0, true, CodeStale, false},
		{"同纪元不同主机", NodeView{Epoch: 2, MasterId: 5, LastContact: fresh}, 2, 6, true, CodeStale, false},
		{"同纪元同主机", NodeView{Epoch: 2, MasterId: 5, LastContact: fresh}, 2, 5, false, "", false},
		{"高纪元但动作不能升", NodeView{Epoch: 1, MasterId: 1, LastContact: stale}, 2, 5, false, CodeNotAdopted, false},
		{"高纪元但租约还在", NodeView{Epoch: 1, MasterId: 1, LastContact: fresh}, 2, 5, true, CodeFollowing, false},
		{"高纪元且租约过期", NodeView{Epoch: 1, MasterId: 1, LastContact: stale}, 2, 5, true, "", true},
		{"投过票", NodeView{Epoch: 1, MasterId: 1, LastContact: fresh, Vote: &Vote{Epoch: 2, Candidate: 5}}, 2, 5, true, "", true},
		{"票投给了别人", NodeView{Epoch: 1, MasterId: 1, LastContact: fresh, Vote: &Vote{Epoch: 2, Candidate: 6}}, 2, 5, true, CodeFollowing, false},
		{"有交接标记", NodeView{Epoch: 2, MasterId: 5, LastContact: fresh, Grant: &Grant{Epoch: 3, To: 1}}, 3, 1, true, "", true},
		{"交接标记是给别人的", NodeView{Epoch: 2, MasterId: 5, LastContact: fresh, Grant: &Grant{Epoch: 3, To: 7}}, 3, 1, true, CodeFollowing, false},
		{"回来后变成副机的原主机", NodeView{Epoch: 1, MasterId: 1, LastContact: fresh, Demoted: true}, 2, 5, true, "", true},
		{"本机还没进过容灾", NodeView{LastContact: fresh}, 1, 1, true, "", true},
		{"本机还没进过容灾但动作不能升", NodeView{LastContact: fresh}, 1, 1, false, CodeNotAdopted, false},
	}
	for _, c := range cases {
		got := Decide(c.v, c.e, c.m, c.adopt, now)
		if got.Code != c.code || got.Adopt != c.wantAdpt {
			t.Errorf("%s: 期望 code=%q adopt=%v,实际 %+v", c.name, c.code, c.wantAdpt, got)
		}
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := http.Header{}
	SetHeader(h, 0, 3)
	if len(h) != 0 {
		t.Fatal("纪元为 0 时不能加任何请求头:没开过容灾的集群请求要和以前一字不差")
	}
	SetHeader(h, 7, 3)
	if e, m := Header(h); e != 7 || m != 3 {
		t.Fatalf("读回 %d %d", e, m)
	}
	if r := ParseReject(409, []byte(`{"error":"x","code":"stale_epoch","epoch":4,"masterId":2,"sinceChange":99}`)); r == nil || r.Epoch != 4 || r.SinceChange != 99 {
		t.Fatalf("409 正文解析: %+v", r)
	}
	if r := ParseReject(409, []byte(`{"error":"副机版本太旧"}`)); r != nil {
		t.Fatal("没有纪元代码的 409(版本门槛)不是纪元判定")
	}
}

// ---- 用真实数据库的 Service ----

func testService(t *testing.T, node bool, kv map[string]string) *Service {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m-ui.db")
	db, err := database.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if node {
		kv["nodeMode"] = "true"
	}
	for k, v := range kv {
		db.Create(&model.Setting{Key: k, Value: v})
	}
	s := New(Deps{DB: db, DBPath: p, Version: "0.7.17"})
	return s
}

func hdr(e uint64, m uint) http.Header {
	h := http.Header{}
	SetHeader(h, e, m)
	return h
}

func setting(db *gorm.DB, k string) string {
	var v string
	db.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
	return v
}

// 副机按纪元规则接受 / 拒绝,切纪元时同一事务里清掉快照序号基线。
func TestCheckAgentNode(t *testing.T) {
	s := testService(t, true, map[string]string{"hubSnapshotSequence": "999999999999"})
	// 两边都没开过容灾:放行,什么都不写
	if rej := s.CheckAgent(http.Header{}, "report"); rej != nil {
		t.Fatalf("没开过容灾应放行: %+v", rej)
	}
	// 主机开了容灾(纪元 1):拉报告不能升纪元
	if rej := s.CheckAgent(hdr(1, 1), "report"); rej == nil || rej.Code != CodeNotAdopted {
		t.Fatalf("拉报告不能升纪元: %+v", rej)
	}
	// 推状态可以升(本机还没进过容灾)
	if rej := s.CheckAgent(hdr(1, 1), "ha/state"); rej != nil {
		t.Fatalf("推状态应升纪元: %+v", rej)
	}
	if s.Epoch() != 1 || setting(s.d.DB, KeyMasterId) != "1" || setting(s.d.DB, "hubSnapshotSequence") != "" {
		t.Fatalf("升纪元后应记下纪元、主机,序号基线清零: epoch=%d master=%s seq=%q", s.Epoch(), setting(s.d.DB, KeyMasterId), setting(s.d.DB, "hubSnapshotSequence"))
	}
	if rej := s.CheckAgent(hdr(1, 1), "report"); rej != nil {
		t.Fatalf("同纪元同主机应放行: %+v", rej)
	}
	// 备用 #5 以纪元 2 来推:本机刚联系过主机 1,不换主
	if rej := s.CheckAgent(hdr(2, 5), "apply"); rej == nil || rej.Code != CodeFollowing || rej.Epoch != 1 {
		t.Fatalf("租约还在时应回 still_following: %+v", rej)
	}
	// 租约过期后接受
	s.mu.Lock()
	s.lastContact = s.now().Add(-LeaseExpire - time.Second)
	s.mu.Unlock()
	if rej := s.CheckAgent(hdr(2, 5), "apply"); rej != nil {
		t.Fatalf("租约过期后应接受新主机: %+v", rej)
	}
	if s.Epoch() != 2 || setting(s.d.DB, KeyMasterId) != "5" {
		t.Fatal("应切到纪元 2、主机 5")
	}
	// 旧主机回来:409 stale_epoch,带上本机认的纪元与主机
	rej := s.CheckAgent(hdr(1, 1), "report")
	if rej == nil || rej.Code != CodeStale || rej.Epoch != 2 || rej.MasterId != 5 {
		t.Fatalf("旧主机应收到 stale_epoch: %+v", rej)
	}
}

// 接任后的主机(不是副机,但保留了副机令牌)对原主机回 stale,对更高纪元回 still_following(不会因推送让位)。
func TestCheckAgentMaster(t *testing.T) {
	s := testService(t, false, map[string]string{KeyEpoch: "2"})
	s.d.DB.Create(&model.Node{Name: "TW", IsLocal: true, Enabled: true})
	if rej := s.CheckAgent(hdr(1, 1), "report"); rej == nil || rej.Code != CodeStale || rej.Epoch != 2 {
		t.Fatalf("旧主机来请求应 409 stale: %+v", rej)
	}
	if rej := s.CheckAgent(hdr(3, 9), "apply"); rej == nil || rej.Code != CodeFollowing {
		t.Fatalf("更高纪元来推:主机不让位: %+v", rej)
	}
	if s.Epoch() != 2 {
		t.Fatal("主机的纪元不能被推送改掉")
	}
}

// 回来后变成副机的原主机:接受任何更高的纪元,切过去后角色清掉、变为副机的记录留着。
func TestCheckAgentDemoted(t *testing.T) {
	s := testService(t, true, map[string]string{KeyEpoch: "1", KeyMasterId: "1", KeyRole: RoleDemoted, KeyDemoted: `{"epoch":2}`})
	if rej := s.CheckAgent(hdr(3, 7), "ha/state"); rej != nil {
		t.Fatalf("回来后变成副机的原主机应接受更高纪元: %+v", rej)
	}
	if setting(s.d.DB, KeyRole) != "" || setting(s.d.DB, KeyDemoted) == "" {
		t.Fatal("切到新主机后角色清掉、变为副机的记录保留")
	}
}

// 变为副机的多数判定(设计 4.3):过半才变,只有一台副机时它说了算,老副机不计,没回齐不下结论。
func TestDemoteTally(t *testing.T) {
	cases := []struct {
		name string
		t    demoteTally
		want bool
	}{
		{"没人回 stale", demoteTally{total: 3, aware: 3}, false},
		{"三台里一台 stale", demoteTally{total: 3, aware: 3, stale: 1}, false},
		{"三台里两台 stale", demoteTally{total: 3, aware: 3, stale: 2}, true},
		{"只有一台副机", demoteTally{total: 1, aware: 1, stale: 1}, true},
		{"两台里一台 stale", demoteTally{total: 2, aware: 2, stale: 1}, false},
		{"三台里一台 stale、两台失联", demoteTally{total: 3, aware: 1, stale: 1}, true},
		{"还有没回话的,stale 不过全部的半数", demoteTally{total: 3, aware: 1, stale: 1, unknown: 2}, false},
		{"stale 已过全部的半数", demoteTally{total: 3, aware: 2, stale: 2, unknown: 1}, true},
	}
	for _, c := range cases {
		if got := c.t.shouldDemote(); got != c.want {
			t.Errorf("%s: 期望 %v,实际 %v", c.name, c.want, got)
		}
	}
}

// 主机侧:一台副机说 stale、另两台还认本机 → 不变,标"已脱离"并告警;两台说 stale → 变为副机(只做一次)、写好记录。
func TestDemoteByMajority(t *testing.T) {
	var notes []string
	s := testService(t, false, map[string]string{KeyEpoch: "1", KeyMode: ModeConfirm})
	s.d.Notify = func(_, text string) { notes = append(notes, text) }
	restarted := 0
	s.d.Restart = func(time.Duration) { restarted++ }
	s.d.Backup = func(prefix string) (string, error) { return prefix + "x.zip", nil }
	db := s.d.DB
	db.Create(&model.Node{Name: "HK", IsLocal: true, Enabled: true})
	ids := []uint{}
	for _, name := range []string{"A", "B", "C"} {
		n := model.Node{Name: name, ApiUrl: "https://" + name + ":2053/app/", Token: "t", Enabled: true}
		db.Create(&n)
		ids = append(ids, n.Id)
	}
	for i := 0; i < 5; i++ {
		db.Create(&model.Change{DateTime: time.Now().Unix(), Actor: "a", Key: "k", Action: "x"})
	}
	ok := hub.EpochReply{Aware: true, NodeEpoch: 1}
	stale := hub.EpochReply{Aware: true, Code: CodeStale, Epoch: 2, MasterId: ids[0], SinceChange: 3}
	s.NoteReply(model.Node{Id: ids[0]}, stale)
	s.NoteReply(model.Node{Id: ids[1]}, ok)
	s.NoteReply(model.Node{Id: ids[2]}, ok)
	if s.IsNode() || restarted != 0 {
		t.Fatal("少数说 stale 不能变为副机")
	}
	if len(notes) != 1 {
		t.Fatalf("应告警一次'已脱离',实际 %d 条", len(notes))
	}
	s.NoteReply(model.Node{Id: ids[0]}, stale) // 重复不再告警
	if len(notes) != 1 {
		t.Fatal("同一台已脱离只告警一次")
	}
	s.NoteReply(model.Node{Id: ids[1]}, stale)
	if !s.IsNode() || setting(db, KeyRole) != RoleDemoted || restarted != 1 {
		t.Fatalf("过半说 stale 应变为副机并重启: node=%v role=%q restarted=%d", s.IsNode(), setting(db, KeyRole), restarted)
	}
	var fb Demoted
	if !s.getJSON(KeyDemoted, &fb) || fb.Epoch != 2 || fb.OldEpoch != 1 || fb.Divergent != 2 || fb.Backup != "former-master-E1-x.zip" {
		t.Fatalf("变为副机的记录不对: %+v", fb)
	}
	if setting(db, hub.LedgerEpochKey) == "" {
		t.Fatal("变为副机时要换一本新的流量账本")
	}
	if n := s.Notices(); len(n) != 1 || n[0].Kind != "demoted" {
		t.Fatalf("面板顶部要留一条提示: %+v", n)
	}
}
