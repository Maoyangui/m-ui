package ha

import (
	"crypto/ed25519"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
)

// ---- 纪元的取值上限与来源可信度(设计 2.1) ----

// 纪元只经 NextEpoch 升:不采信超出窗口的声称,不回绕,到上限报错;源码里不许再有自己加 1 算纪元、绕过 setEpoch 写纪元的地方。
func TestNextEpochBounds(t *testing.T) {
	if n, err := NextEpoch(3); err != nil || n != 4 {
		t.Fatalf("NextEpoch(3) = %d %v", n, err)
	}
	if n, _ := NextEpoch(3, 7, 2); n != 8 {
		t.Fatalf("要高于采信的最高纪元: %d", n)
	}
	if n, _ := NextEpoch(3, math.MaxUint64, math.MaxUint64-1, MaxEpoch, 3+MaxEpochJump+1); n != 4 {
		t.Fatalf("超出窗口的声称不采信(不能回绕成 0、不能被推飞): %d", n)
	}
	if n, _ := NextEpoch(3, 3+MaxEpochJump); n != 3+MaxEpochJump+1 {
		t.Fatalf("窗口以内的照收: %d", n)
	}
	for _, local := range []uint64{MaxEpoch - 1, MaxEpoch, math.MaxUint64} {
		if _, err := NextEpoch(local); err == nil {
			t.Fatalf("纪元 %d 再升就到上限了:要报错", local)
		}
	}
	if !EpochClaimOK(0, MaxEpochJump) || EpochClaimOK(0, MaxEpochJump+1) || EpochClaimOK(MaxEpoch, MaxEpoch) {
		t.Fatal("EpochClaimOK 的窗口不对")
	}
	plusOne := regexp.MustCompile(`(?:^|[^!<>=])(?::=|=)\s*(?:max\(.*\)|L|E|s\.Epoch\(\))\s*\+\s*1\b|Epoch:\s*(?:L|E|s\.Epoch\(\))\s*\+\s*1\b`)
	write := regexp.MustCompile(`\bKeyEpoch\s*:|\[KeyEpoch\]\s*=`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if j := strings.Index(code, "//"); j >= 0 {
				code = code[:j]
			}
			if plusOne.MatchString(code) {
				t.Errorf("%s:%d 自己加 1 算纪元(要经 NextEpoch): %s", f, i+1, strings.TrimSpace(line))
			}
			// 写纪元只经 setEpoch(boot.go);promote.go 的 applyLocal 写的是要换上的新库;ResetLocal 清零
			if write.MatchString(code) && f != "boot.go" && f != "promote.go" {
				t.Errorf("%s:%d 绕过 setEpoch 写纪元: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// masterWith 一台纪元 L 的主机(纪元 0 = 没开过容灾)+ 若干远端副机行,带可拨的钟、记下通知与重启。
func masterWith(t *testing.T, L uint64, names ...string) (*Service, []uint, *[]string, *testClock, *int) {
	t.Helper()
	kv := map[string]string{KeyMode: ModeConfirm}
	if L > 0 {
		kv[KeyEpoch] = strconv.FormatUint(L, 10)
	}
	s := testService(t, false, kv)
	clk := &testClock{t: time.Now()}
	s.SetNow(clk.now)
	var mu sync.Mutex
	notes := &[]string{}
	s.d.Notify = func(_, text string) {
		mu.Lock()
		*notes = append(*notes, text)
		mu.Unlock()
	}
	restarted := new(int)
	s.d.Restart = func(time.Duration) { *restarted++ }
	s.d.Backup = func(prefix string) (string, error) { return prefix + "x.zip", nil }
	s.d.DB.Create(&model.Node{Name: "HK", IsLocal: true, Enabled: true})
	if L > 0 {
		_ = s.set(map[string]string{KeyMasterId: strconv.FormatUint(uint64(s.SelfId()), 10)})
	}
	var ids []uint
	for _, n := range names {
		row := model.Node{Name: n, ApiUrl: "https://" + n + ":2053/app/", Token: "t", Enabled: true}
		s.d.DB.Create(&row)
		ids = append(ids, row.Id)
	}
	return s, ids, notes, clk, restarted
}

func countNotes(notes *[]string, sub string) int {
	n := 0
	for _, x := range *notes {
		if strings.Contains(x, sub) {
			n++
		}
	}
	return n
}

func staleReply(epoch uint64, master uint, leaseAgo int64) hub.EpochReply {
	return hub.EpochReply{Aware: true, Code: CodeStale, Epoch: epoch, MasterId: master, LeaseAgo: leaseAgo, Role: "node"}
}

func rivalReply(epoch uint64, self uint, becameAt int64) hub.EpochReply {
	return hub.EpochReply{Aware: true, Code: CodeStale, Epoch: epoch, MasterId: self, Role: "master", BecameAt: becameAt}
}

var okReply = hub.EpochReply{Aware: true}

// 副机 409 里报了超出窗口的纪元(回绕攻击):不收回、不降级、纪元不变,告警一次。
func TestTallyBogusEpochIgnored(t *testing.T) {
	s, ids, notes, _, restarted := masterWith(t, 2, "A", "B")
	bogus := hub.EpochReply{Aware: true, Code: CodeStale, Epoch: math.MaxUint64, MasterId: 9, LeaseAgo: 999}
	for i := 0; i < 3; i++ {
		s.NoteReply(model.Node{Id: ids[0]}, bogus)
		s.NoteReply(model.Node{Id: ids[1]}, okReply)
	}
	if s.Epoch() != 2 || s.IsNode() || *restarted != 0 {
		t.Fatalf("异常纪元不能让本机收回或降级: epoch=%d node=%v", s.Epoch(), s.IsNode())
	}
	if countNotes(notes, "异常的纪元") != 1 {
		t.Fatalf("要告警一次: %q", *notes)
	}
	// 只有一台副机时也一样(它一台说了本来就算数)
	s2, ids2, _, _, _ := masterWith(t, 2, "A")
	s2.NoteReply(model.Node{Id: ids2[0]}, hub.EpochReply{Aware: true, Code: CodeStale, Epoch: 2 + MaxEpochJump + 1, MasterId: ids2[0]})
	if s2.Epoch() != 2 || s2.IsNode() {
		t.Fatal("跳得太高的纪元不采信")
	}
	if v := s2.replyViews()[strconv.FormatUint(uint64(ids2[0]), 10)]; v.Kind != "bogus" {
		t.Fatalf("回复要记成 bogus: %+v", v)
	}
}

// 降级要有证据说明新主机活着:普通副机谎称不够(新主机自己回话说它不是主机;或那台连不上、副机租约也不新鲜);
// 不认识的主机不降级;一主一副时那台自己回话说是主机就降级。
func TestDemoteNeedsLiveNewMaster(t *testing.T) {
	// 三台里两台说"C 已经在纪元 3 当主机",C 自己回话认本机:是谎话
	s, ids, _, _, restarted := masterWith(t, 2, "A", "B", "C")
	s.NoteReply(model.Node{Id: ids[2]}, okReply)
	s.NoteReply(model.Node{Id: ids[0]}, staleReply(3, ids[2], 0))
	s.NoteReply(model.Node{Id: ids[1]}, staleReply(3, ids[2], 0))
	if s.IsNode() || *restarted != 0 {
		t.Fatal("声称的新主机自己回话认本机:不能降级")
	}
	// 声称的新主机连不上、副机的租约也不新鲜(没人在推它们):不降级,等它们租约过期再收回
	s2, ids2, _, _, _ := masterWith(t, 2, "A", "B", "C")
	s2.NoteReply(model.Node{Id: ids2[2]}, hub.EpochReply{Down: true})
	s2.NoteReply(model.Node{Id: ids2[0]}, staleReply(3, ids2[2], 60))
	s2.NoteReply(model.Node{Id: ids2[1]}, staleReply(3, ids2[2], 60))
	if s2.IsNode() || s2.Epoch() != 2 {
		t.Fatal("新主机活着的证据不够:不降级,也不收回(还没成孤儿)")
	}
	// 本机不认识的主机:不降级不收回,告警
	s3, ids3, notes3, _, _ := masterWith(t, 2, "A")
	s3.NoteReply(model.Node{Id: ids3[0]}, staleReply(3, 99, 0))
	if s3.IsNode() || s3.Epoch() != 2 || countNotes(notes3, "不认识的主机") != 1 {
		t.Fatalf("不认识的主机:不降级、告警一次: node=%v %q", s3.IsNode(), *notes3)
	}
	// 一主一副:那台自己回话说它以纪元 3 当主机 → 降级
	s4, ids4, _, _, restarted4 := masterWith(t, 2, "S")
	s4.NoteReply(model.Node{Id: ids4[0]}, rivalReply(3, ids4[0], 100))
	if !s4.IsNode() || *restarted4 != 1 {
		t.Fatal("一主一副,那台已经以更高纪元当主机:本机变为副机")
	}
	var d Demoted
	if !s4.getJSON(KeyDemoted, &d) || d.Reason != "takenOver" || d.MasterId != ids4[0] || d.Divergent != -1 {
		t.Fatalf("变为副机的记录(不知道失联期间的改动记 -1): %+v", d)
	}
	// 三台:两台被新主机推着(租约新鲜)、新主机自己连不上本机 → 也降级
	s5, ids5, _, _, _ := masterWith(t, 2, "A", "B", "S")
	s5.NoteReply(model.Node{Id: ids5[2]}, hub.EpochReply{Down: true})
	s5.NoteReply(model.Node{Id: ids5[0]}, staleReply(3, ids5[2], 3))
	s5.NoteReply(model.Node{Id: ids5[1]}, staleReply(3, ids5[2], 3))
	if !s5.IsNode() {
		t.Fatal("过半副机正被新主机推着:本机变为副机")
	}
}

// 收回:回 stale 的全是孤儿、它们的主机连不上已经两分钟以上,才以更高纪元收回;同一 (主机, 纪元) 只收回一次。
func TestReclaimOnlyWhenNewMasterGone(t *testing.T) {
	s, ids, notes, clk, _ := masterWith(t, 2, "A", "B", "S")
	round := func() {
		s.NoteReply(model.Node{Id: ids[2]}, hub.EpochReply{Down: true})
		s.NoteReply(model.Node{Id: ids[0]}, staleReply(3, ids[2], 500))
		s.NoteReply(model.Node{Id: ids[1]}, okReply)
	}
	round()
	if s.Epoch() != 2 {
		t.Fatal("新主机刚连不上:还不能收回")
	}
	clk.add(90 * time.Second)
	round()
	if s.Epoch() != 2 {
		t.Fatal("连不上还不到两分钟:不收回")
	}
	clk.add(40 * time.Second)
	round()
	if s.Epoch() != 4 {
		t.Fatalf("新主机两分钟以上连不上、副机都是孤儿:以高于 3 的纪元收回: %d", s.Epoch())
	}
	clk.add(time.Minute)
	round()
	if s.Epoch() != 4 || countNotes(notes, "已收回") != 1 {
		t.Fatalf("同一 (主机, 纪元) 只收回一次: epoch=%d %q", s.Epoch(), *notes)
	}
	// 跟着别的主机的副机租约是新鲜的(那台主机活着):不收回
	s2, ids2, _, clk2, _ := masterWith(t, 2, "A", "B", "S")
	for i := 0; i < 3; i++ {
		s2.NoteReply(model.Node{Id: ids2[2]}, hub.EpochReply{Down: true})
		s2.NoteReply(model.Node{Id: ids2[0]}, staleReply(3, ids2[2], 5))
		s2.NoteReply(model.Node{Id: ids2[1]}, okReply)
		clk2.add(2 * time.Minute)
	}
	if s2.Epoch() != 2 || s2.IsNode() {
		t.Fatal("副机还被别的主机推着:不收回,也不够降级")
	}
}

// 同纪元两台主机(两台备用同时接任):当上主机早的(再比 id 小的)留下,另一台变为副机;
// 本机不是在这个纪元接任上来的,别人说它也在这个纪元当主机就是假话,不理。
func TestSameEpochRivalTiebreak(t *testing.T) {
	setup := func(how string, at int64) (*Service, []uint, *int) {
		s, ids, _, _, restarted := masterWith(t, 3, "R", "A")
		_ = s.set(map[string]string{KeyBecame: mustJSON(Became{How: how, Epoch: 3, At: at})})
		return s, ids, restarted
	}
	s, ids, restarted := setup(HowTakeover, 1000)
	s.NoteReply(model.Node{Id: ids[0]}, rivalReply(3, ids[0], 900))
	if !s.IsNode() || *restarted != 1 {
		t.Fatal("对方先当上:本机让给它")
	}
	var d Demoted
	if !s.getJSON(KeyDemoted, &d) || d.Reason != "sameEpoch" || d.MasterId != ids[0] || d.Epoch != 3 {
		t.Fatalf("记下是同纪元让出: %+v", d)
	}
	s2, ids2, _ := setup(HowTakeover, 1000)
	s2.NoteReply(model.Node{Id: ids2[0]}, rivalReply(3, ids2[0], 1100))
	s2.NoteReply(model.Node{Id: ids2[1]}, staleReply(3, ids2[0], 0)) // 跟着输的那台的副机不算
	if s2.IsNode() {
		t.Fatal("本机先当上:不让")
	}
	s3, ids3, _ := setup(HowSetMaster, 1000)
	s3.NoteReply(model.Node{Id: ids3[0]}, rivalReply(3, ids3[0], 1))
	if s3.IsNode() {
		t.Fatal("本机是设为主机得到的纪元:别人不可能也在这个纪元当主机")
	}
}

// 主机回 409 时不填租约(会被当成跟着一台不在的主机),报自己的角色、实例号、当上主机的时间;副机报它认的主机的实例号。
func TestMasterRejectNoLeaseAgo(t *testing.T) {
	m := testService(t, false, map[string]string{KeyEpoch: "2"})
	m.d.DB.Create(&model.Node{Name: "TW", IsLocal: true, Enabled: true})
	m.SetNow(func() time.Time { return time.Now().Add(time.Hour) }) // 进程跑了很久
	rej := m.CheckAgent(hdr(1, 1), "report")
	if rej == nil || rej.LeaseAgo != 0 || rej.Role != "master" || rej.MasterInst == "" || rej.MasterInst != m.Instance() {
		t.Fatalf("主机回 409:不填租约,报角色与实例号: %+v", rej)
	}
	n := testService(t, true, map[string]string{KeyEpoch: "2", KeyMasterId: "5", KeyState: mustJSON(State{Epoch: 2, MasterId: 5, Instance: "abc"})})
	rej = n.CheckAgent(hdr(1, 1), "report")
	if rej == nil || rej.Role != "node" || rej.MasterInst != "abc" || rej.MasterId != 5 {
		t.Fatalf("副机回 409:报它认的主机与实例号: %+v", rej)
	}
}

// 启动自检问完一圈才判:不会先发"已脱离"又马上变为副机。
func TestStartupCheckNoContradictoryAlerts(t *testing.T) {
	cl := newCluster(t, 2)
	m := cl.m
	cl.h.RunOnce()
	if m.Epoch() != 1 {
		t.Fatal("前提")
	}
	restarted := 0
	m.d.Restart = func(time.Duration) { restarted++ }
	m.d.Backup = func(prefix string) (string, error) { return prefix + "x.zip", nil }
	// 本机失联期间第一台接任了(纪元 2),第二台跟着它、正被它推着
	newMaster := strconv.FormatUint(uint64(cl.ids[0]), 10)
	for _, c := range cl.cands {
		c.set(map[string]string{KeyEpoch: "2", KeyMasterId: newMaster, KeyState: "", KeyMasterPub: ""})
	}
	*cl.notes = nil
	if !m.StartupCheck() {
		t.Fatal("过半副机跟着新主机、新主机活着:启动自检就变为副机")
	}
	if countNotes(cl.notes, "已脱离") != 0 || countNotes(cl.notes, "已变为副机") != 1 || restarted != 1 {
		t.Fatalf("只发一条变为副机的通知,不发已脱离: %q", *cl.notes)
	}
}

// 命令行收回(主机上 takeover --force):新纪元高于见过的全部(采信的)纪元。
func TestCLIReclaimAboveSeen(t *testing.T) {
	s, ids, _, _, _ := masterWith(t, 2, "A", "B", "C")
	s.NoteReply(model.Node{Id: ids[0]}, staleReply(5, 77, 9))
	s.NoteReply(model.Node{Id: ids[1]}, hub.EpochReply{Aware: true, Code: CodeFollowing, Epoch: 1, MasterId: 3})
	s.NoteReply(model.Node{Id: ids[2]}, hub.EpochReply{Aware: true, Code: CodeStale, Epoch: math.MaxUint64 - 1, MasterId: 9})
	e, err := s.ReclaimNow()
	if err != nil || e != 6 || s.Epoch() != 6 || s.IsNode() {
		t.Fatalf("收回到纪元 6(高于见过的 5,不理异常纪元): %d %v", e, err)
	}
	if setting(s.d.DB, KeyMasterId) != strconv.FormatUint(uint64(s.SelfId()), 10) {
		t.Fatal("还是本机当主机")
	}
}

// ---- 签名状态、钉住、交接标记、投票(设计 2.2) ----

func newSignKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	k, err := SignKey(NewSignKey())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func signed(k ed25519.PrivateKey, st State) []byte {
	b, _ := json.Marshal(st)
	return signState(k, b)
}

// nodeAt 一台纪元 L、主机为 m 的副机(钟可拨)。
func nodeAt(t *testing.T, L uint64, m uint) (*Service, *testClock) {
	t.Helper()
	s := testService(t, true, map[string]string{KeyEpoch: strconv.FormatUint(L, 10), KeyMasterId: strconv.FormatUint(uint64(m), 10)})
	s.d.DB.Create(&model.Node{Id: 20, Name: "TW", IsLocal: true, Enabled: true})
	clk := &testClock{t: time.Now()}
	s.SetNow(clk.now)
	s.touch()
	return s, clk
}

// 状态要签名:没签的、签名不对的、同一纪元换了公钥的都拒;没钉过的验过签名后钉住。
func TestStateSignedAndPinned(t *testing.T) {
	n, _ := nodeAt(t, 1, 1)
	k1, k2 := newSignKey(t), newSignKey(t)
	st := State{Epoch: 1, MasterId: 1, MasterPub: PubOf(k1), Self: 20, Mode: ModeConfirm, Standbys: []uint{}}
	plain, _ := json.Marshal(st)
	if err := n.ReceiveState(hdr(1, 1), plain); err == nil {
		t.Fatal("没签名的状态要拒")
	}
	old, _ := json.Marshal(SignedState{V: 1, Body: plain, Sig: "AAAA"})
	if err := n.ReceiveState(hdr(1, 1), old); err == nil {
		t.Fatal("版本不对的要拒")
	}
	good := signed(k1, st)
	var ss SignedState
	_ = json.Unmarshal(good, &ss)
	ss.Body = []byte(strings.Replace(string(ss.Body), `"mode":"confirm"`, `"mode":"auto"`, 1))
	tampered, _ := json.Marshal(ss)
	if err := n.ReceiveState(hdr(1, 1), tampered); err == nil {
		t.Fatal("正文被改过的要拒")
	}
	if err := n.ReceiveState(hdr(1, 1), good); err != nil {
		t.Fatalf("第一份签好名的状态:验过后钉住: %v", err)
	}
	if setting(n.d.DB, KeyMasterPub) != PubOf(k1) {
		t.Fatal("要钉住主机的签名公钥")
	}
	st2 := st
	st2.MasterPub = PubOf(k2)
	if err := n.ReceiveState(hdr(1, 1), signed(k2, st2)); err == nil {
		t.Fatal("同一纪元换了签名公钥:拒")
	}
	if err := n.ReceiveState(hdr(1, 1), good); err != nil {
		t.Fatalf("钉住的那把签的照收: %v", err)
	}
}

// 只拿到副机令牌(备用的副本里有全部令牌)伪造交接标记:签不出当前主机的签名,副机不收,也不因此换主。
func TestHandoverNeedsMasterSignature(t *testing.T) {
	n, _ := nodeAt(t, 1, 1)
	k1, evil := newSignKey(t), newSignKey(t)
	base := State{Epoch: 1, MasterId: 1, MasterPub: PubOf(k1), Self: 20, Mode: ModeConfirm, Standbys: []uint{}}
	if err := n.ReceiveState(hdr(1, 1), signed(k1, base)); err != nil {
		t.Fatal(err)
	}
	forged := base
	forged.MasterPub = PubOf(evil)
	forged.Handover = &Grant{Epoch: 2, To: 5, From: 1, FromEpoch: 1, Expires: time.Now().Add(time.Hour).Unix()}
	if err := n.ReceiveState(hdr(1, 1), signed(evil, forged)); err == nil {
		t.Fatal("不是钉住的主机签的交接标记:拒")
	}
	if setting(n.d.DB, KeyGrant) != "" {
		t.Fatal("伪造的交接标记不能留下")
	}
	if rej := n.CheckAgent(hdr(2, 5), "ha/state"); rej == nil || rej.Code != CodeFollowing {
		t.Fatalf("还联系得上主机:不换主: %+v", rej)
	}
}

// 交接标记过期作废;主机撤销后清掉;不是当前主机在当前纪元签发的不收。
func TestGrantExpiryAndRevoke(t *testing.T) {
	n, clk := nodeAt(t, 1, 1)
	k1, k5 := newSignKey(t), newSignKey(t)
	st := State{Epoch: 1, MasterId: 1, MasterPub: PubOf(k1), Self: 20, Mode: ModeConfirm, Standbys: []uint{}}
	st.Handover = &Grant{Epoch: 2, To: 5, ToPub: PubOf(k5), From: 1, FromEpoch: 1, Expires: clk.now().Add(time.Minute).Unix()}
	if err := n.ReceiveState(hdr(1, 1), signed(k1, st)); err != nil {
		t.Fatal(err)
	}
	if setting(n.d.DB, KeyGrant) == "" {
		t.Fatal("当前主机签发的交接标记要收下")
	}
	clk.add(2 * time.Minute)
	n.touch()
	if rej := n.CheckAgent(hdr(2, 5), "ha/state"); rej == nil || rej.Code != CodeFollowing {
		t.Fatalf("过期的交接标记作废: %+v", rej)
	}
	st.Handover.Expires = clk.now().Add(time.Minute).Unix()
	_ = n.ReceiveState(hdr(1, 1), signed(k1, st))
	st.Handover, st.Revoke = nil, 2
	if err := n.ReceiveState(hdr(1, 1), signed(k1, st)); err != nil {
		t.Fatal(err)
	}
	if setting(n.d.DB, KeyGrant) != "" {
		t.Fatal("撤销后清掉交接标记")
	}
	// 签发纪元不对 / 签发方不是当前主机的不收
	st.Revoke = 0
	st.Handover = &Grant{Epoch: 3, To: 5, From: 1, FromEpoch: 0, Expires: clk.now().Add(time.Minute).Unix()}
	_ = n.ReceiveState(hdr(1, 1), signed(k1, st))
	st.Handover = &Grant{Epoch: 3, To: 5, From: 9, FromEpoch: 1, Expires: clk.now().Add(time.Minute).Unix()}
	_ = n.ReceiveState(hdr(1, 1), signed(k1, st))
	if setting(n.d.DB, KeyGrant) != "" {
		t.Fatal("不是当前主机在当前纪元签发的交接标记不收")
	}
}

// 冒充交接标记里的新主机(有令牌、没有它的签名私钥):不能把副机带到新纪元;真的新主机拿出签名才换,并钉住它的公钥。
func TestGrantBindsToPub(t *testing.T) {
	n, clk := nodeAt(t, 1, 1)
	k1, k5, evil := newSignKey(t), newSignKey(t), newSignKey(t)
	st := State{Epoch: 1, MasterId: 1, MasterPub: PubOf(k1), Self: 20, Mode: ModeConfirm, Standbys: []uint{}}
	st.Handover = &Grant{Epoch: 2, To: 5, ToPub: PubOf(k5), From: 1, FromEpoch: 1, Expires: clk.now().Add(time.Hour).Unix()}
	if err := n.ReceiveState(hdr(1, 1), signed(k1, st)); err != nil {
		t.Fatal(err)
	}
	if rej := n.CheckAgent(hdr(2, 5), "apply"); rej == nil || rej.Code != CodeNotAdopted || n.Epoch() != 1 {
		t.Fatalf("凭交接标记换主要先推签好名的状态,别的动作不能升纪元: %+v", rej)
	}
	if rej := n.CheckAgent(hdr(2, 5), "ha/state"); rej != nil || n.Epoch() != 1 {
		t.Fatalf("ha/state 放行,但验过签名才换: %+v epoch=%d", rej, n.Epoch())
	}
	fake := State{Epoch: 2, MasterId: 5, MasterPub: PubOf(evil), Self: 20, Mode: ModeConfirm, Standbys: []uint{}}
	if err := n.ReceiveState(hdr(2, 5), signed(evil, fake)); err == nil || n.Epoch() != 1 {
		t.Fatalf("冒充的不能升纪元: %v epoch=%d", err, n.Epoch())
	}
	real := State{Epoch: 2, MasterId: 5, MasterPub: PubOf(k5), Self: 20, Mode: ModeConfirm, Standbys: []uint{}}
	if err := n.ReceiveState(hdr(2, 5), signed(k5, real)); err != nil {
		t.Fatal(err)
	}
	if n.Epoch() != 2 || setting(n.d.DB, KeyMasterId) != "5" || setting(n.d.DB, KeyMasterPub) != PubOf(k5) || setting(n.d.DB, KeyGrant) != "" {
		t.Fatalf("真的新主机:换到纪元 2、钉住它的公钥、交接标记用掉: epoch=%d", n.Epoch())
	}
}

// 票 30 分钟有效;重新联系上当前主机就作废。
func TestVoteExpiryAndClearedOnContact(t *testing.T) {
	n, clk := nodeAt(t, 1, 1)
	_ = n.set(map[string]string{KeyVote: mustJSON(Vote{Epoch: 2, Candidate: 5, At: clk.now().Unix()})})
	if rej := n.CheckAgent(hdr(1, 1), "report"); rej != nil {
		t.Fatal(rej)
	}
	if setting(n.d.DB, KeyVote) != "" {
		t.Fatal("当前主机来请求了:之前投的票作废")
	}
	_ = n.set(map[string]string{KeyVote: mustJSON(Vote{Epoch: 2, Candidate: 5, At: clk.now().Add(-voteTTL - time.Second).Unix()})})
	if rej := n.CheckAgent(hdr(2, 5), "ha/state"); rej == nil || rej.Code != CodeFollowing {
		t.Fatalf("过期的票不能拿来换主: %+v", rej)
	}
	_ = n.set(map[string]string{KeyVote: mustJSON(Vote{Epoch: 2, Candidate: 5, At: clk.now().Unix()})})
	if rej := n.CheckAgent(hdr(2, 5), "ha/state"); rej != nil || n.Epoch() != 2 {
		t.Fatalf("有效的票照常换主: %+v", rej)
	}
}

// ---- 接任的原子性(设计 2.3) ----

// blockPromoteTmp 让组新库失败:<库>.promote.tmp 是个非空目录。
func blockPromoteTmp(t *testing.T, s *Service) func() {
	t.Helper()
	p := s.d.DBPath + ".promote.tmp"
	if err := os.MkdirAll(filepath.Join(p, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.RemoveAll(p) }
}

// 组新库失败:纪元、角色都不变,记下接任失败;去掉故障后照样能接任;别的备用不会因此看到"更高纪元"。
func TestTakeoverFailureLeavesEpoch(t *testing.T) {
	p := newP2(t, 2, ModeConfirm)
	c := p.cands[0]
	unblock := blockPromoteTmp(t, c)
	if err := c.Takeover(TakeoverOpts{Force: true, By: "cli"}); err == nil {
		t.Fatal("组新库失败要报错")
	}
	if c.Epoch() != 1 || c.setting(KeyRole) != "" || c.lastTakeoverErr() == 0 {
		t.Fatalf("失败后纪元、角色不变,记下失败: epoch=%d role=%q", c.Epoch(), c.setting(KeyRole))
	}
	if _, err := os.Stat(promoteFile(c.d.DBPath)); err == nil {
		t.Fatal("失败不能留下待换的新库")
	}
	for _, clk := range p.clocks {
		clk.add(8 * time.Minute)
	}
	second := p.cands[1]
	second.eng.tick()
	if ev := second.eng.evidenceView(); ev != nil && ev.Reason == "higherEpoch" {
		t.Fatal("接任失败的备用纪元没变:别的备用不会被它挡住")
	}
	unblock()
	if err := c.Takeover(TakeoverOpts{Force: true, By: "cli"}); err != nil {
		t.Fatalf("去掉故障后照样能接任: %v", err)
	}
	if m, ok := readPromoteMeta(c.d.DBPath); !ok || m.Epoch != 2 || m.Expires <= time.Now().Unix() || c.setting(KeyRole) != RolePromoting || c.Epoch() != 1 {
		t.Fatalf("新库落盘(带时效)后才记接任中;纪元等换上新库才变: %+v role=%q epoch=%d", m, c.setting(KeyRole), c.Epoch())
	}
}

// 剩余空间不够:接任前就拒,什么都不改。
func TestTakeoverDiskPrecheck(t *testing.T) {
	p := newP2(t, 1, ModeConfirm)
	c := p.cands[0]
	old := takeoverDiskFree
	takeoverDiskFree = func(*Service) int64 { return 1 << 20 }
	defer func() { takeoverDiskFree = old }()
	err := c.Takeover(TakeoverOpts{Force: true, By: "cli"})
	if err == nil || !strings.Contains(err.Error(), "磁盘") {
		t.Fatalf("磁盘不够要提前拒: %v", err)
	}
	if c.Epoch() != 1 || c.setting(KeyRole) != "" {
		t.Fatal("什么都不改")
	}
	if _, err := os.Stat(promoteFile(c.d.DBPath)); err == nil {
		t.Fatal("不能留下新库")
	}
}

// 启动时 promoting 却没有待换的新库(组库途中崩溃):清掉;有新库的保留。
func TestPromotingMarkerClearedOnBoot(t *testing.T) {
	s := testService(t, true, map[string]string{KeyEpoch: "1", KeyRole: RolePromoting})
	s.Boot(false)
	if s.setting(KeyRole) != "" {
		t.Fatal("没有新库的 promoting 是崩溃残留:清掉")
	}
	s2 := testService(t, true, map[string]string{KeyEpoch: "1", KeyRole: RolePromoting})
	if err := os.WriteFile(promoteFile(s2.d.DBPath), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2.Boot(false)
	if s2.setting(KeyRole) != RolePromoting {
		t.Fatal("有待换的新库:保留")
	}
}

// 换库:过期的新库不换(改名留档、下次启动提示);旧库的 -wal 先挪开,挪不开就不换。
func TestApplyPendingExpiredAndWal(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m-ui.db")
	write := func(p, v string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	now := time.Unix(1_800_000_000, 0)
	write(db, "old")
	write(db+"-wal", "old-wal")
	write(promoteFile(db), "new")
	if err := writePromoteMeta(db, promoteMeta{Epoch: 3, How: HowTakeover, Expires: now.Add(-time.Second).Unix()}); err != nil {
		t.Fatal(err)
	}
	if ok, err := ApplyPendingAt(db, now); ok || err != nil {
		t.Fatalf("过期的新库不换: %v %v", ok, err)
	}
	if read(db) != "old" || read(db+"-wal") != "old-wal" {
		t.Fatal("当前库不动")
	}
	if m, _ := filepath.Glob(promoteFile(db) + ".expired-*"); len(m) != 1 {
		t.Fatal("过期的新库改名留档")
	}
	if _, err := os.Stat(filepath.Join(HADir(dir), fileExpired)); err != nil {
		t.Fatal("留一条下次启动的提示")
	}
	// 新库没过期,但旧库的 -wal 挪不开:不换
	write(promoteFile(db), "new")
	_ = writePromoteMeta(db, promoteMeta{Epoch: 3, How: HowTakeover, Expires: now.Add(time.Minute).Unix()})
	blocker := db + ".pre-promote-" + now.Format("20060102-150405") + "-wal"
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if ok, err := ApplyPendingAt(db, now); ok || err == nil {
		t.Fatal("旧库的 -wal 挪不开:不换并报错")
	}
	if read(db) != "old" || read(db+"-wal") != "old-wal" {
		t.Fatal("挪不开时旧库原样放回")
	}
	_ = os.RemoveAll(blocker)
	if ok, err := ApplyPendingAt(db, now); !ok || err != nil {
		t.Fatalf("应换上新库: %v %v", ok, err)
	}
	if read(db) != "new" {
		t.Fatal("换上新库")
	}
	if _, err := os.Stat(db + "-wal"); err == nil {
		t.Fatal("旧库的 -wal 不能留在新库旁边")
	}
	if _, err := os.Stat(promoteMetaFile(db)); err == nil {
		t.Fatal("换完说明也删掉")
	}
}

// 名次接力:排在前面的备用副本没过自检(接不了)就不等它;前面的一直在、本机已多等了两个名次间隔,也不再等。
func TestRankRelaySkipsBrokenStandby(t *testing.T) {
	p := newP2(t, 2, ModeConfirm)
	first, second := p.cands[0], p.cands[1]
	ri, _ := first.replicaInfo()
	ri.VerifyErr = "副本库完整性检查没过"
	if err := first.saveReplicaInfo(ri); err != nil {
		t.Fatal(err)
	}
	for _, clk := range p.clocks {
		clk.add(7 * time.Minute)
	}
	second.eng.tick()
	if second.eng.promptView() == nil {
		t.Fatalf("排在前面的备用副本坏了:备用 2 到时间就发确认: %+v", second.eng.evidenceView())
	}
	// 前面的备用好好的,但已经多等了两个名次间隔
	q := newP2(t, 2, ModeConfirm)
	for _, clk := range q.clocks {
		clk.add(4 * time.Minute)
	}
	q.cands[1].eng.tick()
	if ev := q.cands[1].eng.evidenceView(); ev == nil || ev.Reason != "betterStandby" {
		t.Fatalf("前提:先让备用 1: %+v", ev)
	}
	for _, clk := range q.clocks {
		clk.add(9 * time.Minute) // 13 分钟 ≥ 3 + 3 + 2×3
	}
	q.cands[1].eng.tick()
	if q.cands[1].eng.promptView() == nil {
		t.Fatalf("多等了两个名次间隔:不再等备用 1: %+v", q.cands[1].eng.evidenceView())
	}
}

// 面板 / Telegram 的接任只认新鲜的"等确认"判定;证据变了(同伴又联系上主机)确认作废;自动接任模式不给手动接任。
func TestTakeoverVerdictFollowsEvidence(t *testing.T) {
	p := newP2(t, 2, ModeConfirm)
	c, peer := p.cands[0], p.cands[1]
	p.clocks[0].add(4 * time.Minute)
	if ok, why := c.takeoverVerdict(); ok || why != "noPrompt" {
		t.Fatalf("判定还没走到等确认: %v %s", ok, why)
	}
	// 别的副机还联系得上主机:不接任
	c.eng.tick()
	if ok, why := c.takeoverVerdict(); ok || why != "peerSeesMaster" {
		t.Fatalf("同伴还联系得上主机: %v %s", ok, why)
	}
	if err := c.Takeover(TakeoverOpts{By: "panel"}); err == nil {
		t.Fatal("面板上点也不能接任")
	}
	// 主机真的不在了:等确认 → 能接任;过了新鲜期不能,再判一轮又能
	p.clocks[1].add(4 * time.Minute)
	p.clocks[0].add(time.Minute)
	c.eng.tick()
	if ok, why := c.takeoverVerdict(); !ok {
		t.Fatalf("等确认时能接任: %s", why)
	}
	p.clocks[0].add(promptFresh + time.Second)
	if ok, _ := c.takeoverVerdict(); ok {
		t.Fatal("判定不新鲜了:不能接任")
	}
	c.eng.tick()
	if ok, _ := c.takeoverVerdict(); !ok {
		t.Fatal("再判一轮又新鲜了")
	}
	// 同伴又联系上主机:确认作废
	peer.touch()
	p.clocks[0].add(time.Minute)
	c.eng.tick()
	if c.eng.promptView() != nil {
		t.Fatal("证据变了:之前的确认作废")
	}
	if ok, why := c.takeoverVerdict(); ok || why != "peerSeesMaster" {
		t.Fatalf("确认作废后不能接任: %v %s", ok, why)
	}
	// 自动接任模式不给手动接任
	q := newP2(t, 1, ModeAuto)
	q.clocks[0].add(7 * time.Minute)
	if ok, why := q.cands[0].takeoverVerdict(); ok || why != "auto" {
		t.Fatalf("自动接任模式: %v %s", ok, why)
	}
}

// ---- 纪元单调:还原 / 重置 / 换机还原(设计 2.5) ----

// 还原了旧备份、或重置了设置(库里的纪元回退了):本机记录里那个纪元的主机就是本机 → 抬回去并以更高纪元围栏。
func TestBootFloorRestoresEpoch(t *testing.T) {
	s, _, _, _, _ := masterWith(t, 3)
	if r := s.Boot(false); r.Fence {
		t.Fatalf("正常启动不围栏: %+v", r)
	}
	if rec := readEpochRecord(s.Dir()); rec.Epoch != 3 || rec.Master != s.SelfId() {
		t.Fatalf("记下本机的纪元: %+v", rec)
	}
	for _, back := range []string{"1", ""} { // 还原了纪元 1 的备份;菜单「重置全部设置」清掉了纪元
		_ = s.set(map[string]string{KeyEpoch: back})
		r := s.Boot(back != "")
		if !r.Fence || r.Reason != "rolledBack" || r.From != 3 || r.To != 4 || s.Epoch() != 3 {
			t.Fatalf("库纪元 %q:抬回 3、围栏到 4: %+v epoch=%d", back, r, s.Epoch())
		}
		if p, _ := strconv.ParseInt(s.setting(KeyPauseUntil), 10, 64); p <= s.now().Unix() {
			t.Fatal("围栏前暂停接任")
		}
		s.takeFence()
	}
}

// 这份主机库上次在另一台机器上运行:以更高纪元围栏(换机还原);实例号改成本机的。
func TestBootFenceOnForeignInstance(t *testing.T) {
	s, _, _, _, _ := masterWith(t, 2)
	_ = s.set(map[string]string{KeyInstance: "another-machine-instance"})
	r := s.Boot(true)
	if !r.Fence || r.Reason != "restoredElsewhere" || r.From != 2 || r.To != 3 {
		t.Fatalf("换机还原要围栏: %+v", r)
	}
	if s.setting(KeyInstance) != s.Instance() || s.Instance() == "" {
		t.Fatal("实例号改成本机的")
	}
	// 没开过容灾、也没有实例号的旧库:无从判断,不围栏
	s2 := testService(t, false, map[string]string{})
	if r := s2.Boot(true); r.Fence {
		t.Fatalf("容灾以前的旧库不围栏: %+v", r)
	}
}

// 本机当时是跟随者,却还原出一份更旧的主机库:不抬(让它按多数副机自然变为副机);副机库纪元低于记录:抬回记录值与主机。
func TestBootFollowerRestoreNotRaised(t *testing.T) {
	s, _, _, _, _ := masterWith(t, 3)
	if err := writeEpochRecord(s.Dir(), epochRecord{Epoch: 5, Master: 77}); err != nil {
		t.Fatal(err)
	}
	if r := s.Boot(true); r.Fence || s.Epoch() != 3 {
		t.Fatalf("跟随者还原出的主机库不抬: %+v epoch=%d", r, s.Epoch())
	}
	n := testService(t, true, map[string]string{KeyEpoch: "3", KeyMasterId: "4", KeyMasterPub: "x"})
	if err := writeEpochRecord(n.Dir(), epochRecord{Epoch: 5, Master: 7}); err != nil {
		t.Fatal(err)
	}
	n.Boot(true)
	if n.Epoch() != 5 || setting(n.d.DB, KeyMasterId) != "7" || setting(n.d.DB, KeyMasterPub) != "" {
		t.Fatalf("副机库抬回记录的纪元与主机(公钥等第一份状态再钉): epoch=%d", n.Epoch())
	}
}

// 没开过容灾(纪元 0)的主机也认得出副机已经跟了别的主机:按同样的判定变为副机;纪元 0 的正常回话算认本机。
func TestEpochZeroMasterHandlesStale(t *testing.T) {
	s, ids, _, _, restarted := masterWith(t, 0, "A", "B", "S")
	s.NoteReply(model.Node{Id: ids[0]}, hub.EpochReply{}) // 纪元 0 的副机正常回话(不带纪元头)
	s.NoteReply(model.Node{Id: ids[1]}, staleReply(2, ids[2], 2))
	if s.IsNode() {
		t.Fatal("纪元 0 时正常回话的副机算认本机:一台 stale 不够")
	}
	s.NoteReply(model.Node{Id: ids[2]}, rivalReply(2, ids[2], 50))
	if !s.IsNode() || *restarted != 1 {
		t.Fatal("过半副机跟着活着的新主机:纪元 0 的原主机也变为副机")
	}
}

// 退出集群(主机删除了本机 / 换令牌 / m-ui ha reset):纪元、认的主机、钉的公钥、票、交接标记、集群密钥、副本与备用密钥都清掉,纪元记录归零。
func TestLeaveAndResetLocal(t *testing.T) {
	n, _ := nodeAt(t, 3, 1)
	_ = n.set(map[string]string{KeyMasterPub: "p", KeyState: "{}", KeyVote: "{}", KeyGrant: "{}", KeyNotify: "x", KeyPeerKey: "k", KeyRole: RoleDemoted})
	if _, err := n.standbyKey(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{fileCur, filePrev, fileInfo} {
		_ = os.WriteFile(filepath.Join(n.Dir(), f), []byte("x"), 0o600)
	}
	_ = writeEpochRecord(n.Dir(), epochRecord{Epoch: 3, Master: 1})
	if err := n.LeaveCluster(); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{KeyEpoch, KeyMasterId, KeyMasterPub, KeyState, KeyVote, KeyGrant, KeyNotify, KeyPeerKey, KeyRole} {
		if setting(n.d.DB, k) != "" {
			t.Errorf("%s 要清掉", k)
		}
	}
	for _, f := range []string{fileCur, filePrev, fileInfo, fileKey} {
		if _, err := os.Stat(filepath.Join(n.Dir(), f)); err == nil {
			t.Errorf("%s 要删掉", f)
		}
	}
	if rec := readEpochRecord(n.Dir()); rec.Epoch != 0 {
		t.Fatalf("纪元记录归零: %+v", rec)
	}
	if r := n.Boot(false); r.Fence || n.Epoch() != 0 {
		t.Fatal("退出后重启不会再被抬回去")
	}
	// 退出后谁来都当没进过纪元:新主机能把它加进来
	if rej := n.CheckAgent(hdr(1, 9), "ha/state"); rej != nil || n.Epoch() != 1 {
		t.Fatalf("退出集群后能被别的主机重新加入: %+v", rej)
	}
}

// ---- 原主机回来(设计 2.7) ----

// 原主机回来的判定要求回话来自认了本机纪元的 m-ui(X-HA-Node 等于本机纪元);随便回个 200 的不算;明文地址只提示。
func TestFormerTickNeedsEpochAck(t *testing.T) {
	var mu sync.Mutex
	header := ""
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := header
		mu.Unlock()
		if h != "" {
			w.Header().Set(HeaderNode, h)
		}
		w.Write([]byte(`{"ok":"1"}`))
	})
	tls := httptest.NewTLSServer(handler)
	defer tls.Close()
	plain := httptest.NewServer(handler)
	defer plain.Close()
	for _, c := range []struct {
		url    string
		header string
		want   bool
	}{{tls.URL, "", false}, {tls.URL, "1", false}, {tls.URL, "2", true}, {plain.URL, "2", false}} {
		m, _, notes, _, _ := masterWith(t, 2)
		testHub(t, m)
		row := model.Node{Name: "OLD", ApiUrl: c.url + "/app/", Token: "t", Insecure: true}
		m.d.DB.Create(&row)
		m.d.DB.Model(&model.Node{}).Where("id = ?", row.Id).Update("enabled", false) // Create 会把 false 换成默认的 true
		_ = m.set(map[string]string{KeyFormer: mustJSON(Former{Id: row.Id, Name: "OLD"})})
		mu.Lock()
		header = c.header
		mu.Unlock()
		m.formerTick()
		var got model.Node
		m.d.DB.First(&got, row.Id)
		if got.Enabled != c.want {
			t.Fatalf("%s 头=%q:启用=%v,期望 %v", c.url, c.header, got.Enabled, c.want)
		}
		if strings.HasPrefix(c.url, "http://") && c.header == "2" && countNotes(notes, "明文 http") != 1 {
			t.Fatalf("明文地址要提示管理员手动启用: %q", *notes)
		}
	}
}

// 推给还没确认的原主机的状态不带集群密钥、通知块。
func TestFormerStateHasNoClusterKey(t *testing.T) {
	m, ids, _, _, _ := masterWith(t, 2, "A")
	_ = m.set(map[string]string{KeyPubPrefix + strconv.FormatUint(uint64(ids[0]), 10): ""})
	body, rev := m.stateFor(model.Node{Id: ids[0]}, false)
	var ss SignedState
	if rev == "" || json.Unmarshal(body, &ss) != nil {
		t.Fatal("要有状态")
	}
	var st State
	_ = json.Unmarshal(ss.Body, &st)
	if st.ClusterKey != "" || st.Notify != "" {
		t.Fatalf("不带集群密钥与通知块: %+v", st)
	}
	body, _ = m.stateFor(model.Node{Id: ids[0]}, true)
	_ = json.Unmarshal(body, &ss)
	_ = json.Unmarshal(ss.Body, &st)
	if st.ClusterKey == "" {
		t.Fatal("正常的副机要有集群密钥")
	}
}

// ---- 让出、FenceAbandon、名单宽限(设计 2.4 / 2.8) ----

// 让出时"失联期间多收的改动"按本机当上主机时的数据算(不把整张审计表算进去),原因写 yielded。
func TestYieldCountsOwnChanges(t *testing.T) {
	s, ids, notes, clk, _ := masterWith(t, 2, "B", "C", "D")
	for i := 0; i < 10; i++ {
		s.d.DB.Create(&model.Change{DateTime: time.Now().Unix(), Actor: "a", Key: "k", Action: "x"})
	}
	var max uint64
	s.d.DB.Raw("SELECT MAX(id) FROM changes").Scan(&max)
	_ = s.set(map[string]string{KeyBecame: mustJSON(Became{How: HowTakeover, Epoch: 2, At: 1, MaxChangeId: max - 3})})
	following := hub.EpochReply{Aware: true, Code: CodeFollowing, Epoch: 1, MasterId: 9, Role: "node"}
	for i := 0; i < 8; i++ {
		s.NoteReply(model.Node{Id: ids[0]}, okReply)
		s.NoteReply(model.Node{Id: ids[1]}, following)
		s.NoteReply(model.Node{Id: ids[2]}, following)
		clk.add(30 * time.Second)
	}
	var d Demoted
	if !s.IsNode() || !s.getJSON(KeyDemoted, &d) || d.Reason != "yielded" || !d.Yield || d.Divergent != 3 {
		t.Fatalf("让出:改动只算本机当上主机之后的: %+v", d)
	}
	if countNotes(notes, "让出主机") != 1 {
		t.Fatalf("通知说是让出: %q", *notes)
	}
	if n := s.Notices(); len(n) != 1 || n[0].Reason != "yielded" || !n[0].Yield {
		t.Fatalf("提示带上原因: %+v", n)
	}
}

// 设为主机结果不明、放弃时:撤销给目标的交接标记,以高于目标的纪元继续当主机;认本机的副机凭自交接标记跟上。
func TestFenceAbandonRaisesAboveTarget(t *testing.T) {
	cl := newCluster(t, 2)
	m := cl.m
	cl.h.RunOnce()
	cl.h.RunOnce()
	next, err := m.FenceAbandon(cl.ids[0], 2)
	if err != nil || next != 3 || m.Epoch() != 3 {
		t.Fatalf("以高于目标(纪元 2)的纪元继续当主机: %d %v", next, err)
	}
	cl.h.RunOnce()
	for i, c := range cl.cands {
		if c.Epoch() != 3 || setting(c.d.DB, KeyMasterId) != strconv.FormatUint(uint64(m.SelfId()), 10) {
			t.Fatalf("副机 %d 要跟到纪元 3: %d", i, c.Epoch())
		}
	}
	// 目标之后以纪元 2 起来:副机按纪元拒绝它
	if rej := cl.cands[1].CheckAgent(hdr(2, cl.ids[0]), "ha/state"); rej == nil || rej.Code != CodeStale {
		t.Fatalf("目标以旧纪元起来会被拒: %+v", rej)
	}
}

// 备用名单整个是空的(主机自己断网刚恢复,还没拉到报告):不马上删副本,连续 10 分钟才删;主机关了容灾立即删。
func TestEmptyStandbyListGrace(t *testing.T) {
	cl := newCluster(t, 1)
	m, c := cl.m, cl.cands[0]
	cl.h.RunOnce()
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	cl.h.RunOnce()
	st, _ := c.NodeState()
	st.Standbys = []uint{}
	clk := &testClock{t: time.Now()}
	c.SetNow(clk.now)
	c.checkListed(st, clk.now())
	if _, ok := c.replicaInfo(); !ok {
		t.Fatal("空名单:先不删")
	}
	clk.add(standbyDropGrace + time.Second)
	c.checkListed(st, clk.now())
	if _, ok := c.replicaInfo(); ok {
		t.Fatal("空名单连续 10 分钟:删")
	}
}

// Telegram 上有别的程序在收这个机器人的消息(409,多半是主机的机器人还活着):第一次就停止轮询,不和它抢消息,告警一次;
// 面板上照样能确认。
func TestTelegramConflictStopsPolling(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	var sent []string
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var b struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			sent = append(sent, b.Text)
			mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			mu.Lock()
			polls++
			mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer tg.Close()
	p := newP2(t, 1, ModeConfirm)
	p.m.set(map[string]string{"tgEnabled": "true", "tgToken": "T", "tgChatId": "42", "tgApiBase": tg.URL})
	p.m.invalidate()
	p.h.RunOnce()
	c := p.cands[0]
	p.clocks[0].add(4 * time.Minute)
	c.eng.tick()
	if c.eng.promptView() == nil {
		t.Fatal("前提:在等确认")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n, said := polls, 0
		for _, s := range sent {
			if strings.Contains(s, "另一个程序正在用这个机器人") {
				said++
			}
		}
		mu.Unlock()
		if n > 0 && said > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	n := polls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("第一次 409 就停止轮询:实际查了 %d 次", n)
	}
	if ev := c.eng.evidenceView(); ev == nil || !ev.TGConflict {
		t.Fatalf("证据里记下主机的机器人可能还活着: %+v", ev)
	}
	if ok, why := c.takeoverVerdict(); !ok {
		t.Fatalf("面板上照样能确认: %s", why)
	}
}
