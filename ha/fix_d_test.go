package ha

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// ---- 核验员第二轮的修复 ----

// 只拿到副机令牌的人:读出 409 里的纪元与主机、带上对的请求头发 ha/leave / ha/drop,副机也不清纪元、不删副本 ——
// 正文要当前主机用钉住的那把签名密钥签过名,动作、纪元、主机、目标副机、时间都要对得上。
func TestCtlNeedsMasterSignature(t *testing.T) {
	cl := readyCluster(t, 1)
	m, c := cl.m, cl.cands[0]
	L := c.Epoch()
	if L == 0 || c.setting(KeyMasterPub) == "" {
		t.Fatal("前提:副机已进纪元并钉住主机公钥")
	}
	row := nodeByName(t, m.d.DB, "TW1")
	kept := func(what string) {
		t.Helper()
		if c.Epoch() != L || c.setting(KeyMasterPub) == "" {
			t.Fatalf("%s:不能清掉纪元与钉住的公钥 (epoch=%d)", what, c.Epoch())
		}
	}
	if err := c.LeaveSigned(nil); err == nil {
		t.Fatal("没有正文:拒")
	}
	kept("没签名")
	if err := c.LeaveSigned([]byte(`{"v":1,"body":{"action":"ha/leave"},"sig":"AAAA"}`)); err == nil {
		t.Fatal("签名不对:拒")
	}
	kept("签名不对")
	// 拿自己的密钥签一份字段全对的
	evil := newSignKey(t)
	body, _ := json.Marshal(ctlBody{Action: "ha/leave", Node: row.Id, Epoch: L, Master: m.SelfId(), Ts: time.Now().Unix()})
	sig := ed25519.Sign(evil, append([]byte(ctlSigContext), body...))
	forged, _ := json.Marshal(signedCtl{V: ctlV, Body: body, Sig: base64.StdEncoding.EncodeToString(sig)})
	if err := c.LeaveSigned(forged); err == nil {
		t.Fatal("不是钉住的主机签的:拒")
	}
	kept("别人签的")
	// 主机签的删副本请求挪来当离开集群:动作对不上
	drop, err := m.ctlRequest("ha/drop", row)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.LeaveSigned(drop); err == nil {
		t.Fatal("动作对不上:拒")
	}
	kept("动作挪用")
	// 发给别的副机的
	other := row
	other.Id = row.Id + 100
	if b, _ := m.ctlRequest("ha/leave", other); c.LeaveSigned(b) == nil {
		t.Fatal("发给别的副机的:拒")
	}
	kept("别的副机")
	// 过期的(时间超出允许误差)
	cl.clk.add(-ctlSkew - time.Minute)
	old, _ := m.ctlRequest("ha/leave", row)
	cl.clk.add(ctlSkew + time.Minute)
	if err := c.LeaveSigned(old); err == nil {
		t.Fatal("过期的:拒")
	}
	kept("过期")
	// 当前主机签的:删副本、离开集群照常
	if err := m.sendCtl(row, "ha/drop", 10*time.Second); err != nil {
		t.Fatalf("主机签的删副本: %v", err)
	}
	if err := m.sendCtl(row, "ha/leave", 10*time.Second); err != nil {
		t.Fatalf("主机签的离开集群: %v", err)
	}
	if c.Epoch() != 0 || c.setting(KeyMasterPub) != "" {
		t.Fatal("当前主机签的离开集群:纪元清零")
	}
	// 没钉住公钥的副机:一律拒(不知道该信谁)
	if err := c.DropSigned(drop); err == nil || !strings.Contains(err.Error(), errCtlNoPin.Error()) {
		t.Fatalf("没钉住公钥时拒: %v", err)
	}
}

// 提交没回话(其实生效了),目标与原主机之间断了;别的副机已经凭交接标记跟了目标。冻结截止时不能直接隔离:
// 先当场问、再看副机的回复 —— 有副机正被目标以新纪元推着,就按换主机完成收尾(变为副机),不升纪元、不解冻收写。
func TestFenceSeesNodesFollowingTarget(t *testing.T) {
	shortCommitWait(t)
	cl := readyCluster(t, 2)
	m, other := cl.m, cl.cands[1]
	m.d.Restart = func(time.Duration) {}
	cl.cands[0].d.Restart = func(time.Duration) {}
	L := m.Epoch()
	var mu sync.Mutex
	committed := false
	wrapAgent(t, cl, 0, agentHook{
		before: func(action string) {
			mu.Lock()
			defer mu.Unlock()
			if committed {
				cl.agents[0].setDown(true) // 提交以后原主机再也连不上目标
			}
		},
		lose: func(action string) bool {
			if action != "ha/commit" {
				return false
			}
			mu.Lock()
			committed = true
			mu.Unlock()
			return true
		},
	})
	cl.h.RunOnce()
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitchDone(t, m)
	if st.Stage != StageUnknown {
		t.Fatalf("前提:提交结果不明: %+v", st)
	}
	// 目标以 E+1 起来,别的副机凭交接标记跟了它(正被它推着)
	if err := other.adopt(st.Epoch, cl.ids[0], ""); err != nil {
		t.Fatal(err)
	}
	cl.clk.add(time.Duration(st.FreezeUntil-cl.clk.now().Unix()+1) * time.Second)
	m.sw.tick()
	got := m.Switch()
	if m.Epoch() == L+2 || got.Stage == StageFenced {
		t.Fatalf("有副机已跟目标:不能以 E+2 隔离、解冻: %+v epoch=%d", got, m.Epoch())
	}
	if !m.IsNode() || got.Stage != StageDone {
		t.Fatalf("按换主机完成收尾,本机变为副机: %+v", got)
	}
}

// 撤销要有足够的副机确认才隔离、解冻:截止前放弃要目标以外的副机全部确认;截止后要过半。不够就保持冻结,说明原因,之后自动再试。
func TestFenceNeedsAcks(t *testing.T) {
	shortCommitWait(t)
	cl := readyCluster(t, 2)
	m := cl.m
	L := m.Epoch()
	cl.agents[0].mu.Lock()
	cl.agents[0].dieAfterHandover = true
	cl.agents[0].mu.Unlock()
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	st := waitSwitchDone(t, m)
	if st.Stage != StageUnknown {
		t.Fatalf("前提:结果不明: %+v", st)
	}
	cl.agents[1].setDown(true) // 另一台副机连不上:它手里的交接标记撤不掉
	if err := m.AbandonSwitch("admin"); err == nil {
		t.Fatal("截止前放弃:还有副机没收到撤销,不能隔离解冻")
	}
	if got := m.Switch(); got.Stage != StageUnknown || m.Epoch() != L || !m.Frozen() || !strings.Contains(got.Err, "确认收到撤销") {
		t.Fatalf("保持冻结、说明原因: %+v epoch=%d", got, m.Epoch())
	}
	cl.clk.add(time.Duration(st.FreezeUntil-cl.clk.now().Unix()+1) * time.Second)
	m.sw.mu.Lock()
	m.sw.lastFence = time.Time{}
	m.sw.mu.Unlock()
	m.sw.tick()
	if got := m.Switch(); got.Stage != StageUnknown || m.Epoch() != L || !m.Frozen() {
		t.Fatalf("截止后目标以外的副机也凑不够过半:仍不隔离: %+v", got)
	}
	if _, ok := m.BeginWrite(); ok {
		t.Fatal("没隔离之前写操作保持暂停")
	}
	if n := countNotes(cl.notes, "暂不隔离"); n != 1 {
		t.Fatalf("暂不隔离只通知一次: %d", n)
	}
	cl.agents[1].setDown(false)
	m.sw.mu.Lock()
	m.sw.lastFence = time.Time{}
	m.sw.mu.Unlock()
	m.sw.tick()
	if got := m.Switch(); got.Stage != StageFenced || m.Epoch() != L+2 || m.Frozen() {
		t.Fatalf("副机回来确认了撤销:隔离、解冻: %+v epoch=%d", got, m.Epoch())
	}
}

// 提交请求发出之后、结论出来之前写闸一直关着:冻结时间到了也不自己打开(当场追问可能越过截止;重启时也一样)。
func TestCommitKeepsWriteGateClosed(t *testing.T) {
	shortCommitWait(t)
	cl := readyCluster(t, 1)
	m := cl.m
	m.d.Restart = func(time.Duration) {}
	cl.cands[0].d.Restart = func(time.Duration) {}
	var mu sync.Mutex
	var gate, deadline int64
	wrapAgent(t, cl, 0, agentHook{before: func(action string) {
		if action == "ha/commit" {
			mu.Lock()
			gate, deadline = m.freezeUntil(), m.switchState().FreezeUntil
			mu.Unlock()
		}
	}})
	cl.h.RunOnce()
	if _, err := m.SetMaster(cl.ids[0], "admin", SetMasterOpts{}); err != nil {
		t.Fatal(err)
	}
	waitSwitchDone(t, m)
	mu.Lock()
	defer mu.Unlock()
	if deadline == 0 || gate < deadline+int64(holdAfterDeadline/time.Second) {
		t.Fatalf("发提交请求前写闸要延长到截止之后: gate=%d deadline=%d", gate, deadline)
	}
	// 进程在提交途中停下、重启时冻结已经过了:写闸照样关着,直到有结论
	s, _, _, clk, _ := masterWith(t, 3, "TW")
	now := clk.now().Unix()
	_ = s.set(map[string]string{KeySwitch: mustJSON(SwitchState{Id: "x", Target: 2, Stage: StageCommit, FreezeUntil: now - 5}), KeyFreezeUntil: strconv.FormatInt(now-5, 10)})
	if !s.Frozen() {
		t.Fatal("提交阶段写闸关着")
	}
	if _, ok := s.BeginWrite(); ok {
		t.Fatal("提交阶段不收写")
	}
	_ = s.set(map[string]string{KeySwitch: mustJSON(SwitchState{Id: "x", Target: 2, Stage: StageFailed, FreezeUntil: now - 5})})
	if s.Frozen() {
		t.Fatal("有结论之后解冻")
	}
}

// 非让出的变为副机(被更高纪元接管、同纪元输了)也要把本任期改过、仍指向本机的 DNS 改掉:指向新主机,不知道它的 IP 就改回原值。
func TestDemoteMovesDNS(t *testing.T) {
	for _, tc := range []struct {
		name, newIP, want string
	}{{"知道新主机的 IP", "192.0.2.9", "192.0.2.9"}, {"不知道新主机的 IP", "", "203.0.113.50"}} {
		t.Run(tc.name, func(t *testing.T) {
			cf := newFakeCF(t)
			s, ids, notes, clk, _ := masterWith(t, 2, "TW")
			s.SetDNSAPI(cf.srv.URL)
			s.d.DB.Model(&model.Node{}).Where("id = ?", ids[0]).Update("public_ip", tc.newIP)
			b := Became{How: HowTakeover, Epoch: 2, Dns: []DnsChange{{Name: "sub.example.com", Type: "A", From: "203.0.113.50", To: "198.51.100.1"}}}
			_ = s.set(map[string]string{KeyDnsToken: "tok", KeyBecame: mustJSON(b)})
			if err := s.Demote(Demoted{Epoch: 3, MasterId: ids[0], OldEpoch: 2, At: clk.now().Unix(), Reason: "takenOver"}); err != nil {
				t.Fatal(err)
			}
			cf.mu.Lock()
			r1, r2 := cf.records["r1"]["content"], cf.records["r2"]["content"]
			cf.mu.Unlock()
			if r1 != tc.want || r2 != "198.51.100.99" {
				t.Fatalf("仍指向本机的记录要改掉,别的不动: r1=%v r2=%v", r1, r2)
			}
			var d Demoted
			if !s.getJSON(KeyDemoted, &d) || len(d.Dns) != 1 || d.Dns[0].From != "198.51.100.1" || d.Dns[0].To != tc.want {
				t.Fatalf("变为副机的记录里写清改了什么: %+v", d)
			}
			if countNotes(notes, "198.51.100.1 → "+tc.want) != 1 {
				t.Fatalf("通知里写原值 → 新值: %v", *notes)
			}
		})
	}
	// 新主机是本机的另一个实例(换机还原):服务器表里"它"的 IP 就是本机的,不动
	cf := newFakeCF(t)
	s, _, _, clk, _ := masterWith(t, 2, "TW")
	s.SetDNSAPI(cf.srv.URL)
	b := Became{How: HowTakeover, Epoch: 2, Dns: []DnsChange{{Name: "sub.example.com", Type: "A", From: "203.0.113.50", To: "198.51.100.1"}}}
	_ = s.set(map[string]string{KeyDnsToken: "tok", KeyBecame: mustJSON(b)})
	_ = s.Demote(Demoted{Epoch: 3, MasterId: s.SelfId(), OldEpoch: 2, At: clk.now().Unix(), Reason: "restoredElsewhere"})
	cf.mu.Lock()
	defer cf.mu.Unlock()
	if cf.records["r1"]["content"] != "198.51.100.1" {
		t.Fatal("换机还原时不动 DNS")
	}
}

// 副机库的纪元被回退、启动时抬回:钉住的主机公钥一并恢复成记录里的,不清空(清空了谁抢先推一份自己签的状态就钉谁)。
func TestBootRestoresPinnedPub(t *testing.T) {
	n, _ := nodeAt(t, 3, 1)
	n.Boot(false)
	k, evil := newSignKey(t), newSignKey(t)
	st := State{Epoch: 3, MasterId: 1, MasterPub: PubOf(k), Self: 20, Mode: ModeConfirm, Standbys: []uint{}}
	if err := n.ReceiveState(hdr(3, 1), signed(k, st)); err != nil {
		t.Fatal(err)
	}
	if rec := readEpochRecord(n.Dir()); rec.Epoch != 3 || rec.Pub != PubOf(k) {
		t.Fatalf("钉住的公钥记进纪元记录: %+v", rec)
	}
	// 还原了一份旧库:纪元 2、没钉公钥
	_ = n.set(map[string]string{KeyEpoch: "2", KeyMasterPub: "", KeyMasterId: "1"})
	n.Boot(false)
	if n.Epoch() != 3 || n.setting(KeyMasterPub) != PubOf(k) {
		t.Fatalf("抬回纪元时一并恢复钉住的公钥: epoch=%d pub=%q", n.Epoch(), n.setting(KeyMasterPub))
	}
	forged := st
	forged.MasterPub = PubOf(evil)
	if err := n.ReceiveState(hdr(3, 1), signed(evil, forged)); err == nil {
		t.Fatal("别人签的状态不能趁机钉住")
	}
	// 换了纪元:记录里的公钥跟着作废,等新纪元第一份状态
	if err := n.adopt(4, 1, ""); err != nil {
		t.Fatal(err)
	}
	if rec := readEpochRecord(n.Dir()); rec.Epoch != 4 || rec.Pub != "" {
		t.Fatalf("新纪元的记录不带旧公钥: %+v", rec)
	}
}
